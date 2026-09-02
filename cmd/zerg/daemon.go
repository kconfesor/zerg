package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kconfesor/zerg/internal/store"
)

// Running the daemon in the background.
//
// ARCHITECTURE §7.4 records this as the one job tmux was doing that zerg had no
// answer for: `zerg up` runs in the foreground, so closing the terminal ends
// the daemon and every agent under it. The advice until now was a launchd or
// systemd unit, or nohup, which is a reasonable answer for a server and a poor
// one for the machine somebody is actually working on.
//
// What is here is the smallest thing that removes the terminal from the
// picture: re-exec into a session of the daemon's own, point its output at a
// file, and record where it went. It is deliberately not a service manager. It
// does not restart the daemon, and it has no opinion about boot; those are what
// launchd and systemd are for, and reimplementing either badly would be worse
// than the units the README already suggests.

// pidFileName is where a running daemon records itself, beside the database
// rather than in a fixed system path: the database is what identifies an
// installation, and two daemons on two databases are a supported thing to be
// doing.
const pidFileName = "zerg.pid"

// logFileName is where a detached daemon's output goes. Appended, never
// rotated: a daemon writing megabytes of logs is a thing worth noticing rather
// than a thing to quietly absorb, and rotation is a job for the tool that
// already does it on this machine.
const logFileName = "zerg.log"

// resolveDBPath fills in the default and makes the result absolute.
//
// Absolute for the reason runUp already resolves it: everything else is derived
// from this path, and a relative one resolves differently in the daemon's
// working directory than in an agent's worktree. The pid and log files sit
// beside it, so they inherit the same guarantee.
func resolveDBPath(path string) (string, error) {
	if path == "" {
		p, err := store.DefaultPath()
		if err != nil {
			return "", err
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}
	return abs, nil
}

// pidPath and logPath are derived from the database path, which is resolved to
// an absolute path before anything asks for either.
func pidPath(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), pidFileName) }
func logPath(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), logFileName) }

// The pid file is a lock, not a note.
//
// It used to be a note, and "is there a daemon" was answered by asking whether
// the pid in it still existed. Signal 0 cannot tell a reused pid from the
// original, and pids are reused: reproduced by putting a live `sleep`'s pid in
// zerg.pid, after which `zerg down` sent SIGTERM to it, killed it, and reported
// that zerg had stopped. The same probe made exclusivity a check followed by a
// write, so two `zerg up`s could both see nothing running and both take the
// file, leaving two daemons on one database.
//
// A daemon now holds an exclusive flock on the file for its whole life. The
// kernel answers both questions and can be wrong about neither: the lock is
// held only while the process that took it is alive, and taking it is atomic,
// so there is no window between deciding and acting. The pid inside is then
// only ever read from a file somebody is provably holding, which is what makes
// it safe to signal.

// pidLock is a held pid file. Releasing it is what tells the next daemon it may
// start.
type pidLock struct {
	f    *os.File
	path string
}

// lockPidFile takes the daemon lock for this database and records this process
// in it, or reports who has it.
//
// Taken by the daemon itself rather than by whatever started it: the lock has
// to be released by the process ending, and only that process's own file
// descriptor does that. A detached start therefore waits to be told the daemon
// is up rather than writing the file on its behalf, and a foreground start gets
// the same exclusivity and the same `zerg status` for free.
func lockPidFile(dbPath string) (*pidLock, error) {
	path := pidPath(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	// A few attempts, for the one race a lock file has that a lock does not:
	// the file can be unlinked by the daemon releasing it between our open and
	// our flock, leaving us holding an exclusive lock on an inode that is no
	// longer at the path — after which the next daemon creates a new file,
	// locks that, and both run. Locking and then checking that the thing we
	// locked is still the thing at the path closes it.
	for attempt := 0; attempt < 5; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			pid, _ := pidIn(path)
			who := "another zerg daemon"
			if pid > 0 {
				who = fmt.Sprintf("zerg (pid %d)", pid)
			}
			// The file is named for orientation and not as a remedy. Deleting
			// it used to be the advice, because the file was the claim; it is
			// now a lock the kernel holds, so removing it changes nothing and
			// `zerg down` is the only way through.
			return nil, fmt.Errorf("%s is already running for this database.\n"+
				"Stop it with `zerg down`; it holds the lock on %s", who, path)
		}
		if !sameFile(f, path) {
			// Somebody released the lock and unlinked the file underneath us.
			f.Close()
			continue
		}

		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, fmt.Errorf("recording the daemon's pid in %s: %w", path, err)
		}
		if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
			f.Close()
			return nil, fmt.Errorf("recording the daemon's pid in %s: %w", path, err)
		}
		// On disk before anything else happens, because the whole point of the
		// number is to be readable by a process that arrives at any moment.
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, fmt.Errorf("recording the daemon's pid in %s: %w", path, err)
		}
		return &pidLock{f: f, path: path}, nil
	}
	return nil, fmt.Errorf("could not take the daemon lock on %s: it is being created and removed repeatedly", path)
}

// release gives the lock up and takes the file with it.
//
// Unlinked before the descriptor is closed, and the order is the whole of the
// correctness: closing first would let another daemon lock this same inode in
// the gap, and the unlink would then remove the file it is holding, after which
// a third daemon creates a fresh one and two are running.
func (l *pidLock) release() {
	if l == nil {
		return
	}
	os.Remove(l.path)
	l.f.Close()
}

// sameFile reports whether an open descriptor is still the file at path.
func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// pidIn reads the number out of a pid file, saying nothing about whether it
// means anything.
func pidIn(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

// readPidFile returns the pid of the daemon running for this database, and
// whether there is one.
//
// The answer comes from the lock rather than from the pid: if the lock can be
// taken, no daemon holds it, whatever the file says and whatever process
// happens to have that number now. A file naming a process that has gone is not
// an error — it is what a killed daemon leaves behind — and it is why the pid
// is returned even when the answer is "not running", so `zerg status` can say
// the file is stale rather than pretending it was never there.
func readPidFile(dbPath string) (pid int, running bool) {
	path := pidPath(dbPath)
	pid, _ = pidIn(path)

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		// No file at all is the ordinary "nothing has ever run here".
		return pid, false
	}
	defer f.Close()

	// flock does not need write access, and asking for none means `zerg status`
	// works against a pid file whose owner this user cannot write to.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return pid, true
	}
	// Ours only for as long as it takes to learn nothing holds it. Released
	// rather than kept, and the file is left where it is: a stale file is
	// evidence for the message `zerg status` prints about it.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return pid, false
}

// detach re-execs this binary in a session of its own and returns.
//
// The child is the same command with --detach removed and the database path
// made explicit, so it cannot resolve a different default from a different
// working directory. Setsid is what makes it survive the terminal: without a
// controlling terminal there is no SIGHUP to receive when that terminal closes.
func detach(dbPath string, args []string) error {
	if pid, running := readPidFile(dbPath); running {
		return fmt.Errorf("zerg is already running for this database (pid %d).\n"+
			"Stop it with `zerg down`, or if that process is not zerg, delete %s",
			pid, pidPath(dbPath))
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this binary to run it in the background: %w", err)
	}

	out := logPath(dbPath)
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(out), err)
	}
	// Appended rather than truncated, so a restart does not throw away the log
	// of the run somebody is about to go looking for the reason for.
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening the log file %s: %w", out, err)
	}
	defer f.Close()

	// The pipe the child says it is serving on.
	//
	// Waiting for the pid file instead was wrong, and reproducibly: the file is
	// the lifetime lock and is taken first, before the database is opened, TLS
	// is resolved or anything is bound, so `zerg up --detach` printed "running"
	// and exited 0 while the daemon was still on its way to failing. Two
	// seconds later `zerg status` said stopped and the log said
	// "bind: address already in use". A pipe answers a different question --
	// not "has it started" but "is it serving" -- and it answers the failure
	// case for free, because the write end closes when the process dies.
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("preparing to wait for the daemon: %w", err)
	}
	defer readyR.Close()

	child := exec.Command(self, append([]string{"up", "--db", dbPath}, args...)...)
	child.Stdout = f
	child.Stderr = f
	// No stdin: a background daemon has nobody to read from, and leaving the
	// terminal's stdin attached is how a backgrounded process ends up stopped
	// with SIGTTIN the first time anything reads it.
	child.Stdin = nil
	child.ExtraFiles = []*os.File{readyW}
	child.Env = append(os.Environ(), fmt.Sprintf("%s=3", readyEnv))
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		readyW.Close()
		return fmt.Errorf("starting the daemon in the background: %w", err)
	}
	// The parent's copy, closed so that the child's is the only one left and
	// the read end sees EOF the moment the child dies.
	readyW.Close()

	// Reaped, so that a daemon which fails to start is *seen* to have failed.
	//
	// The obvious liveness probe is kill(pid, 0), which is what everything else
	// here uses, and on a child of this process it is wrong: an exited child
	// nobody has waited on is a zombie, and a zombie answers signal 0 exactly
	// as a running process does. Watched happening -- a daemon that died on a
	// socket it could not bind was reported, ten seconds later, as one that had
	// "not reported itself", which sends somebody looking for a hang instead of
	// reading the error two lines into the log.
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	// Wait for the daemon to say it is serving. Without this, a start that
	// fails -- a port in use, a TLS setting that cannot be satisfied -- reports
	// success to the shell and leaves the operator to discover the failure in a
	// log they have not been told about yet.
	ready, err := waitForDaemon(readyR, exited, 30*time.Second)
	if err != nil {
		return fmt.Errorf("%w\nIts output is in %s", err, out)
	}

	fmt.Printf("zerg is running in the background (pid %d)\n", ready.pid)
	for _, url := range ready.urls {
		fmt.Printf("Cockpit: %s\n", url)
	}
	fmt.Printf("Log:  %s\n", out)
	fmt.Printf("Stop: zerg down\n")
	return nil
}

// readiness is what a detached daemon says about itself once it is serving.
type readiness struct {
	pid  int
	urls []string
}

// waitForDaemon blocks until the started daemon reports itself ready, dies, or
// runs out of time.
//
// Three outcomes and they are genuinely different, which is why the pipe is
// read rather than a file polled. A line is a daemon that is bound and serving.
// EOF with nothing said is one that exited on the way there, and the exit
// status is collected to say so. The deadline is a daemon that is neither --
// wedged on a certificate or a slow disk -- and is generous for that reason:
// it is not the common case and cutting it short would report a failure that
// has not happened.
func waitForDaemon(ready *os.File, exited <-chan error, grace time.Duration) (readiness, error) {
	said := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(ready).ReadString('\n')
		said <- strings.TrimSpace(line)
	}()

	select {
	case line := <-said:
		if r, ok := parseReadiness(line); ok {
			return r, nil
		}
		// Nothing, or nothing we understand, and the pipe is closed: the
		// daemon is gone. Its exit status is worth a moment's wait, because it
		// is the difference between "exited 1" and a signal.
		var err error
		select {
		case err = <-exited:
		case <-time.After(2 * time.Second):
		}
		if err != nil {
			return readiness{}, fmt.Errorf("the daemon exited before it was serving: %w", err)
		}
		return readiness{}, errors.New("the daemon exited before it was serving")
	case <-time.After(grace):
		return readiness{}, fmt.Errorf("the daemon did not report itself serving within %s", grace)
	}
}

// parseReadiness reads the line a ready daemon writes: the keyword, its pid,
// and the addresses it is serving on.
func parseReadiness(line string) (readiness, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != readyWord {
		return readiness{}, false
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil {
		return readiness{}, false
	}
	return readiness{pid: pid, urls: fields[2:]}, true
}

// readyEnv names the descriptor a detached daemon reports itself on, and
// readyWord is the first field of what it writes there.
const (
	readyEnv  = "ZERG_READY_FD"
	readyWord = "serving"
)

// takeReadyPipe claims the readiness pipe, if this daemon was started with one.
//
// Close-on-exec, and set here rather than left to the parent, because this is
// the last moment before the daemon starts spawning things. The cockpit's dev
// server and every agent are children of this process, and an inherited write
// end would keep the pipe open after the daemon itself had died -- so a parent
// waiting for EOF on a daemon that failed would wait for the full timeout
// instead of being told immediately.
//
// The variable is removed from the environment for the same reason: a child
// that inherited it would think it had a parent to report to.
func takeReadyPipe() *os.File {
	v := os.Getenv(readyEnv)
	os.Unsetenv(readyEnv)
	if v == "" {
		return nil
	}
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return nil
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "zerg readiness pipe")
}

// signalReady tells whoever started this daemon that it is serving, and on
// what. Closed immediately: the parent is waiting for a line or for the pipe to
// end, and has no further use for it.
func signalReady(pipe *os.File, urls ...string) {
	if pipe == nil {
		return
	}
	fmt.Fprintf(pipe, "%s %d %s\n", readyWord, os.Getpid(), strings.Join(urls, " "))
	pipe.Close()
}

// runDown asks a running daemon to shut down and waits for it to.
//
// SIGTERM and nothing harder. The daemon's shutdown is not a formality: it
// stops every agent, closes their sessions, returns in-flight work to the queue
// and can be partway through a git integration when it is asked. A SIGKILL
// after an impatient timeout would abandon exactly the state this whole feature
// exists to preserve, so a daemon that will not go is reported rather than
// shot, with its pid, which is enough for somebody who has decided otherwise.
func runDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	dbPath := fs.String("db", "", "database path (default ~/.zerg/zerg.db)")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for it to stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := resolveDBPath(*dbPath)
	if err != nil {
		return err
	}

	pid, running := readPidFile(path)
	if !running {
		return fmt.Errorf("no zerg daemon is running for %s", path)
	}
	// A pid read out of a *held* lock file, which is what makes this safe to
	// send. The number alone was not: with a stale file and a reused pid this
	// signalled, killed and reported success over somebody else's process.
	if pid <= 1 {
		return fmt.Errorf("a zerg daemon holds %s but has not recorded its pid yet; try again in a moment",
			pidPath(path))
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("asking zerg (pid %d) to stop: %w", pid, err)
	}

	// Waiting for the lock to be released rather than for the pid to disappear.
	// The lock is held for the daemon's whole life and by nothing else, so its
	// release is the daemon being gone; the pid could be reused by then and
	// answer as though it were still there.
	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		if _, still := readPidFile(path); !still {
			fmt.Printf("zerg stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("zerg (pid %d) is still shutting down after %s.\n"+
		"It stops its agents and finishes any integration in progress first; give it longer, "+
		"or run `kill -9 %d` if you are sure", pid, *wait, pid)
}

// runStatus says whether a daemon is running for this database, and where to
// look if it is.
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dbPath := fs.String("db", "", "database path (default ~/.zerg/zerg.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := resolveDBPath(*dbPath)
	if err != nil {
		return err
	}

	pid, running := readPidFile(path)
	if !running {
		if pid != 0 {
			// Worth saying rather than reporting a plain "not running": a file
			// naming a pid is what a killed daemon leaves behind. Not "which is
			// gone", which was the old wording and is a claim this cannot make
			// — the number may well name a live process, just not a daemon, and
			// that is exactly the case that used to get it signalled.
			fmt.Printf("zerg is not running for %s\n", path)
			fmt.Printf("(%s is left over from a daemon that was killed; it names pid %d and nothing holds it)\n",
				pidPath(path), pid)
			return nil
		}
		fmt.Printf("zerg is not running for %s\n", path)
		return nil
	}
	fmt.Printf("zerg is running (pid %d)\n", pid)
	fmt.Printf("DB:  %s\n", path)
	if _, err := os.Stat(logPath(path)); err == nil {
		fmt.Printf("Log: %s\n", logPath(path))
	}
	return nil
}

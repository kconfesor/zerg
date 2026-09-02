package main

import (
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

// alive reports whether a process exists.
//
// Signal 0 delivers nothing and tests for existence, which is the same probe
// internal/runner uses to decide whether a server it asked to stop actually
// stopped. It cannot tell a reused pid from the original, which is why every
// message built on it names the file, so somebody looking at a wrong answer can
// see where it came from and delete it.
func alive(pid int) bool {
	if pid <= 1 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// readPidFile returns the pid recorded for this database and whether that
// process is still there. A file naming a process that has gone is not an
// error: it is what a killed daemon leaves behind.
func readPidFile(dbPath string) (pid int, running bool) {
	raw, err := os.ReadFile(pidPath(dbPath))
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return pid, alive(pid)
}

// writePidFile records this process as the daemon for this database, refusing
// when another one is already running.
//
// Written by the daemon itself rather than by whatever started it, so there is
// one owner and it is the process the file names. A detached start therefore
// waits for the file instead of writing it, and a foreground start gets the
// same protection and the same `zerg status` for free.
func writePidFile(dbPath string) (release func(), err error) {
	path := pidPath(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// O_EXCL is the actual exclusion. Checking then WriteFile let two zerg up
	// both see "not running" and both write, and the loser deleted the
	// winner's file on the way out — a running daemon that status and down
	// could not find.
	payload := []byte(strconv.Itoa(os.Getpid()))
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.Write(payload)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				os.Remove(path)
				if werr == nil {
					werr = cerr
				}
				return nil, fmt.Errorf("recording the daemon's pid in %s: %w", path, werr)
			}
			return func() {
				// Only ours. A later daemon can replace a stale file after we
				// have exited; deleting that would hide it from zerg down.
				if pid, _ := readPidFile(dbPath); pid == os.Getpid() {
					os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("recording the daemon's pid in %s: %w", path, err)
		}
		if pid, running := readPidFile(dbPath); running {
			return nil, fmt.Errorf("zerg is already running for this database (pid %d).\n"+
				"Stop it with `zerg down`, or if that process is not zerg, delete %s", pid, path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("removing a stale pid file %s: %w", path, err)
		}
	}
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

	child := exec.Command(self, append([]string{"up", "--db", dbPath}, args...)...)
	child.Stdout = f
	child.Stderr = f
	// No stdin: a background daemon has nobody to read from, and leaving the
	// terminal's stdin attached is how a backgrounded process ends up stopped
	// with SIGTTIN the first time anything reads it.
	child.Stdin = nil
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return fmt.Errorf("starting the daemon in the background: %w", err)
	}

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

	// Wait for the daemon to say it is up, by writing its own pid file. Without
	// this, a start that fails immediately -- a port in use, a TLS setting that
	// cannot be satisfied -- reports success to the shell and leaves the
	// operator to discover the failure in a log they have not been told about
	// yet.
	pid, err := waitForDaemon(dbPath, exited, 10*time.Second)
	if err != nil {
		return fmt.Errorf("%w\nIts output is in %s", err, out)
	}

	fmt.Printf("zerg is running in the background (pid %d)\n", pid)
	fmt.Printf("Log:  %s\n", out)
	fmt.Printf("Stop: zerg down\n")
	return nil
}

// waitForDaemon blocks until the started process records itself, or dies.
//
// The pid file is checked once more after the process is seen to have exited,
// because both can be true: a daemon that starts, writes its file and then
// fails on something later is still a daemon that started, and the operator
// should be told where its log is rather than that it never came up.
func waitForDaemon(dbPath string, exited <-chan error, grace time.Duration) (int, error) {
	deadline := time.After(grace)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if pid, running := readPidFile(dbPath); running {
			return pid, nil
		}
		select {
		case err := <-exited:
			if pid, running := readPidFile(dbPath); running {
				return pid, nil
			}
			if err != nil {
				return 0, fmt.Errorf("the daemon exited immediately: %w", err)
			}
			return 0, errors.New("the daemon exited immediately")
		case <-deadline:
			return 0, fmt.Errorf("the daemon did not report itself within %s", grace)
		case <-tick.C:
		}
	}
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
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("asking zerg (pid %d) to stop: %w", pid, err)
	}

	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		if !alive(pid) {
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
			// Worth saying rather than reporting a plain "not running": a stale
			// file is what a killed daemon leaves, and it is also what a wrong
			// answer from a reused pid would look like.
			fmt.Printf("zerg is not running for %s\n", path)
			fmt.Printf("(%s names pid %d, which is gone)\n", pidPath(path), pid)
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

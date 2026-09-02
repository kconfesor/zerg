package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A daemon that is gone leaves a pid file behind, and that file must not read
// as a running daemon.
//
// This is the state a killed daemon leaves, so it is the state `zerg up`,
// `zerg down` and `zerg status` most need to get right: reporting a dead pid as
// running would refuse the start that is meant to replace it.
func TestAStalePidFileIsNotARunningDaemon(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	// A pid that has certainly exited: a real process, waited on, so the number
	// names something that was ours and is not any more.
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running a throwaway process: %v", err)
	}
	gone := cmd.Process.Pid

	if err := os.WriteFile(pidPath(db), []byte(strconv.Itoa(gone)), 0o600); err != nil {
		t.Fatalf("writing the pid file: %v", err)
	}
	if pid, running := readPidFile(db); running {
		t.Fatalf("pid %d is gone but was reported as running", pid)
	}

	// And a start over it succeeds, taking the file rather than refusing.
	lock, err := lockPidFile(db)
	if err != nil {
		t.Fatalf("starting over a stale pid file: %v", err)
	}
	defer lock.release()
	if pid, running := readPidFile(db); !running || pid != os.Getpid() {
		t.Fatalf("pid file names %d (running=%v), want this process %d", pid, running, os.Getpid())
	}
}

// A stale pid file naming a pid that has since been reused must not read as a
// running daemon, and must never be signalled.
//
// This is the reported failure, reproduced: a live `sleep`'s pid was put in
// zerg.pid, `zerg down` sent it SIGTERM, killed it, and reported that zerg had
// stopped. Existence is not identity, and the pid file is now a lock rather
// than a number to probe — nothing holds it here, so there is no daemon here.
func TestAReusedPidIsNotARunningDaemon(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	// Somebody else's process, alive for the duration of the test. It stands in
	// for whatever the operating system has given the old daemon's number to.
	stranger := exec.Command("sleep", "30")
	if err := stranger.Start(); err != nil {
		t.Fatalf("starting a bystander process: %v", err)
	}
	t.Cleanup(func() {
		_ = stranger.Process.Kill()
		_ = stranger.Wait()
	})

	if err := os.WriteFile(pidPath(db), []byte(strconv.Itoa(stranger.Process.Pid)), 0o600); err != nil {
		t.Fatalf("writing the pid file: %v", err)
	}

	if _, running := readPidFile(db); running {
		t.Fatal("a live process that is not zerg was reported as a running daemon")
	}

	// `zerg down` reads the same answer, so it never gets as far as signalling.
	err := runDown([]string{"--db", db, "--wait", "1s"})
	if err == nil {
		t.Fatal("zerg down reported success against a pid that is not a daemon")
	}
	if !strings.Contains(err.Error(), "no zerg daemon is running") {
		t.Errorf("zerg down said something other than that nothing is running: %v", err)
	}

	// And the bystander is untouched.
	if stranger.ProcessState != nil {
		t.Fatal("the bystander process was waited on by something")
	}
	if err := stranger.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the bystander process is no longer there: %v", err)
	}
}

// Two daemons must not share one database, and the refusal has to say what to
// do about it.
func TestASecondDaemonOnTheSameDatabaseIsRefused(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	lock, err := lockPidFile(db)
	if err != nil {
		t.Fatalf("lockPidFile: %v", err)
	}
	defer lock.release()

	_, err = lockPidFile(db)
	if err == nil {
		t.Fatal("a second daemon on the same database was allowed")
	}
	// AGENTS.md: an error a person can fix must name the thing to fix.
	if !strings.Contains(err.Error(), pidPath(db)) {
		t.Errorf("the refusal does not name the pid file: %v", err)
	}
	if !strings.Contains(err.Error(), "zerg down") {
		t.Errorf("the refusal does not say how to stop the daemon that holds it: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("the refusal does not say which process holds it: %v", err)
	}
}

// Exactly one of a crowd of simultaneous starts may take the lock.
//
// The old check-then-write could not promise this: every one of them read the
// file, saw nothing running, and wrote its own pid over the last, leaving as
// many daemons as there were starts on one database. The lock is taken by the
// kernel in one step, so there is no window between deciding and acting.
func TestOnlyOneOfManySimultaneousStartsTakesTheLock(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	const racers = 16
	var (
		begin sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		won   []*pidLock
		lost  int
	)
	begin.Add(1)
	for i := 0; i < racers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			begin.Wait()
			lock, err := lockPidFile(db)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				lost++
				return
			}
			won = append(won, lock)
		}()
	}
	begin.Done()
	done.Wait()

	for _, l := range won {
		defer l.release()
	}
	if len(won) != 1 {
		t.Fatalf("%d of %d simultaneous starts took the lock, want exactly 1", len(won), racers)
	}
	if lost != racers-1 {
		t.Errorf("%d starts were refused, want %d", lost, racers-1)
	}
}

// Releasing the lock lets the next daemon start, and the file goes with it.
func TestReleasingTheLockLetsTheNextDaemonStart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	lock, err := lockPidFile(db)
	if err != nil {
		t.Fatalf("lockPidFile: %v", err)
	}
	lock.release()

	if _, err := os.Stat(pidPath(db)); !os.IsNotExist(err) {
		t.Errorf("the pid file outlived the daemon that held it: %v", err)
	}
	if _, running := readPidFile(db); running {
		t.Error("a released lock still reads as a running daemon")
	}

	next, err := lockPidFile(db)
	if err != nil {
		t.Fatalf("starting after a clean release: %v", err)
	}
	next.release()
}

// A daemon that dies during startup is reported as having died, not as one that
// is taking a while.
//
// The probe used to be the pid file, which the daemon writes before it opens
// the database or binds anything, so a start that failed on its listener had
// already "reported itself". The readiness pipe answers the question that was
// meant all along -- is it serving -- and reports a death for free, because the
// write end closes when the process does.
func TestAFailedStartIsReportedImmediately(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	child := exec.Command("sh", "-c", "exit 3")
	child.ExtraFiles = []*os.File{w}
	if err := child.Start(); err != nil {
		t.Fatalf("starting the doomed child: %v", err)
	}
	w.Close()
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	// Generous, so that a pass means the failure was noticed rather than that
	// the grace period happened to be short.
	start := time.Now()
	_, err = waitForDaemon(r, exited, 30*time.Second)
	if err == nil {
		t.Fatal("a daemon that exited immediately was reported as started")
	}
	if !strings.Contains(err.Error(), "exited before it was serving") {
		t.Errorf("the failure does not say the daemon exited: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("noticing the exit took %s; it waited out the grace period", took)
	}
}

// Writing the pid file is not being ready.
//
// Reported and reproduced: `zerg up --detach` returned 0 and printed "running",
// and two seconds later `zerg status` said stopped with
// "bind: address already in use" in the log. The pid file is the lifetime lock
// and is taken before the database is opened or anything is bound, so a parent
// that treats it as readiness is reporting a daemon that has not got there yet.
func TestTakingThePidFileIsNotReadiness(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	// A child that does exactly what the daemon used to do and no more: takes
	// the pid file, then dies on the thing it could not bind.
	child := exec.Command("sh", "-c",
		fmt.Sprintf("printf %d > %q; exit 1", os.Getpid(), pidPath(db)))
	child.ExtraFiles = []*os.File{w}
	if err := child.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	w.Close()
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	if _, err := waitForDaemon(r, exited, 5*time.Second); err == nil {
		t.Fatal("a daemon that wrote its pid file and then failed was reported as running")
	}
}

// A daemon that says it is serving is reported as serving, with the addresses
// it named.
func TestAServingDaemonIsReportedWithItsAddresses(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	go func() {
		signalReady(w, "https://box:8443", "http://127.0.0.1:8443")
	}()

	exited := make(chan error, 1)
	ready, err := waitForDaemon(r, exited, 5*time.Second)
	if err != nil {
		t.Fatalf("waitForDaemon: %v", err)
	}
	if ready.pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", ready.pid, os.Getpid())
	}
	want := []string{"https://box:8443", "http://127.0.0.1:8443"}
	if strings.Join(ready.urls, " ") != strings.Join(want, " ") {
		t.Errorf("urls = %v, want %v", ready.urls, want)
	}
}

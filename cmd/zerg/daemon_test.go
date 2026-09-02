package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	release, err := writePidFile(db)
	if err != nil {
		t.Fatalf("starting over a stale pid file: %v", err)
	}
	defer release()
	if pid, running := readPidFile(db); !running || pid != os.Getpid() {
		t.Fatalf("pid file names %d (running=%v), want this process %d", pid, running, os.Getpid())
	}
}

// Two daemons must not share one database, and the refusal has to say what to
// do about it.
func TestASecondDaemonOnTheSameDatabaseIsRefused(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	release, err := writePidFile(db)
	if err != nil {
		t.Fatalf("writePidFile: %v", err)
	}
	defer release()

	_, err = writePidFile(db)
	if err == nil {
		t.Fatal("a second daemon on the same database was allowed")
	}
	// AGENTS.md: an error a person can fix must name the thing to fix.
	if !strings.Contains(err.Error(), pidPath(db)) {
		t.Errorf("the refusal does not name the pid file to delete: %v", err)
	}
	if !strings.Contains(err.Error(), "zerg down") {
		t.Errorf("the refusal does not say how to stop the daemon that holds it: %v", err)
	}
}

// Releasing does not delete a pid file that belongs to somebody else.
//
// A daemon that lost the race and is exiting runs the same deferred release as
// one that won, and deleting the winner's file would leave a running daemon
// that `zerg down` cannot find.
func TestReleasingOnlyRemovesOurOwnPidFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	release, err := writePidFile(db)
	if err != nil {
		t.Fatalf("writePidFile: %v", err)
	}

	// Somebody else takes the file over while we hold a release for it.
	if err := os.WriteFile(pidPath(db), []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatalf("overwriting the pid file: %v", err)
	}
	release()

	if _, err := os.Stat(pidPath(db)); err != nil {
		t.Fatalf("the other daemon's pid file was deleted: %v", err)
	}
}

// A daemon that dies during startup is reported as having died, not as one that
// is taking a while.
//
// The obvious liveness probe is kill(pid, 0), and on a child of this process it
// is wrong: an exited child nobody has waited on is a zombie, and a zombie
// answers signal 0 exactly as a running process does. That turned "the socket
// could not be bound" into a ten-second wait and then "the daemon did not
// report itself", which sends somebody looking for a hang instead of reading
// the error in the log.
func TestAFailedStartIsReportedImmediately(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")

	child := exec.Command("sh", "-c", "exit 3")
	if err := child.Start(); err != nil {
		t.Fatalf("starting the doomed child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	// Generous, so that a pass means the failure was noticed rather than that
	// the grace period happened to be short.
	start := time.Now()
	_, err := waitForDaemon(db, exited, 30*time.Second)
	if err == nil {
		t.Fatal("a daemon that exited immediately was reported as started")
	}
	if !strings.Contains(err.Error(), "exited immediately") {
		t.Errorf("the failure does not say the daemon exited: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("noticing the exit took %s; it waited out the grace period", took)
	}
}

// A daemon that starts, records itself and only then fails is a daemon that
// started: the operator should be pointed at its log rather than told it never
// came up.
func TestADaemonThatRecordedItselfCountsAsStarted(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zerg.db")
	if err := os.WriteFile(pidPath(db), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("writing the pid file: %v", err)
	}

	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")

	pid, err := waitForDaemon(db, exited, time.Second)
	if err != nil {
		t.Fatalf("waitForDaemon: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

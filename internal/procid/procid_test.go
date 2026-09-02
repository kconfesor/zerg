package procid

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A process identifies as itself, and identifying it twice gives the same
// answer. Without that this is not an identity, it is a timestamp.
func TestAProcessIdentifiesAsItself(t *testing.T) {
	first, err := Of(os.Getpid())
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if first == "" {
		t.Fatal("Of returned an empty identity")
	}
	second, err := Of(os.Getpid())
	if err != nil {
		t.Fatalf("Of, again: %v", err)
	}
	if first != second {
		t.Errorf("the same process identified two ways:\n%q\n%q", first, second)
	}
	ok, err := Is(os.Getpid(), first)
	if err != nil {
		t.Fatalf("Is: %v", err)
	}
	if !ok {
		t.Error("a process did not match its own identity")
	}
}

// A pid whose process has been replaced does not match what was recorded for
// it. This is the whole reason the identity exists: the alternative is
// signalling a stranger's process on the strength of a number in a database.
func TestAReusedPidDoesNotMatch(t *testing.T) {
	live := exec.Command("sleep", "30")
	if err := live.Start(); err != nil {
		t.Fatalf("starting a process to stand in for the reuser: %v", err)
	}
	t.Cleanup(func() {
		_ = live.Process.Kill()
		_ = live.Wait()
	})

	ok, err := Is(live.Process.Pid, "proc:0")
	if err != nil {
		t.Fatalf("Is: %v", err)
	}
	if ok {
		t.Error("a live process matched an identity that was never its own")
	}

	// And an identity that was never recorded is an error rather than a
	// cheerful no: a caller must not read "we have nothing to compare" as
	// "this is not ours".
	if _, err := Is(live.Process.Pid, ""); err == nil {
		t.Error("an empty identity was accepted as a comparison")
	}
}

// A pid that is simply gone is a no rather than an error. Nothing to act on is
// an answer, and the common one after a machine has been rebooted.
func TestAPidThatIsGoneDoesNotMatchAndIsNotAnError(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running a throwaway process: %v", err)
	}

	ok, err := Is(cmd.Process.Pid, "proc:whatever")
	if err != nil {
		t.Fatalf("Is on a pid that is gone: %v", err)
	}
	if ok {
		t.Error("a process that has exited matched a stored identity")
	}
}

// A process group is stopped as a whole, descendants included, which is the
// point of putting agents in one: every bash tool call is a child in it, and a
// grandchild left running is a grandchild still writing to the worktree.
func TestStopGroupTakesTheDescendantsToo(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a group: %v", err)
	}
	// Reaped as it exits, the way init reaps a dead daemon's orphans. A zombie
	// answers signal 0 exactly as a live process does, so without this the
	// group never looks empty.
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waited
	})

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if !GroupAlive(pgid) {
		t.Fatal("the group was not there to begin with")
	}
	if !StopGroup(pgid, 5*time.Second) {
		t.Fatal("StopGroup reported the group was still running")
	}
	if GroupAlive(pgid) {
		t.Error("something is still in the group after it was stopped")
	}
}

// This daemon's own process group is never signalled.
//
// A pgid read back out of a database is a number, and one that has come to name
// the daemon's own group would have the boot sweep kill the daemon, its cockpit
// and every agent it is supervising.
func TestStopGroupRefusesOurOwnGroup(t *testing.T) {
	if StopGroup(syscall.Getpgrp(), time.Second) {
		t.Error("StopGroup claimed to have stopped the group this process is in")
	}
	if !GroupAlive(syscall.Getpgrp()) {
		t.Fatal("this process's own group was signalled")
	}
}

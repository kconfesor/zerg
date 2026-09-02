package overmind

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/procid"
	"github.com/kconfesor/zerg/internal/store"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stray starts a process in a process group of its own, with a child in that
// group, and returns what a killed daemon would have left on record for it.
//
// A child, because the leader alone is not what has to die: every bash tool
// call an agent makes is in the group, and a grandchild left running is a
// grandchild still writing to the worktree.
func stray(t *testing.T) (pid, pgid int, identity string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a stand-in agent: %v", err)
	}

	// Reaped on a goroutine, which is what a real orphan gets for free and this
	// one does not: the daemon that spawned these is dead, so init reaps them,
	// while here the test binary is their parent. An exited child nobody has
	// waited on is a zombie, and a zombie answers signal 0 exactly as a running
	// process does — so without this the group never looks empty and the test
	// would report a failure to stop that is an artefact of the fixture.
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waited
	})

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("reading the process group: %v", err)
	}
	identity, err = procid.Of(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("identifying the stand-in agent: %v", err)
	}
	return cmd.Process.Pid, pgid, identity
}

// An agent the previous daemon left running is stopped before this one does
// anything else with its project.
//
// This is the failure the whole table exists for. Each agent runs in a process
// group of its own and the group is signalled from cmd.Cancel, which a
// SIGKILLed daemon never reaches: measured, a coder was still writing files
// into its worktree thirty seconds after kill -9 on the daemon. The next daemon
// then reclaimed its lease and, with resumeOnStart, put a replacement into the
// same worktree with the same conversation resumed.
func TestAgentsLeftRunningByTheLastDaemonAreStopped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	pid, pgid, identity := stray(t)
	if err := h.db.RecordAgentProcess(ctx, store.AgentProcess{
		ProjectID: h.project.ID, Role: "coder",
		PID: pid, PGID: pgid, Identity: identity,
		Worktree: h.project.Path,
	}); err != nil {
		t.Fatalf("RecordAgentProcess: %v", err)
	}

	stopped, left, err := ReapPreviousRun(ctx, h.db, quietLog())
	if err != nil {
		t.Fatalf("ReapPreviousRun: %v", err)
	}
	if stopped != 1 {
		t.Errorf("stopped %d agents, want 1", stopped)
	}
	if len(left) != 0 {
		t.Errorf("%d agents were left unaccounted for, want 0", len(left))
	}

	// The effect, not the call: nothing is left in that process group. The
	// child counts, which is why one was started.
	if procid.GroupAlive(pgid) {
		t.Error("something is still running in the agent's process group")
	}

	rows, err := h.db.AgentProcessesFor(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("AgentProcessesFor: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d rows survived a successful stop; the next start would be refused", len(rows))
	}
}

// A pid that has been reused is somebody else's process, and it is left alone.
//
// The whole reason the identity is stored. Signalling on a pid read out of a
// database is how a cleanup kills a stranger's process — reproduced against the
// pid file, where `zerg down` did exactly that to a `sleep` and reported
// success. Here the refusal costs a resume, which is the right side to be on.
func TestAProcessThatIsNotOursIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	pid, pgid, _ := stray(t)
	if err := h.db.RecordAgentProcess(ctx, store.AgentProcess{
		ProjectID: h.project.ID, Role: "coder",
		PID: pid, PGID: pgid,
		// What the previous daemon recorded, against a pid the machine has
		// since given to something else.
		Identity: "ps:Mon Jan  1 00:00:00 2001 /usr/bin/claude",
		Worktree: h.project.Path,
	}); err != nil {
		t.Fatalf("RecordAgentProcess: %v", err)
	}

	stopped, left, err := ReapPreviousRun(ctx, h.db, quietLog())
	if err != nil {
		t.Fatalf("ReapPreviousRun: %v", err)
	}
	if stopped != 0 {
		t.Errorf("stopped %d processes that were not ours, want 0", stopped)
	}
	if len(left) != 1 {
		t.Fatalf("%d agents were reported unaccounted for, want 1", len(left))
	}

	if !procid.GroupAlive(pgid) {
		t.Error("a process group that was not ours was signalled")
	}

	// And the row stays, which is what refuses the unattended resume.
	rows, err := h.db.AgentProcessesFor(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("AgentProcessesFor: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows left, want the unresolved one to stay", len(rows))
	}
	if err := h.over.Start(ctx, h.project.ID); err == nil {
		t.Error("a start into a worktree that may still be held was allowed")
	}
}

// A process that is simply gone leaves nothing to do, and its row goes with it.
//
// The common case after a crash: the machine has been rebooted, or the harness
// noticed its parent had died. A row kept here would refuse every later start
// of a project with nothing wrong with it.
func TestAnAgentThatIsAlreadyGoneIsForgotten(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	// A real process, in its own group, waited on: the number named something
	// that was ours and is not any more.
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		t.Fatalf("running a throwaway process: %v", err)
	}
	gone := cmd.Process.Pid

	if err := h.db.RecordAgentProcess(ctx, store.AgentProcess{
		ProjectID: h.project.ID, Role: "coder",
		PID: gone, PGID: gone, Identity: "ps:whatever it was",
		Worktree: h.project.Path,
	}); err != nil {
		t.Fatalf("RecordAgentProcess: %v", err)
	}

	stopped, left, err := ReapPreviousRun(ctx, h.db, quietLog())
	if err != nil {
		t.Fatalf("ReapPreviousRun: %v", err)
	}
	if stopped != 0 || len(left) != 0 {
		t.Errorf("stopped=%d unresolved=%d, want a process that was already gone to be neither", stopped, len(left))
	}
	rows, err := h.db.AgentProcessesFor(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("AgentProcessesFor: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d rows survived for a process that no longer exists", len(rows))
	}
	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Errorf("a project whose agents are provably gone would not start: %v", err)
	}
}

// A running swarm has its processes on record, and a clean stop takes them off
// again.
//
// The record is written by the supervisor at spawn and removed when the process
// has been waited for, so what is left at boot is exactly what the previous
// daemon did not stop. A row that outlived a clean stop would refuse the next
// start of a project with nothing wrong with it.
func TestARunningAgentIsOnRecordAndACleanStopClearsIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var running []store.AgentProcess
	waitFor(t, func() bool {
		rows, err := h.db.AgentProcessesFor(ctx, h.project.ID)
		if err != nil || len(rows) == 0 {
			return false
		}
		running = rows
		return true
	}, 15*time.Second, "no agent process was ever recorded for a running swarm")

	for _, p := range running {
		if !procid.GroupAlive(p.PGID) {
			t.Errorf("role %s is on record as process group %d, which is not there", p.Role, p.PGID)
		}
		if p.Worktree == "" {
			t.Errorf("role %s was recorded without the worktree it is holding", p.Role)
		}
	}

	if err := h.over.Stop(ctx, h.project.ID, "test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	rows, err := h.db.AgentProcessesFor(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("AgentProcessesFor: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d agent processes are still on record after a clean stop", len(rows))
	}
}

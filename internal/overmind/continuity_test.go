package overmind

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/cerebrate"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/preflight"
	"github.com/kconfesor/zerg/internal/store"
)

// resumableHarness is a scripted agent that names its own conversation, which
// is what both real harnesses do and the only thing that makes resume possible.
//
// It is a separate double from scriptedHarness rather than a flag on it because
// it has to be session-scoped: the latch belongs to one process, and sharing it
// is the bug adapter.SessionScoped exists to prevent.
type resumableHarness struct {
	// shared is state every instance reports into, since the point of the test
	// is what the *next* spawn was given.
	shared *spawnLog

	session string
}

// spawnLog is what the harness was asked to run, across spawns.
type spawnLog struct {
	mu      sync.Mutex
	resumed []string // Spec.ResumeSession, one entry per spawn, in order

	// reject makes every spawn that is handed a session refuse it the way
	// claude refuses one it has no transcript for. Set on the log rather than
	// on the harness because a restarted daemon builds its adapters afresh,
	// and the double has to keep answering the same way across that.
	reject bool

	// silent makes every spawn announce a session and then say nothing, which
	// is a role that is never given work: it boots, reports ready and waits.
	// The harness has named a conversation; there is no conversation.
	silent bool
}

func (l *spawnLog) record(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resumed = append(l.resumed, id)
}

func (l *spawnLog) rejectResumes() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reject = true
}

func (l *spawnLog) staySilent() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.silent = true
}

func (l *spawnLog) quiet() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.silent
}

func (l *spawnLog) rejecting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reject
}

func (l *spawnLog) spawns() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.resumed...)
}

func (h *resumableHarness) Name() string            { return "scripted" }
func (h *resumableHarness) Checks() []adapter.Check { return nil }
func (h *resumableHarness) Capabilities() adapter.Caps {
	return adapter.Caps{StructuredOutput: true, StructuredInput: true, ResumeSession: true}
}
func (h *resumableHarness) ListModels(adapter.Ctx) ([]adapter.Model, error) { return nil, nil }
func (h *resumableHarness) NewSession() adapter.Adapter {
	return &resumableHarness{shared: h.shared}
}
func (h *resumableHarness) SessionID() string { return h.session }

func (h *resumableHarness) Command(ctx context.Context, spec adapter.Spec) (*exec.Cmd, error) {
	h.shared.record(spec.ResumeSession)

	// The conversation this process is writing to, announced on its own stream
	// the way claude and pi announce theirs. A resumed process keeps the id it
	// was handed; a cold one invents one, which is the harness naming a new
	// conversation.
	id := spec.ResumeSession
	if id == "" {
		id = "sess-" + spec.Role + "-" + store.NewID()[:8]
	}
	// Announced, then answered. The second line is what makes the conversation
	// real: claude names a session in its first frame and writes the transcript
	// only once there is something in it, so a double that only announces is a
	// role that was never given work, not a role that can be resumed.
	script := "printf 'session:" + id + "\\n'; printf 'said:ok\\n'; sleep 30"
	if h.shared.quiet() {
		script = "printf 'session:" + id + "\\n'; sleep 30"
	}
	if spec.ResumeSession != "" && h.shared.rejecting() {
		// What claude does with a session id it has no transcript for: says so
		// on the stream and exits non-zero, without ever running a turn.
		script = "printf 'stale:" + spec.ResumeSession + "\\n'; exit 1"
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = spec.Worktree
	cmd.Env = append(os.Environ(),
		agent.EnvSocket+"="+spec.Socket,
		agent.EnvToken+"="+spec.Token,
		agent.EnvRole+"="+spec.Role,
	)
	return cmd, nil
}

func (h *resumableHarness) Parse(line []byte) ([]adapter.Event, error) {
	text := strings.TrimSpace(string(line))
	if id, ok := strings.CutPrefix(text, "session:"); ok {
		h.session = id
		return []adapter.Event{{Kind: adapter.EventReady}}, nil
	}
	if _, ok := strings.CutPrefix(text, "said:"); ok {
		return []adapter.Event{{Kind: adapter.EventMessage, Text: "ok"}}, nil
	}
	if id, ok := strings.CutPrefix(text, "stale:"); ok {
		// The id still rides the frame, as it does on the real one, which is
		// why forgetting cannot be left to the latch.
		h.session = id
		return []adapter.Event{{
			Kind:         adapter.EventError,
			Text:         "No conversation found with session ID: " + id,
			StaleSession: true,
		}}, nil
	}
	return nil, nil
}

func (h *resumableHarness) EncodeTurn(text string) ([]byte, error) { return []byte(text + "\n"), nil }
func (h *resumableHarness) EncodeInterrupt() ([]byte, error)       { return nil, adapter.ErrNoInterrupt }

// newDaemon builds a second Overmind over an existing harness's database, which
// is what a daemon restart is: the processes are gone, the database is not.
//
// It is given a fresh resumable harness sharing the same spawn log, because a
// restarted daemon builds its adapters from the registry again. Reusing the
// instance would hide the thing being tested: what the second daemon knows has
// to come out of the database, not out of a Go value that outlived the process.
func newDaemon(t *testing.T, h *harness, log *spawnLog) *Overmind {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.Register(&resumableHarness{shared: log})
	over := New(Config{
		DB: h.db, Nydus: h.nyd, Registry: reg,
		Preflight: preflight.NewRunner(h.db, reg),
		Bus:       event.NewBus(),
		Agents:    h.agents,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:  t.TempDir(),
	})
	t.Cleanup(func() { over.StopAll(context.Background(), "test over") })
	return over
}

// resumableHarnessFor swaps the standard double for one that names sessions.
func resumableHarnessFor(t *testing.T, log *spawnLog) *harness {
	t.Helper()
	// newHarness registers whatever it is given under the name the seeded roles
	// use, so the swap is transparent to everything downstream.
	h := newHarness(t, &scriptedHarness{script: idleAgent})
	reg := adapter.NewRegistry()
	reg.Register(&resumableHarness{shared: log})
	h.over.registry = reg
	h.over.preflight = preflight.NewRunner(h.db, reg)
	return h
}

// A daemon restart puts the agents back into the conversation they were
// holding, rather than starting them cold.
//
// The assertion is on the spec the second spawn actually received, and the id
// in it came out of the first process's own output — not from anything the test
// told the daemon to remember.
func TestAgentsResumeTheConversationAfterADaemonRestart(t *testing.T) {
	ctx := context.Background()
	log := &spawnLog{}
	h := resumableHarnessFor(t, log)

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Every running role, not any of them.
	//
	// The assertion below is that each spawn after the restart resumes an id
	// this daemon recorded, so the snapshot has to be complete before it is
	// taken. Waiting for the first one meant a role that recorded a moment
	// later was resumed against a set that predated it, and the test failed
	// naming an id its own daemon had stored. Rare while every role recorded
	// on its first line; a standing race once recording waits for the agent to
	// answer, because the roles then reach it at their own pace.
	waitFor(t, func() bool {
		roles, err := h.db.ProjectsWantingStart(ctx)
		return err == nil && len(roles) == 1 && allRecorded(t, h)
	}, 10*time.Second, "not every running role recorded a session")

	first := recordedSessions(t, h.db, h.project.ID)
	if len(first) == 0 {
		t.Fatal("expected at least one role to have recorded a session")
	}

	// The daemon goes down. Not the operator: nothing here is a decision.
	h.over.StopAll(ctx, "the daemon shut down")

	spawnsBefore := len(log.spawns())
	next := newDaemon(t, h, log)
	n, err := next.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the one running project to come back, got %d", n)
	}

	waitFor(t, func() bool { return len(log.spawns()) > spawnsBefore }, 10*time.Second,
		"nothing was spawned after the restart")

	// Every spawn of the second daemon asked to continue a conversation, and
	// each id is one the first daemon's processes announced.
	after := log.spawns()[spawnsBefore:]
	if len(after) == 0 {
		t.Fatal("no spawns recorded after the restart")
	}
	for i, got := range after {
		if got == "" {
			t.Errorf("spawn %d after the restart started a cold session; expected a resume", i)
			continue
		}
		if !first[got] {
			t.Errorf("spawn %d resumed %q, which no earlier process ever announced", i, got)
		}
	}
}

// Pressing Stop is a decision, and it is recorded as one: the project stops
// wanting to run, and the conversations are forgotten.
func TestAnOperatorStopWithdrawsWhatADaemonShutdownKeeps(t *testing.T) {
	ctx := context.Background()
	log := &spawnLog{}
	h := resumableHarnessFor(t, log)

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return anyRecorded(t, h.db, h.project.ID) }, 10*time.Second,
		"no session was recorded")

	// A shutdown leaves both intact.
	h.over.StopAll(ctx, "the daemon shut down")
	if want, err := h.db.ProjectsWantingStart(ctx); err != nil || len(want) != 1 {
		t.Fatalf("after a daemon shutdown the project should still want to run, got %v (%v)", want, err)
	}
	if len(recordedSessions(t, h.db, h.project.ID)) == 0 {
		t.Fatal("a daemon shutdown discarded the conversations it should have kept")
	}

	// The operator's stop clears both. Started again first, because Stop needs
	// something running to stop.
	next := newDaemon(t, h, log)
	if err := next.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start again: %v", err)
	}
	if err := next.Stop(ctx, h.project.ID, "stopped by the operator"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if want, err := h.db.ProjectsWantingStart(ctx); err != nil || len(want) != 0 {
		t.Fatalf("after an operator stop the project should not want to run, got %v (%v)", want, err)
	}
	if got := recordedSessions(t, h.db, h.project.ID); len(got) != 0 {
		t.Fatalf("an operator stop left %d conversations behind", len(got))
	}
}

// A project the operator stopped does not come back by itself, which is the
// half of the feature that is about not doing something.
func TestResumeLeavesAStoppedProjectStopped(t *testing.T) {
	ctx := context.Background()
	log := &spawnLog{}
	h := resumableHarnessFor(t, log)

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.over.Stop(ctx, h.project.ID, "stopped by the operator"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	next := newDaemon(t, h, log)
	n, err := next.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 0 {
		t.Fatalf("a stopped project was started again by Resume (%d started)", n)
	}
	if next.Running(h.project.ID) {
		t.Fatal("a stopped project is running after a restart")
	}
}

// A prompt edit is exactly the change a resumed conversation would contradict,
// so the stored session must not survive one.
//
// ARCHITECTURE §11.3 restarts a role when its configuration changes precisely
// so the new configuration applies. Resuming across that would replay a
// conversation shaped by the instructions that were just replaced, while the
// board reported the new ones were in force.
func TestAPromptEditIsNotResumedAcross(t *testing.T) {
	ctx := context.Background()
	log := &spawnLog{}
	h := resumableHarnessFor(t, log)

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return anyRecorded(t, h.db, h.project.ID) }, 10*time.Second,
		"no session was recorded")
	h.over.StopAll(ctx, "the daemon shut down")

	// The operator rewrites the shared instructions, which every role's
	// composed prompt is built from.
	if err := h.db.SetSetting(ctx, store.SettingSharedInstructions,
		"Work only in Rust. This is not what the previous conversation was told."); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	spawnsBefore := len(log.spawns())
	next := newDaemon(t, h, log)
	if _, err := next.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, func() bool { return len(log.spawns()) > spawnsBefore }, 10*time.Second,
		"nothing was spawned after the edit")

	for i, got := range log.spawns()[spawnsBefore:] {
		if got != "" {
			t.Errorf("spawn %d resumed %q across a prompt edit; it should have started cold", i, got)
		}
	}
}

// A session the harness no longer has must be dropped, not retried.
//
// zerg latches a session id off the harness's first frame; claude writes the
// transcript only when there is something to write. A swarm killed before any
// role finished a turn therefore leaves ids pointing at nothing, and the
// restart this feature exists for spawned into "No conversation found with
// session ID" every time, doubling the backoff towards a swarm that would never
// come back. Measured against claude 2.1.258 before this: five attempts per
// role, no turns, no tokens, and the reason never reached the log.
func TestASessionTheHarnessNoLongerHasIsDroppedRatherThanRetried(t *testing.T) {
	ctx := context.Background()
	log := &spawnLog{}
	h := resumableHarnessFor(t, log)

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return anyRecorded(t, h.db, h.project.ID) }, 10*time.Second,
		"no session was recorded")
	before := recordedSessions(t, h.db, h.project.ID)
	h.over.StopAll(ctx, "the daemon shut down")

	// The transcripts are gone, which the daemon has no way of knowing until a
	// spawn is refused.
	log.rejectResumes()

	spawnsBefore := len(log.spawns())
	next := newDaemon(t, h, log)
	if _, err := next.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The refusal itself drops the row, so the next spawn is cold and that
	// spawn's new id is stored. Recording the latched id before noticing
	// StaleSession left Forget for a later spawn's first line, and that
	// leftover flag then dropped the cold spawn's id — so the assertion is
	// both that a cold spawn happens and that what it announced is on record.
	waitFor(t, func() bool {
		for _, got := range log.spawns()[spawnsBefore:] {
			if got == "" {
				return true
			}
		}
		return false
	}, 30*time.Second, "every attempt after the refusal resumed the session that was refused")

	waitFor(t, func() bool {
		now := recordedSessions(t, h.db, h.project.ID)
		for id := range before {
			if now[id] {
				return false
			}
		}
		return true
	}, 30*time.Second, "a session the harness rejected is still on record")

	waitFor(t, func() bool {
		now := recordedSessions(t, h.db, h.project.ID)
		for id := range now {
			if !before[id] {
				return true
			}
		}
		return false
	}, 30*time.Second, "the cold spawn did not record a new session")
}

// anyRecorded reports whether any role of this project has a session on record,
// under any fingerprint.
func anyRecorded(t *testing.T, db *store.DB, projectID string) bool {
	t.Helper()
	return len(recordedSessions(t, db, projectID)) > 0
}

// allRecorded reports whether every role the swarm is running has a session
// stored, which is what a test asserting on the whole set has to wait for.
func allRecorded(t *testing.T, h *harness) bool {
	t.Helper()
	live := h.live(t)
	if len(live) == 0 {
		return false
	}
	stored, err := h.db.ListRoleSessions(context.Background(), h.project.ID)
	if err != nil {
		t.Fatalf("reading recorded sessions: %v", err)
	}
	byRole := map[string]bool{}
	for _, s := range stored {
		byRole[s.Role] = true
	}
	for role := range live {
		if !byRole[role] {
			return false
		}
	}
	return true
}

// recordedSessions is every session id stored for a project, as a set. Read
// straight out of the table, because the point is what the daemon persisted
// rather than what it was told.
func recordedSessions(t *testing.T, db *store.DB, projectID string) map[string]bool {
	t.Helper()
	sessions, err := db.ListRoleSessions(context.Background(), projectID)
	if err != nil {
		t.Fatalf("reading recorded sessions: %v", err)
	}
	out := map[string]bool{}
	for _, s := range sessions {
		out[s.SessionID] = true
	}
	return out
}

// A conversation the harness named but never wrote is not recorded.
//
// The id a harness announces is a proxy for a resumable conversation, and on
// claude it is not the thing itself: the transcript appears once there is
// something in it. A role the cards keep skipping boots, reports ready and
// waits, so the id went on record naming nothing. The next start resumed it,
// claude answered "No conversation found with session ID", the agent exited 1,
// and its replacement recorded another empty id: a failure that regenerated its
// own cause on every restart, one wasted spawn per idle role, forever. Observed
// on a live daemon across four restarts, each with a different dead id.
//
// Recording nothing is the honest answer. The role starts cold, which is what a
// refused resume left it with anyway, one spawn later.
func TestARoleThatNeverSpokeRecordsNoSessionToResume(t *testing.T) {
	ctx := context.Background()
	log := &spawnLog{}
	log.staySilent()
	h := resumableHarnessFor(t, log)

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Ready is produced by the same line that latches the session id, and the
	// recorder runs on that line's events, so a ready role has already been
	// through every chance to record one. No sleeping for it.
	waitFor(t, func() bool {
		roles, err := h.over.Status(ctx, h.project.ID)
		if err != nil || len(roles) == 0 {
			return false
		}
		for _, r := range roles {
			if r.State != cerebrate.StateReady {
				return false
			}
		}
		return true
	}, 10*time.Second, "the swarm never came up")

	if got := recordedSessions(t, h.db, h.project.ID); len(got) != 0 {
		t.Fatalf("recorded %v for roles that said nothing; every one of those ids "+
			"names a conversation the harness never wrote", got)
	}

	// And the restart it exists for starts them cold rather than resuming into
	// a conversation that was never written.
	h.over.StopAll(ctx, "the daemon shut down")
	spawnsBefore := len(log.spawns())
	next := newDaemon(t, h, log)
	if _, err := next.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, func() bool { return len(log.spawns()) > spawnsBefore }, 10*time.Second,
		"nothing was spawned after the restart")
	for i, got := range log.spawns()[spawnsBefore:] {
		if got != "" {
			t.Errorf("spawn %d resumed %q, which the harness never wrote a transcript for", i, got)
		}
	}
}

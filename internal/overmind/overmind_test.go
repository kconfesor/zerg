package overmind

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/cerebrate"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/preflight"
	"github.com/kconfesor/zerg/internal/store"
)

// scriptedHarness stands in for claude or pi. Its "agent" is a shell script
// that speaks the same four verbs a real one does, so a whole pipeline runs
// end to end without a token being spent.
type scriptedHarness struct {
	script func(spec adapter.Spec) string
	checks []adapter.Check
	// harnessName overrides "scripted", for a test that needs two registered
	// harnesses to move a role between them.
	harnessName string

	mu       sync.Mutex
	prompts  []string
	spawned  []string
	turnsGot int
}

func (h *scriptedHarness) Name() string {
	if h.harnessName != "" {
		return h.harnessName
	}
	return "scripted"
}

func (h *scriptedHarness) Checks() []adapter.Check { return h.checks }
func (h *scriptedHarness) Capabilities() adapter.Caps {
	return adapter.Caps{StructuredOutput: true, StructuredInput: true}
}
func (h *scriptedHarness) ListModels(adapter.Ctx) ([]adapter.Model, error) { return nil, nil }

func (h *scriptedHarness) Command(ctx context.Context, spec adapter.Spec) (*exec.Cmd, error) {
	h.mu.Lock()
	h.spawned = append(h.spawned, spec.Role)
	if spec.SystemFile != "" {
		if b, err := os.ReadFile(spec.SystemFile); err == nil {
			h.prompts = append(h.prompts, string(b))
		}
	}
	h.mu.Unlock()

	cmd := exec.CommandContext(ctx, "sh", "-c", h.script(spec))
	cmd.Dir = spec.Worktree
	cmd.Env = append(os.Environ(),
		agent.EnvSocket+"="+spec.Socket,
		agent.EnvToken+"="+spec.Token,
		agent.EnvRole+"="+spec.Role,
		"ZERG_BIN="+zergBin,
	)
	return cmd, nil
}

func (h *scriptedHarness) Parse(line []byte) ([]adapter.Event, error) {
	text := strings.TrimSpace(string(line))
	switch {
	case text == "":
		return nil, nil
	case text == "ready":
		return []adapter.Event{{Kind: adapter.EventReady}}, nil
	case text == "turn_end":
		return []adapter.Event{{Kind: adapter.EventTurnEnd}}, nil
	case strings.HasPrefix(text, "log:"):
		return []adapter.Event{{Kind: adapter.EventMessage, Text: text[4:]}}, nil
	default:
		return nil, nil
	}
}

func (h *scriptedHarness) EncodeTurn(text string) ([]byte, error) {
	h.mu.Lock()
	h.turnsGot++
	h.mu.Unlock()
	return []byte(text + "\n"), nil
}

func (h *scriptedHarness) EncodeInterrupt() ([]byte, error) {
	return []byte("{\"interrupt\":true}\n"), nil
}

// turns and spawns are read from the test goroutine while the supervisor
// writes them, so both need the lock. Reading a counter unlocked because "it
// is only a test" is still a race, and the detector is right to say so.
func (h *scriptedHarness) turns() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turnsGot
}

func (h *scriptedHarness) spawnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.spawned)
}

func (h *scriptedHarness) prompt(i int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i < len(h.prompts) {
		return h.prompts[i]
	}
	return ""
}

// zergBin is the CLI the scripted agents actually invoke, so the socket, the
// token and the four verbs are exercised for real rather than simulated.
var zergBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zergbin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating a temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	zergBin = filepath.Join(dir, "zerg")
	build := exec.Command("go", "build", "-o", zergBin, "github.com/kconfesor/zerg/cmd/zerg")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the zerg CLI: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type harness struct {
	over    *Overmind
	db      *store.DB
	nyd     *nydus.Nydus
	agents  *agent.Server
	project *store.Project
	script  *scriptedHarness
}

func newHarness(t *testing.T, sh *scriptedHarness) *harness {
	t.Helper()
	ctx := context.Background()

	dbDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dbDir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "scripted"); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// Point every seeded role at the scripted harness.
	tpls, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	for i := range tpls {
		tpls[i].Harness = "scripted"
		if err := db.UpdateTemplate(ctx, &tpls[i]); err != nil {
			t.Fatalf("UpdateTemplate: %v", err)
		}
	}

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p, err := db.CreateProject(ctx, repo, "", "main")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}

	reg := adapter.NewRegistry()
	reg.Register(sh)

	nyd := nydus.New(db, nydus.WithIntegrator(nydus.Git{}))
	agents := agent.NewServer(db, nyd, artifact.New(filepath.Join(t.TempDir(), "artifacts")), slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Short socket path: macOS caps unix socket paths near 104 bytes.
	socket := filepath.Join("/tmp", store.NewID()[:12]+".sock")
	if err := agents.Listen(socket); err != nil {
		t.Fatalf("agent Listen: %v", err)
	}
	t.Cleanup(func() { agents.Close() })

	over := New(Config{
		DB: db, Nydus: nyd, Registry: reg,
		Preflight: preflight.NewRunner(db, reg),
		Bus:       event.NewBus(),
		Agents:    agents,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:  filepath.Join(dbDir, "state"),
	})
	t.Cleanup(func() { over.StopAll(context.Background(), "test over") })

	return &harness{over: over, db: db, nyd: nyd, agents: agents, project: p, script: sh}
}

// idleAgent reports ready and then waits, doing nothing on its own.
func idleAgent(adapter.Spec) string { return `printf 'ready\n'; sleep 30` }

// workingAgent claims work through the real CLI, forwards or finishes, and
// acknowledges — the loop a real agent's prompt asks it to run.
func workingAgent(spec adapter.Spec) string {
	// --body is required on a handoff, so the scripted agent sends one too. A
	// fake that skips what every real agent must do stops testing the path a
	// real agent takes.
	forward := `"$ZERG_BIN" send --to reviewer --task "$TASK" --commit "$SHA" --body "did the work" >/dev/null`
	if spec.Role == "reviewer" {
		forward = `"$ZERG_BIN" send --task "$TASK" --commit "$SHA" --body "approved" >/dev/null`
	}
	return `printf 'ready\n'
while IFS= read -r _line; do
  WORK=$("$ZERG_BIN" next --wait 5s)
  [ -z "$WORK" ] && { printf 'turn_end\n'; continue; }
  LEASE=$(printf '%s' "$WORK" | sed -n 's/.*"leaseId": "\([^"]*\)".*/\1/p')
  TASK=$(printf '%s' "$WORK"  | sed -n 's/.*"taskId": "\([^"]*\)".*/\1/p' | head -1)
  printf 'log:claimed %s\n' "$LEASE"

  printf '%s did work\n' "$ZERG_ROLE" >> output.txt
  git add -A >/dev/null 2>&1
  git -c user.email=t@example.com -c user.name=T commit -q -m "work by $ZERG_ROLE" >/dev/null 2>&1
  SHA=$(git rev-parse HEAD)

  ` + forward + `
  "$ZERG_BIN" done --lease "$LEASE" >/dev/null
  printf 'turn_end\n'
done`
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}

// ── the whole thing ───────────────────────────────────────────────────────

// A task travels the entire pipeline: the operator opens a card, coder claims
// it through the real socket, hands to reviewer, reviewer finishes, and the
// commit reaches the base branch. No tokens, no tmux, no keystrokes.
func TestFullPipelineWithScriptedAgents(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: workingAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	task, err := h.nyd.NewTask(ctx, h.project.ID, "Calculator", "build a calculator", "", nil)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	waitFor(t, func() bool {
		got, err := h.db.GetTask(ctx, task.ID)
		return err == nil && got.State == store.TaskDone
	}, 45*time.Second, "the task never reached Done")

	done, err := h.db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if done.Lane != store.LaneDone {
		t.Errorf("finished card sits in %q, want the Done well", done.Lane)
	}
	if done.ActiveMS <= 0 {
		t.Error("worked time did not accrue")
	}

	// The terminal role's commit reached the base branch, and the orchestrator
	// is what put it there.
	out, err := exec.Command("git", "-C", h.project.Path, "log", "--oneline", "main").Output()
	if err != nil {
		t.Fatalf("reading the base branch: %v", err)
	}
	if !strings.Contains(string(out), "work by reviewer") {
		t.Errorf("the base branch does not carry the terminal work:\n%s", out)
	}
}

// ── the readiness gate ────────────────────────────────────────────────────

// A team that cannot work must never reach a running board. Without the gate a
// launch succeeds whatever state its agents are in.
func TestStartRefusesWhenARoleIsBlocked(t *testing.T) {
	blocked := adapter.Check{Name: "binary_version", Run: func(adapter.Ctx, adapter.Spec) adapter.Result {
		return adapter.Result{Reason: "the CLI is too old for this model", Remedy: "upgrade it"}
	}}
	h := newHarness(t, &scriptedHarness{script: idleAgent, checks: []adapter.Check{blocked}})

	err := h.over.Start(context.Background(), h.project.ID)
	if err == nil {
		t.Fatal("a blocked team was started anyway")
	}

	var notReady *ErrNotReady
	if !errors.As(err, &notReady) {
		t.Fatalf("got %v, want ErrNotReady carrying the report", err)
	}
	// The caller needs the detail, not merely a refusal.
	if notReady.Report == nil || len(notReady.Report.Roles) == 0 {
		t.Fatal("the refusal came without a readiness report")
	}
	var sawRemedy bool
	for _, role := range notReady.Report.Roles {
		for _, c := range role.Checks {
			if c.Remedy != "" {
				sawRemedy = true
			}
		}
	}
	if !sawRemedy {
		t.Error("a blocked start must say how to fix it")
	}
	if h.over.Running(h.project.ID) {
		t.Error("the project is marked running after a refused start")
	}
	if n := h.script.spawnCount(); n != 0 {
		t.Errorf("spawned %d agents despite the gate", n)
	}
}

// ── lifecycle ─────────────────────────────────────────────────────────────

func TestStartSpawnsEveryEnabledRoleAndStopIsClean(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !h.over.Running(h.project.ID) {
		t.Fatal("the project is not marked running")
	}

	waitFor(t, func() bool {
		st, err := h.over.Status(ctx, h.project.ID)
		if err != nil || len(st) != 2 {
			return false
		}
		return st[0].State == cerebrate.StateReady && st[1].State == cerebrate.StateReady
	}, 15*time.Second, "both roles never became ready")

	st, _ := h.over.Status(ctx, h.project.ID)
	if st[0].Role != "coder" || st[1].Role != "reviewer" {
		t.Errorf("roles are %s, %s; want the pipeline order", st[0].Role, st[1].Role)
	}
	if !st[1].Terminal {
		t.Error("the last enabled role must be reported as terminal")
	}

	if err := h.over.Start(ctx, h.project.ID); err == nil {
		t.Error("starting an already-running project was allowed")
	}

	if err := h.over.Stop(ctx, h.project.ID, "test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h.over.Running(h.project.ID) {
		t.Error("still marked running after Stop")
	}

	sessions, err := h.db.ListSessions(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].EndedAt == nil {
		t.Errorf("the session was not closed: %+v", sessions)
	}
}

// Each role gets its own worktree, so two agents cannot overwrite each other.
func TestStartPreparesAWorktreePerRole(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})
	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, role := range []string{"coder", "reviewer"} {
		path := filepath.Join(h.project.Path, ".worktrees", role)
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			t.Errorf("%s has no worktree: %v", role, err)
		}
	}
}

// The composed prompt is built fresh from the database at every spawn, so an
// edit is live on restart and no stale copy can survive anywhere.
func TestPromptsAreComposedFromTheDatabase(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.db.SetSetting(ctx, store.SettingSharedInstructions, "SHARED-MARKER"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	tpl, err := h.db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatalf("GetTemplateByName: %v", err)
	}
	tpl.Prompt = "ROLE-MARKER"
	if err := h.db.UpdateTemplate(ctx, tpl); err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return h.script.prompt(0) != "" }, 10*time.Second,
		"no composed prompt was written")

	var found bool
	for i := 0; i < 2; i++ {
		p := h.script.prompt(i)
		if strings.Contains(p, "SHARED-MARKER") && strings.Contains(p, "ROLE-MARKER") {
			found = true
		}
	}
	if !found {
		t.Error("the composed prompt did not carry both the shared instructions and the role's own")
	}
}

// An idle agent with work waiting gets told to claim it. A busy one is left
// alone: the queue is durable, so a skipped nudge costs nothing.
func TestIdleAgentsAreNudgedWhenWorkArrives(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})
	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool {
		st, err := h.over.Status(ctx, h.project.ID)
		return err == nil && len(st) > 0 && st[0].State == cerebrate.StateReady
	}, 15*time.Second, "coder never became ready")

	before := h.script.turns()
	if _, err := h.nyd.NewTask(ctx, h.project.ID, "Calculator", "x", "", nil); err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	waitFor(t, func() bool { return h.script.turns() > before }, 10*time.Second,
		"an idle agent with queued work was never nudged")
}

func TestStopOnAProjectThatIsNotRunning(t *testing.T) {
	h := newHarness(t, &scriptedHarness{script: idleAgent})
	if err := h.over.Stop(context.Background(), h.project.ID, "test"); err == nil {
		t.Error("stopping a stopped project was accepted")
	}
}

// Start checks whether a project is running, then unlocks and spends seconds in
// preflight, worktree preparation and spawning before registering the swarm.
// Without a reservation both callers pass the check and both bring a swarm up,
// leaving two sets of agents drawing from one queue.
func TestConcurrentStartsBringUpOneSwarm(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	const callers = 4
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- h.over.Start(ctx, h.project.ID)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	// Exactly one wins, and — the part that matters — the losers are refused
	// for being late rather than failing incidentally. Without a reservation
	// all four proceed and three die colliding on git init against the same
	// repository, which also leaves one winner and would pass a bare count.
	won := 0
	for err := range errs {
		if err == nil {
			won++
			continue
		}
		if !strings.Contains(err.Error(), "already") {
			t.Errorf("a losing Start failed by collision rather than refusal: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d of %d Starts succeeded, want exactly 1", won, callers)
	}

	// And exactly one set of agents is up.
	waitFor(t, func() bool {
		st, err := h.over.Status(ctx, h.project.ID)
		return err == nil && len(st) == 2 &&
			st[0].State == cerebrate.StateReady && st[1].State == cerebrate.StateReady
	}, 40*time.Second, "the surviving swarm never became ready")

	if err := h.over.Stop(ctx, h.project.ID, "test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h.over.Running(h.project.ID) {
		t.Error("still running after Stop, so a second swarm was left behind")
	}
}

// Stopping a swarm kills its agents, so their claims are dead too. Left to
// lapse on their own the work sits claimed for up to the full lease period, and
// a swarm started again in the meantime stands idle beside a card that says it
// is being worked on — which is exactly what a stop-and-start looked like.
func TestStoppingReturnsInFlightWorkToTheQueue(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	task, err := h.nyd.NewTask(ctx, h.project.ID, "Held", "do it", "", nil)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	// The first role claims it, and never acknowledges — an agent killed
	// mid-turn.
	lease, err := h.nyd.Claim(ctx, h.project.ID, "coder")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v (lease %v)", err, lease)
	}
	if n, err := h.db.QueuedCount(ctx, h.project.ID, "coder"); err != nil || n != 0 {
		t.Fatalf("queued before stop = %d (err %v), want 0 because it is claimed", n, err)
	}

	if err := h.over.Stop(ctx, h.project.ID, "test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Back in the queue immediately, not in twenty minutes.
	n, err := h.db.QueuedCount(ctx, h.project.ID, "coder")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("queued after stop = %d, want 1, since the work is still held by a dead agent", n)
	}
	_ = task
}

// ── live team edits ───────────────────────────────────────────────────────

// A team edit reaches the running swarm.
//
// It used to reach only whichever roles happened to respawn: a role added to
// the pipeline got no process at all, so work routed to it queued behind
// nothing, and a role taken out kept working until it next crashed.
func TestReconcileFollowsATeamEdit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return len(h.live(t)) == 2 }, 15*time.Second,
		"the swarm never came up with both roles")

	// Add a third role and drop the reviewer, in one edit.
	tpls, err := h.db.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	byName := map[string]string{}
	for _, tpl := range tpls {
		byName[tpl.Name] = tpl.ID
	}
	if err := h.db.SetTeam(ctx, h.project.ID, []store.TeamPresetRole{
		{TemplateID: byName["coder"], Enabled: true},
		{TemplateID: byName["cleaner"], Enabled: true},
	}); err != nil {
		t.Fatalf("SetTeam: %v", err)
	}
	if err := h.over.Reconcile(ctx, h.project.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	live := h.live(t)
	if _, ok := live["cleaner"]; !ok {
		t.Error("the role added to the team has no process")
	}
	if _, ok := live["reviewer"]; ok {
		t.Error("the role removed from the team is still supervised")
	}
	if _, ok := live["coder"]; !ok {
		t.Error("an untouched role was disturbed by the edit")
	}
}

// Reconciling a stopped project is a no-op rather than an error: the next
// Start reads the team as it now is.
func TestReconcileOnAStoppedProjectDoesNothing(t *testing.T) {
	h := newHarness(t, &scriptedHarness{script: idleAgent})
	if err := h.over.Reconcile(context.Background(), h.project.ID); err != nil {
		t.Errorf("Reconcile on a stopped project: %v", err)
	}
}

// Stop must not return while agents are still exiting.
//
// It used to wait on the goroutine that ticks the queue, which returns the
// instant the context is cancelled — so Stop reported the swarm down, reclaimed
// its leases and returned while harnesses were still writing, and a Start
// straight afterwards put a second agent into a worktree the first had not left.
func TestStopWaitsForAgentsToExit(t *testing.T) {
	ctx := context.Background()
	// Each agent writes its own pid as it starts; when Stop returns, none of
	// those processes may still exist.
	//
	// This used to be inferred from shell traps, and that premise does not
	// hold: bash 3.2 waiting on `sleep` and signalled through its process
	// group may run a TERM handler, an EXIT handler, both, or neither, so the
	// mark count measured the shell rather than the daemon. Under the race
	// detector it failed 19 runs in 20 against a daemon doing exactly what
	// this test says it should. Whether a pid is still alive is a fact the
	// kernel keeps, and it is also the thing that actually went wrong: a
	// second agent entered a worktree the first had not left.
	// The agent ignores SIGTERM, so "still exiting" is a state that lasts long
	// enough to observe: cerebrate's WaitDelay SIGKILLs it after its shutdown
	// grace, and a Stop that returned early would return inside that window.
	// An agent that dies on the first signal is gone in microseconds, and a
	// Stop that waited for nothing would pass.
	//
	// `trap "" TERM` rather than a handler: ignoring a signal is set once, at
	// registration, so it does not depend on when or whether the shell gets
	// around to dispatching a trap.
	pids := filepath.Join(t.TempDir(), "pids")
	h := newHarness(t, &scriptedHarness{script: func(adapter.Spec) string {
		return `trap '' TERM
printf '%d\n' $$ >> ` + pids + `
printf 'ready\n'
sleep 30`
	}})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Both processes running, not both roleProcs registered. The map is
	// written at spawn and the shell runs a moment later, so waiting on the
	// map let Stop arrive before the second agent had started: there was
	// nothing to wait for, and the test passed for the wrong reason.
	waitFor(t, func() bool { return len(recordedPids(pids)) == 2 }, 15*time.Second,
		"both agents never started")

	if err := h.over.Stop(ctx, h.project.ID, "test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Read immediately: the point is that Stop has already waited.
	spawned := recordedPids(pids)
	if len(spawned) != 2 {
		t.Fatalf("%d agent(s) recorded a pid, want 2", len(spawned))
	}
	for _, field := range spawned {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("an agent wrote %q where a pid was expected: %v", field, err)
		}
		// Signal 0 checks for existence without delivering anything. Every
		// agent is reaped by cerebrate's Wait before Run returns, so a live
		// pid here is a process Stop did not wait for rather than a zombie.
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("agent %d was still running when Stop returned", pid)
		}
	}
}

// recordedPids is every agent that has reached the point of writing its own
// pid. A missing file is none of them yet, which is what the wait is for.
func recordedPids(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// live is the set of roles the swarm is actually supervising right now.
func (h *harness) live(t *testing.T) map[string]bool {
	t.Helper()
	h.over.mu.Lock()
	s, ok := h.over.running[h.project.ID]
	h.over.mu.Unlock()
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for name := range s.snapshot() {
		out[name] = true
	}
	return out
}

// ── the architect sidecar ─────────────────────────────────────────────────

// A supervised card summons the sidecar, and what the cockpit reads is whether
// a process is actually running.
//
// The first cut used the pointer as liveness and never cleared it, so a sidecar
// that had stopped read as one that was working: no replacement, nothing
// reported, and a card waiting on a decider that was gone. The badge was drawn
// from the card's request rather than from this, which is the same mistake one
// layer up.
func TestTheArchitectsStateIsWhetherAProcessIsRunning(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := h.over.SupervisorState(ctx, h.project.ID); st.Wanted || st.Live {
		t.Fatalf("state = %+v with no supervised card, want nothing wanted", st)
	}

	task, err := h.nyd.NewTaskWith(ctx, nydus.NewTaskOpts{
		ProjectID: h.project.ID, Name: "Spec", Body: "write it", Supervised: true,
	})
	if err != nil {
		t.Fatalf("NewTaskWith: %v", err)
	}
	if err := h.over.SyncSupervisor(ctx, h.project.ID); err != nil {
		t.Fatalf("SyncSupervisor: %v", err)
	}
	st := h.over.SupervisorState(ctx, h.project.ID)
	if !st.Wanted || !st.Live || st.Role != "supervisor" || st.Error != "" {
		t.Fatalf("state = %+v, want a live supervisor", st)
	}

	// The process ends on its own. The pointer has to go with it, or the next
	// sync sees a live sidecar that is not there.
	s := h.swarm(t)
	s.mu.Lock()
	p := s.supervisor
	s.mu.Unlock()
	p.cancel()
	waitFor(t, func() bool {
		return !h.over.SupervisorState(ctx, h.project.ID).Live
	}, 5*time.Second, "the architect's pointer outlived its process")

	// A cancelled sidecar is not a failure, so the next sync starts a
	// replacement rather than reporting one.
	if err := h.over.SyncSupervisor(ctx, h.project.ID); err != nil {
		t.Fatalf("SyncSupervisor after the process ended: %v", err)
	}
	if st := h.over.SupervisorState(ctx, h.project.ID); !st.Live {
		t.Errorf("state = %+v, want a replacement after the process ended", st)
	}

	// Finishing the card retires it.
	if err := h.db.SetTaskSupervised(ctx, task.ID, false); err != nil {
		t.Fatalf("SetTaskSupervised: %v", err)
	}
	if err := h.over.SyncSupervisor(ctx, h.project.ID); err != nil {
		t.Fatalf("SyncSupervisor: %v", err)
	}
	if st := h.over.SupervisorState(ctx, h.project.ID); st.Wanted || st.Live {
		t.Errorf("state = %+v, want the architect retired", st)
	}
}

// A card that asks for an architect the library cannot provide says so, rather
// than looking like a card an architect is deciding.
func TestASupervisedCardWithNoSupervisorRoleReportsItself(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	// The operator deleted the role, or never had one.
	tpl, err := h.db.RoleFor(ctx, store.PurposeSupervisor)
	if err != nil {
		t.Fatalf("RoleFor: %v", err)
	}
	if err := h.db.DeleteTemplate(ctx, tpl.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.nyd.NewTaskWith(ctx, nydus.NewTaskOpts{
		ProjectID: h.project.ID, Name: "Spec", Body: "write it", Supervised: true,
	}); err != nil {
		t.Fatalf("NewTaskWith: %v", err)
	}
	if err := h.over.SyncSupervisor(ctx, h.project.ID); err != nil {
		t.Fatalf("SyncSupervisor: %v", err)
	}

	st := h.over.SupervisorState(ctx, h.project.ID)
	if !st.Wanted {
		t.Fatalf("state = %+v, want the card's request to be visible", st)
	}
	if st.Live {
		t.Fatalf("state = %+v, want no process", st)
	}
	if !strings.Contains(st.Error, "supervisor purpose") {
		t.Errorf("error = %q, want it to name what the operator has to add", st.Error)
	}
}

// A harness change replaces the process rather than refreshing it.
//
// Refresh re-reads the prompt and the flags, and cannot swap the adapter the
// process was built with: the sidecar went on running the old harness with the
// new one's arguments. Reconcile has handled this for pipeline roles since the
// team editor shipped; the sidecar is not on the team and was left out.
func TestChangingTheArchitectsHarnessReplacesTheProcess(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	other := &scriptedHarness{script: idleAgent, harnessName: "scripted-two"}
	h.over.registry.Register(other)

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.nyd.NewTaskWith(ctx, nydus.NewTaskOpts{
		ProjectID: h.project.ID, Name: "Spec", Body: "write it", Supervised: true,
	}); err != nil {
		t.Fatalf("NewTaskWith: %v", err)
	}
	if err := h.over.SyncSupervisor(ctx, h.project.ID); err != nil {
		t.Fatalf("SyncSupervisor: %v", err)
	}
	before := h.swarm(t)
	before.mu.Lock()
	first := before.supervisor
	before.mu.Unlock()
	if first == nil || first.harness != "scripted" {
		t.Fatalf("supervisor harness = %+v, want scripted", first)
	}

	tpl, err := h.db.RoleFor(ctx, store.PurposeSupervisor)
	if err != nil {
		t.Fatalf("RoleFor: %v", err)
	}
	tpl.Harness = "scripted-two"
	if err := h.db.UpdateTemplate(ctx, tpl); err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if err := h.over.SyncSupervisor(ctx, h.project.ID); err != nil {
		t.Fatalf("SyncSupervisor after the harness change: %v", err)
	}

	s := h.swarm(t)
	s.mu.Lock()
	second := s.supervisor
	s.mu.Unlock()
	if second == nil {
		t.Fatal("the architect was stopped and not replaced")
	}
	if second == first {
		t.Fatal("the same process is still running after the harness changed")
	}
	if second.harness != "scripted-two" {
		t.Errorf("harness = %q, want scripted-two", second.harness)
	}
	waitFor(t, func() bool { return other.spawnCount() > 0 },
		5*time.Second, "the new harness was never asked to spawn anything")
}

// swarm reaches the running swarm, for the assertions that are about the
// process table rather than about what the API reports.
func (h *harness) swarm(t *testing.T) *swarm {
	t.Helper()
	h.over.mu.Lock()
	defer h.over.mu.Unlock()
	s, ok := h.over.running[h.project.ID]
	if !ok {
		t.Fatal("the project is not running")
	}
	return s
}

// The swarm's own tick never waits for the lifecycle lock.
//
// keepMoving is counted in the swarm's `live` group, and Stop waits on that
// group while holding that lock. A tick that blocked acquiring it could not
// finish, so Stop could not either, and shutdown fell through to the
// ten-second grace timeout whenever the two overlapped -- which is every stop
// that lands mid-tick. Skipping one tick's housekeeping costs two seconds; the
// next tick does it.
func TestTheTickDoesNotWaitForTheLifecycleLock(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.nyd.NewTaskWith(ctx, nydus.NewTaskOpts{
		ProjectID: h.project.ID, Name: "Spec", Body: "write it", Supervised: true,
	}); err != nil {
		t.Fatalf("NewTaskWith: %v", err)
	}

	// Stop is holding it while it waits for the processes; this stands in for
	// that, without the race of trying to catch the real one mid-wait.
	unlock := h.over.lock(h.project.ID)
	defer unlock()

	done := make(chan error, 1)
	go func() { done <- h.over.syncSupervisorTick(ctx, h.project.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the tick returned %v, want it to skip quietly", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the tick blocked on the lifecycle lock, which is what makes shutdown wait out its grace period")
	}
}

// What a role is running is read off the process, not off the library.
//
// A decision records the model that took it, and the two answers part company
// the moment somebody edits a role: reading it back off current configuration
// would quietly rewrite what an old approval was made by. The sidecar is
// included because it is the one whose decisions this is mostly about, and it
// is deliberately not in the roles map.
func TestRunningRoleReportsTheProcessRatherThanTheLibrary(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &scriptedHarness{script: idleAgent})

	if err := h.over.Start(ctx, h.project.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.nyd.NewTaskWith(ctx, nydus.NewTaskOpts{
		ProjectID: h.project.ID, Name: "Spec", Body: "write it", Supervised: true,
	}); err != nil {
		t.Fatalf("NewTaskWith: %v", err)
	}
	if err := h.over.SyncSupervisor(ctx, h.project.ID); err != nil {
		t.Fatalf("SyncSupervisor: %v", err)
	}

	for _, role := range []string{"coder", "supervisor"} {
		harness, model, ok := h.over.RunningRole(h.project.ID, role)
		if !ok {
			t.Errorf("%s is running and RunningRole did not report it", role)
			continue
		}
		if harness != "scripted" || model == "" {
			t.Errorf("%s reported %s/%q, want the scripted harness and a model", role, harness, model)
		}
	}

	// The library moves; the process does not. A decision taken now must
	// record what is running now.
	tpl, err := h.db.RoleFor(ctx, store.PurposeSupervisor)
	if err != nil {
		t.Fatalf("RoleFor: %v", err)
	}
	was := tpl.Model
	tpl.Model = "some-other-model"
	if err := h.db.UpdateTemplate(ctx, tpl); err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if _, model, _ := h.over.RunningRole(h.project.ID, "supervisor"); model != was {
		t.Errorf("model = %q after a library edit, want the %q the process is still running",
			model, was)
	}

	// A role nobody is running has no answer, rather than a plausible one.
	if _, _, ok := h.over.RunningRole(h.project.ID, "nobody"); ok {
		t.Error("RunningRole answered for a role that is not running")
	}
	if _, _, ok := h.over.RunningRole("no-such-project", "coder"); ok {
		t.Error("RunningRole answered for a project that is not running")
	}
}

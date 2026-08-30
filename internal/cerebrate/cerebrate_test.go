package cerebrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/store"
)

// scriptedAdapter runs a shell script instead of a harness, so supervision —
// spawning, crashing, backing off, going fatal — is exercised end to end
// against real processes without a single token being spent.
type scriptedAdapter struct {
	script string

	mu     sync.Mutex
	spawns int
	turns  []string
}

func (s *scriptedAdapter) Name() string            { return "scripted" }
func (s *scriptedAdapter) Checks() []adapter.Check { return nil }
func (s *scriptedAdapter) Capabilities() adapter.Caps {
	return adapter.Caps{StructuredOutput: true, StructuredInput: true}
}
func (s *scriptedAdapter) ListModels(adapter.Ctx) ([]adapter.Model, error) { return nil, nil }

func (s *scriptedAdapter) Command(ctx context.Context, spec adapter.Spec) (*exec.Cmd, error) {
	s.mu.Lock()
	s.spawns++
	s.mu.Unlock()
	cmd := exec.CommandContext(ctx, "sh", "-c", s.script)
	cmd.Dir = spec.Worktree
	return cmd, nil
}

func (s *scriptedAdapter) Spawns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawns
}

// Parse reads the tiny line protocol the scripts emit: "kind[:text]".
func (s *scriptedAdapter) Parse(line []byte) ([]adapter.Event, error) {
	text := strings.TrimSpace(string(line))
	if text == "" {
		return nil, nil
	}
	kind, rest, _ := strings.Cut(text, ":")
	switch kind {
	case "ready":
		return []adapter.Event{{Kind: adapter.EventReady}}, nil
	case "message":
		return []adapter.Event{{Kind: adapter.EventMessage, Text: rest}}, nil
	case "turn_end":
		return []adapter.Event{{Kind: adapter.EventTurnEnd}}, nil
	case "fatal":
		return []adapter.Event{{Kind: adapter.EventError, Text: rest, Fatal: true}}, nil
	case "error":
		return []adapter.Event{{Kind: adapter.EventError, Text: rest}}, nil
	case "garbage":
		return nil, errors.New("unparseable line")
	default:
		return nil, nil
	}
}

func (s *scriptedAdapter) EncodeTurn(text string) ([]byte, error) {
	b, err := json.Marshal(map[string]string{"turn": text})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (s *scriptedAdapter) EncodeInterrupt() ([]byte, error) {
	return []byte("{\"interrupt\":true}\n"), nil
}

type blockingPreflight struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (b *blockingPreflight) CheckRole(context.Context, adapter.Spec, adapter.Adapter) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.err
}

func (b *blockingPreflight) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *blockingPreflight) allow() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = nil
}

func newCerebrate(t *testing.T, a adapter.Adapter, opts ...func(*Config)) (*Cerebrate, *event.Bus) {
	t.Helper()
	bus := event.NewBus()
	cfg := Config{
		ProjectID:    "P1",
		Role:         store.ResolvedRole{RoleTemplate: store.RoleTemplate{Name: "coder", Model: "test"}, Enabled: true},
		Adapter:      a,
		Worktree:     t.TempDir(),
		StateDir:     t.TempDir(),
		SystemPrompt: "be useful",
		Bus:          bus,
		backoffBase:  10 * time.Millisecond, // keep the tests quick
	}
	for _, o := range opts {
		o(&cfg)
	}
	return New(cfg), bus
}

// collect drains events until want is satisfied or the deadline passes.
func collect(t *testing.T, bus *event.Bus, until func([]event.Event) bool, timeout time.Duration) []event.Event {
	t.Helper()
	ch, cancel := bus.Subscribe(256)
	defer cancel()

	var got []event.Event
	deadline := time.After(timeout)
	for {
		if until(got) {
			return got
		}
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
}

func kinds(evs []event.Event) []adapter.EventKind {
	out := make([]adapter.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestPublishesEventsAndReachesReady(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\nmessage:working\nturn_end\n'; sleep 5`}
	c, bus := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	evs := collect(t, bus, func(e []event.Event) bool { return len(e) >= 3 }, 5*time.Second)
	if len(evs) < 3 {
		t.Fatalf("got %v, want ready/message/turn_end", kinds(evs))
	}
	if evs[0].Kind != adapter.EventReady {
		t.Errorf("first event is %s, want ready", evs[0].Kind)
	}
	// Events carry where they came from, so the cockpit can filter by role.
	if evs[0].Role != "coder" || evs[0].ProjectID != "P1" {
		t.Errorf("event lost its origin: role=%q project=%q", evs[0].Role, evs[0].ProjectID)
	}
	if evs[0].ID == "" {
		t.Error("events need ids to be replayable")
	}

	waitFor(t, func() bool { return c.State() == StateReady }, 15*time.Second,
		"cerebrate never reached ready")
}

// A process that exits must be restarted — an agent dying should not silently
// remove a role from the pipeline.
func TestRestartsAfterExit(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\n'; exit 1`}
	c, _ := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, func() bool { return a.Spawns() >= 3 }, 15*time.Second,
		"the agent was not restarted after exiting")
	if c.Restarts() == 0 {
		t.Error("restarts were not counted")
	}
}

// Backoff must grow, or a crash loop burns tokens and rate limits faster than
// anyone can notice it.
func TestBackoffGrowsBetweenRestarts(t *testing.T) {
	a := &scriptedAdapter{script: `exit 1`}
	c, _ := newCerebrate(t, a, func(cfg *Config) {
		cfg.backoffBase = 60 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	go c.Run(ctx)
	waitFor(t, func() bool { return a.Spawns() >= 4 }, 24*time.Second,
		"the agent did not restart enough times to observe backoff")
	elapsed := time.Since(start)

	// 60 + 120 + 240 = 420ms of waiting before the fourth spawn. Immediate
	// retries would arrive far sooner.
	if elapsed < 350*time.Millisecond {
		t.Errorf("four spawns took %s; backoff does not appear to be growing", elapsed)
	}
}

// A fatal error is one a restart cannot fix. Respawning into it burns twenty
// minutes looking busy.
func TestFatalErrorStopsSupervision(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\nfatal:model requires a newer version\n'; exit 1`}
	c, _ := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a fatal error")
	}

	if c.State() != StateFailed {
		t.Errorf("state = %s, want failed", c.State())
	}
	if !strings.Contains(c.LastError(), "newer version") {
		t.Errorf("last error = %q; a failed role must say why", c.LastError())
	}
	if s := a.Spawns(); s != 1 {
		t.Errorf("spawned %d times after a fatal error, want 1", s)
	}
}

// Preflight runs before every spawn, not only at Start: tokens expire and
// binaries get upgraded between one task and the next.
func TestBlockedPreflightPreventsSpawnAndRecovers(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\n'; sleep 5`}
	pf := &blockingPreflight{err: errors.New("claude is not on PATH")}
	c, _ := newCerebrate(t, a, func(cfg *Config) { cfg.Preflight = pf })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Generous on purpose: waitFor polls and returns the moment the condition
	// holds, so a wide bound costs nothing when things work and only matters
	// on a machine already running several race-instrumented packages.
	waitFor(t, func() bool { return c.State() == StateBlocked }, 40*time.Second,
		"a blocked preflight did not block the role")
	if a.Spawns() != 0 {
		t.Errorf("spawned %d times despite a blocked preflight, want 0", a.Spawns())
	}
	if !strings.Contains(c.LastError(), "not on PATH") {
		t.Errorf("last error = %q; a blocked role must carry the reason", c.LastError())
	}

	// Blocked is not fatal. Fixing the cause should be picked up without a
	// manual restart.
	pf.allow()
	waitFor(t, func() bool { return c.State() == StateReady }, 15*time.Second,
		"the role did not recover once preflight passed")
}

// Submitting a turn is a write to a pipe, not a keystroke injected into a
// screen — and a closed pipe is an error the caller can see.
func TestSubmitWritesTheEncodedTurn(t *testing.T) {
	out := filepath.Join(t.TempDir(), "stdin.log")
	a := &scriptedAdapter{script: fmt.Sprintf(
		`printf 'ready\n'; while IFS= read -r line; do printf '%%s\n' "$line" >> %q; done`, out)}
	c, _ := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := c.Submit("do the thing"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	waitFor(t, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && strings.Contains(string(b), "do the thing")
	}, 15*time.Second, "the submitted turn never reached the agent's stdin")

	b, _ := os.ReadFile(out)
	// Encoding belongs to the adapter, because the harnesses disagree entirely.
	var decoded map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &decoded); err != nil {
		t.Fatalf("stdin was not the adapter's encoding: %q", b)
	}
	if decoded["turn"] != "do the thing" {
		t.Errorf("decoded %v, want the submitted text", decoded)
	}
}

func TestSubmitFailsWhenNotRunning(t *testing.T) {
	a := &scriptedAdapter{script: `sleep 0`}
	c, _ := newCerebrate(t, a)
	if err := c.Submit("hello"); err == nil {
		t.Fatal("submitting to a stopped agent was accepted")
	}
}

// Harnesses print things adapters have not seen. An unparseable line is worth
// logging, but killing a working agent over it would be a bad trade.
func TestUnparseableOutputDoesNotKillTheAgent(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\ngarbage\nmessage:still here\n'; sleep 5`}
	c, bus := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	evs := collect(t, bus, func(e []event.Event) bool {
		for _, ev := range e {
			if ev.Kind == adapter.EventMessage && ev.Text == "still here" {
				return true
			}
		}
		return false
	}, 5*time.Second)

	var found bool
	for _, ev := range evs {
		if ev.Kind == adapter.EventMessage && ev.Text == "still here" {
			found = true
		}
	}
	if !found {
		t.Errorf("output after an unparseable line was lost: %v", kinds(evs))
	}
}

// Agent output carries whole files and diffs, so a 64KB line is ordinary.
// Truncating one would surface as a parse failure and look like a harness bug.
func TestLongLinesAreNotTruncated(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\nmessage:'; head -c 200000 < /dev/zero | tr '\0' 'x'; printf '\n'; sleep 5`}
	c, bus := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	evs := collect(t, bus, func(e []event.Event) bool {
		for _, ev := range e {
			if ev.Kind == adapter.EventMessage && len(ev.Text) > 100000 {
				return true
			}
		}
		return false
	}, 6*time.Second)

	var longest int
	for _, ev := range evs {
		if ev.Kind == adapter.EventMessage && len(ev.Text) > longest {
			longest = len(ev.Text)
		}
	}
	if longest < 200000 {
		t.Errorf("longest message was %d bytes, want the full 200000", longest)
	}
}

func TestCancellingStopsCleanly(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\n'; sleep 30`}
	c, _ := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitFor(t, func() bool { return c.State() == StateReady }, 15*time.Second, "never became ready")
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if c.State() != StateStopped {
		t.Errorf("state = %s, want stopped", c.State())
	}
}

// The composed prompt is written fresh on every spawn and never copied into the
// worktree, so an edit is live on restart and no stale snapshot can survive.
func TestSystemPromptIsWrittenOutsideTheWorktree(t *testing.T) {
	a := &scriptedAdapter{script: `printf 'ready\n'; sleep 3`}
	stateDir := t.TempDir()
	worktree := t.TempDir()
	c, _ := newCerebrate(t, a, func(cfg *Config) {
		cfg.StateDir = stateDir
		cfg.Worktree = worktree
		cfg.SystemPrompt = "shared instructions\n\nrole prompt"
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	path := filepath.Join(stateDir, "coder.system.md")
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil }, 15*time.Second,
		"the composed prompt was never written")

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the composed prompt: %v", err)
	}
	if !strings.Contains(string(body), "role prompt") {
		t.Errorf("composed prompt is %q", body)
	}
	if entries, _ := os.ReadDir(worktree); len(entries) != 0 {
		t.Error("something was copied into the worktree; that is how config goes stale")
	}
}

// waitFor polls until a condition holds, and gives up generously.
//
// The bound is wall-clock, and these tests share a machine that may be running
// several agent processes. A tight bound turns "the laptop was busy" into a
// test failure, which teaches people to re-run rather than to read — and the
// generous bound costs nothing when the condition holds, because this returns
// the instant it does.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// ARCHITECTURE §4.4 says configuration is resolved at every spawn. It was
// resolved once, when the swarm started, so a role that crashed came back with
// the prompt and model it had when the swarm went up — silently, and only for
// the roles that happened to crash. That is the config-snapshot failure §6
// records, in a slower form.
func TestARespawnPicksUpEditedConfiguration(t *testing.T) {
	// Exits immediately, so it respawns.
	a := &scriptedAdapter{script: `exit 1`}

	var mu sync.Mutex
	prompt := "first"
	seen := []string{}

	c, _ := newCerebrate(t, a, func(cfg *Config) {
		cfg.SystemPrompt = "first"
		cfg.Refresh = func(context.Context) (Refreshed, error) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, prompt)
			return Refreshed{Role: cfg.Role, SystemPrompt: prompt}, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= 1
	}, 20*time.Second, "the role never spawned")

	// The operator edits the prompt while it is running.
	mu.Lock()
	prompt = "second"
	mu.Unlock()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range seen {
			if p == "second" {
				return true
			}
		}
		return false
	}, 30*time.Second, "a respawn reused the configuration from start-up")
}

// A role removed from the team must not come back on the next respawn.
func TestARoleRemovedFromTheTeamStopsInsteadOfRespawning(t *testing.T) {
	a := &scriptedAdapter{script: `exit 1`}
	c, _ := newCerebrate(t, a, func(cfg *Config) {
		cfg.Refresh = func(context.Context) (Refreshed, error) {
			return Refreshed{Gone: true}, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, func() bool { return c.State() == StateFailed }, 20*time.Second,
		"a role removed from the team kept respawning")
	if a.Spawns() != 0 {
		t.Errorf("spawned %d times after removal, want 0", a.Spawns())
	}
}

// An agent that is mid-turn and producing nothing is distinguishable from one
// that is working.
//
// Nothing could tell them apart. A reviewer ran a headless browser against a
// dev server whose hot-reload socket never idles, the screenshot never
// returned, and the role sat in "working" holding a lease with an empty
// transcript until a person happened to look. Silence is only measured inside
// a turn: an idle agent is quiet for a good reason.
func TestSilenceIsMeasuredOnlyWhileMidTurn(t *testing.T) {
	now := time.Date(2026, 8, 28, 23, 0, 0, 0, time.UTC)
	c, _ := newCerebrate(t, &scriptedAdapter{}, func(cfg *Config) {
		cfg.clock = func() time.Time { return now }
	})

	// Nothing said yet, and not in a turn: no claim either way.
	if got := c.Silence(); got != 0 {
		t.Errorf("silence before anything happened = %s, want 0", got)
	}

	c.setBusy(true)
	c.publish(adapter.Event{Kind: adapter.EventMessage, Text: "starting"})
	if got := c.Silence(); got != 0 {
		t.Errorf("silence right after speaking = %s, want 0", got)
	}

	// Eight minutes of nothing, mid-turn.
	now = now.Add(8 * time.Minute)
	if got := c.Silence(); got != 8*time.Minute {
		t.Errorf("silence = %s, want 8m", got)
	}

	// Anything at all resets it, including a tool call that produces no text.
	c.publish(adapter.Event{Kind: adapter.EventToolCall, Tool: "bash"})
	if got := c.Silence(); got != 0 {
		t.Errorf("silence after a tool call = %s, want 0", got)
	}

	// And an agent between turns is not silent, it is finished.
	now = now.Add(time.Hour)
	c.setBusy(false)
	if got := c.Silence(); got != 0 {
		t.Errorf("an idle agent reported %s of silence", got)
	}
}

// A turn stopped on request is not a failure.
//
// The harness reports an abandoned turn the way it reports a broken one: a
// result marked as an error with nothing to say about it. The adapter cannot
// tell those apart, since it did not ask for the stop, so pressing stop put
// "the harness reported an error without describing it" on screen underneath
// the answer somebody had just chosen to cut short.
func TestStoppingAnAnswerDoesNotReadAsAFailure(t *testing.T) {
	c, _ := newCerebrate(t, &scriptedAdapter{})

	// The state a stop leaves behind: asked for, not yet answered.
	c.mu.Lock()
	c.interrupting = true
	c.mu.Unlock()

	if !c.tookInterrupt() {
		t.Fatal("the interrupt was not remembered")
	}
	// One interrupt ends one turn: a genuine error after it is still an error.
	if c.tookInterrupt() {
		t.Error("the interrupt was still set for a second event")
	}
}

// Nothing to stop is not a failure either: the answer arrived while the button
// was being pressed, which is the common case for a short turn.
func TestInterruptingAnIdleAgentIsQuiet(t *testing.T) {
	c, _ := newCerebrate(t, &scriptedAdapter{})
	c.mu.Lock()
	c.stdin = nopWriteCloser{}
	c.busy = false
	c.mu.Unlock()

	if err := c.Interrupt(); err != nil {
		t.Errorf("interrupting an idle agent: %v", err)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.interrupting {
		t.Error("an idle agent was left waiting for a stop it never sent")
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// Package cerebrate supervises one agent process.
//
// It owns a role's lifecycle and nothing else: preflight, spawn, read events,
// submit turns, restart, stop. Routing is nydus's job.
//
// The supervision here answers a specific failure. tmux kept a session "alive"
// around a codex process that returned HTTP 400 on every turn for twenty
// minutes — its liveness check was "does a process exist", which is nearly
// uncorrelated with "is this agent working". A supervisor reading exit codes
// and typed error events can tell the difference.
package cerebrate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/store"
)

// State is what the cockpit shows for a role.
type State string

const (
	StateIdle     State = "idle"     // not started
	StateStarting State = "starting" // spawning; the process is not up yet
	StateReady    State = "ready"    // accepting turns
	StateWorking  State = "working"  // mid-turn
	StateBlocked  State = "blocked"  // preflight refused; needs a human
	StateFailed   State = "failed"   // fatal; will not restart
	// StateThrottled is a provider quota window that is spent. Distinct from
	// failed because it needs no one to do anything: it ends by itself.
	StateThrottled State = "throttled"
	StateStopped   State = "stopped" // shut down deliberately
)

const (
	// minBackoff through maxBackoff bound restart attempts. A crash loop that
	// retried instantly would burn tokens and rate limits faster than anyone
	// could notice it.
	minBackoff = time.Second
	maxBackoff = time.Minute

	// stableRun is how long a process must survive before its restart budget
	// resets. Without it, a process that crashes after five minutes each time
	// would eventually be treated as healthy.
	stableRun = 2 * time.Minute

	// shutdownGrace is how long a stopping agent has to exit and close its
	// pipes before the supervisor stops waiting on it.
	shutdownGrace = 5 * time.Second
)

// Preflighter is the readiness check run before each spawn. It is an interface
// so a test can refuse a spawn without installing a broken harness.
type Preflighter interface {
	CheckRole(ctx context.Context, spec adapter.Spec, a adapter.Adapter) error
}

// Config is everything a cerebrate needs. Every field originates in the
// database or is derived from it; nothing is read from a config file.
// Refreshed is what a role looks like in the database right now.
type Refreshed struct {
	Role         store.ResolvedRole
	SystemPrompt string
	HarnessFlags []string
	// Gone is true when the role is no longer part of the team. The supervisor
	// stops rather than respawning something the operator has removed.
	Gone bool
}

// liveConfig is the part of Config that changes while the role runs.
type liveConfig struct {
	role         store.ResolvedRole
	systemPrompt string
	harnessFlags []string
}

type Config struct {
	ProjectID string
	Role      store.ResolvedRole
	Adapter   adapter.Adapter
	Worktree  string
	ConfigDir string
	Socket    string
	Token     string

	// BinDir holds the zerg executable the agent is told to run.
	BinDir string

	// Env is extra variables for this agent, as NAME=value; see adapter.Spec.
	Env []string

	// HarnessFlags apply to every role on this harness, from settings.
	HarnessFlags []string

	// Streaming asks for the answer as it is written, for a session somebody
	// is watching. Chat sets it; the pipeline does not, because nothing reads
	// a role's output as it appears and the fragments are volume on the bus
	// for no reader.
	Streaming bool

	// SystemPrompt is composed from shared instructions plus the role prompt.
	// It is the value to use when Refresh is nil or fails.
	SystemPrompt string

	// Refresh re-reads this role from the database immediately before each
	// spawn.
	//
	// ARCHITECTURE §4.4 says configuration is resolved at every spawn, and
	// without this it was resolved once when the swarm started: a role that
	// crashed and respawned came back with the prompt, model and flags it had
	// an hour ago, silently, which is the same class of failure as the config
	// snapshot that once produced a Clojure calculator from a Rust spec.
	//
	// Nil leaves the Config values in place, which is what the tests use.
	Refresh func(context.Context) (Refreshed, error)

	Bus         *event.Bus
	Log         *slog.Logger
	Preflight   Preflighter
	StateDir    string // where the composed prompt is written
	clock       func() time.Time
	backoffBase time.Duration
}

// Cerebrate supervises one role's agent process.
type Cerebrate struct {
	cfg Config

	mu       sync.RWMutex
	state    State
	lastErr  string
	restarts int
	// live is the configuration as of the last spawn: the role, its composed
	// prompt and its harness flags.
	//
	// Separate from cfg and behind the mutex because it is rewritten on every
	// spawn by the run goroutine and read by others — the quota poller asks
	// which harness this role is on while a respawn is rewriting exactly that.
	// Two changes that were each correct alone: re-reading configuration per
	// spawn, and polling quota per harness.
	live liveConfig

	// session is this process's own adapter instance, so state latched from
	// one agent's stream cannot be read as another's. Replaced at every spawn.
	session adapter.Adapter

	// throttledUntil is when a spent provider quota is expected to lift.
	throttledUntil time.Time
	// lastStreamErr is the most recent error the agent's own output carried,
	// fatal or not. The process exit status rarely says why.
	lastStreamErr string

	// quota is the subscription's remaining headroom, as last reported.
	// Whether it arrived on the stream or was fetched does not matter here.
	quota     *adapter.Quota
	quotaSeen time.Time

	stdin io.WriteCloser
	ready chan struct{} // closed when the agent first reports ready

	// busy is true between submitting a turn and the agent finishing it.
	//
	// Readiness cannot be the gate for sending work. claude in stream-json
	// mode emits its init event only after it receives a first message, so
	// waiting for "ready" before submitting is a circular wait: no turn, no
	// init, no ready, no turn. What matters is whether the agent is mid-turn,
	// which is a thing the supervisor knows without being told.
	busy bool

	// interrupting is set between asking a harness to stop and it saying it
	// has. Exactly one event is affected: the error the harness reports for the
	// turn it just abandoned, which is not a fault and must not read as one.
	interrupting bool

	// spoke is when this agent last produced anything at all.
	//
	// A process that is up and a process that is working are different states,
	// and nothing here could tell them apart: an agent that ran a command which
	// never returned stayed "working" with a held lease and an empty transcript
	// until the lease expired twenty minutes later. A headless browser
	// screenshotting a dev server did exactly that, and it was noticed by a
	// person watching a board, which is not a mechanism.
	spoke time.Time
}

func New(cfg Config) *Cerebrate {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.clock == nil {
		cfg.clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.backoffBase == 0 {
		cfg.backoffBase = minBackoff
	}
	return &Cerebrate{
		cfg:   cfg,
		state: StateIdle,
		ready: make(chan struct{}),
		// Seeded from Config, then owned by the spawn loop.
		live: liveConfig{
			role:         cfg.Role,
			systemPrompt: cfg.SystemPrompt,
			harnessFlags: cfg.HarnessFlags,
		},
	}
}

func (c *Cerebrate) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// Silence is how long this agent has produced nothing while mid-turn.
//
// Zero when it is not in a turn, or has said something since the last check:
// an idle agent is quiet for a good reason and a working one is not. This is
// what the supervisor watches, because "working" on its own cannot distinguish
// a long build from a command that will never return.
func (c *Cerebrate) Silence() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.busy || c.spoke.IsZero() {
		return 0
	}
	return c.cfg.clock().Sub(c.spoke)
}

// LastError is why a role is blocked or failed, for the cockpit to show.
func (c *Cerebrate) LastError() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

// Restarts counts respawns since the last stable run.
// Harness names the CLI this role runs on. Quota is reported per harness,
// because that is what an account belongs to.
func (c *Cerebrate) Harness() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.live.role.Harness
}

// Role is the role as currently configured.
func (c *Cerebrate) Role() store.ResolvedRole {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.live.role
}

// name is the role's name, for logs and errors.
func (c *Cerebrate) name() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.live.role.Name
}

// Quota is the subscription headroom last reported for this role, and when it
// was learned. Nil until something reports it, which for an API-key provider
// is never.
func (c *Cerebrate) Quota() (*adapter.Quota, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.quota, c.quotaSeen
}

// mergeQuota takes the new percentages but keeps reset times the fetched
// source did not carry.
//
// claude has two sources for one fact: the stream event states exact reset
// stamps, and `/usage` states only percentages. A poll must not blank a stamp
// the stream already gave — the fresher number is not the more complete record.
func (c *Cerebrate) mergeQuota(q *adapter.Quota) {
	c.mu.Lock()
	prev := c.quota
	c.mu.Unlock()

	if prev != nil {
		known := map[time.Duration]time.Time{}
		for _, w := range prev.Windows {
			if !w.ResetsAt.IsZero() {
				known[w.Window] = w.ResetsAt
			}
		}
		for i, w := range q.Windows {
			if w.ResetsAt.IsZero() {
				if at, ok := known[w.Window]; ok {
					q.Windows[i].ResetsAt = at
				}
			}
		}
		if q.Plan == "" {
			q.Plan = prev.Plan
		}
	}
	c.setQuota(q)
}

func (c *Cerebrate) setQuota(q *adapter.Quota) {
	c.mu.Lock()
	c.quota, c.quotaSeen = q, c.cfg.clock()
	c.mu.Unlock()
}

// AdoptQuota copies a reading taken for another role on the same harness.
// Only fills a gap: a role that has heard from its own stream has the better
// record, since that one carries exact reset stamps.
func (c *Cerebrate) AdoptQuota(q *adapter.Quota, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.quota == nil || at.After(c.quotaSeen) {
		c.quota, c.quotaSeen = q, at
	}
}

// RefreshQuota asks a harness that has to be asked. Harnesses that report
// quota on their own stream do not implement QuotaReporter and are skipped,
// so this is not a second path to the same number.
func (c *Cerebrate) RefreshQuota(ctx context.Context) {
	r, ok := c.cfg.Adapter.(adapter.QuotaReporter)
	if !ok {
		return
	}
	q, ok, err := r.Quota(ctx)
	if err != nil || !ok {
		// A gauge that cannot be read is not a failure of the role. The last
		// good reading is kept rather than blanked, with its timestamp, so the
		// cockpit can say how stale it is instead of showing nothing.
		return
	}
	c.mergeQuota(&q)
}

// ThrottledUntil is when a spent quota window is expected to lift. Zero when
// the role is not throttled, or when the harness did not say.
func (c *Cerebrate) ThrottledUntil() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.throttledUntil
}

func (c *Cerebrate) Restarts() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.restarts
}

func (c *Cerebrate) setBusy(b bool) {
	c.mu.Lock()
	c.busy = b
	c.mu.Unlock()
}

func (c *Cerebrate) setState(s State, reason string) {
	c.mu.Lock()
	c.state = s
	if s != StateThrottled {
		c.throttledUntil = time.Time{}
	}
	if reason != "" {
		c.lastErr = reason
	}
	c.mu.Unlock()
}

// Run supervises the agent until ctx is cancelled or the role fails fatally.
//
// It returns nil on deliberate shutdown and an error only when the role cannot
// run at all — a failed role is a state to display, not a reason to take the
// rest of the swarm down.
func (c *Cerebrate) Run(ctx context.Context) error {
	backoff := c.cfg.backoffBase

	for {
		if ctx.Err() != nil {
			c.setState(StateStopped, "")
			return nil
		}

		fatal, ranFor, err := c.runOnce(ctx)
		switch {
		case ctx.Err() != nil:
			c.setState(StateStopped, "")
			return nil
		}

		// Before anything else, a spent quota window: it presents as fatal on
		// one harness and as an ordinary error on the other, so it is checked
		// on both paths. Checking only the fatal one is how this first shipped,
		// and it left claude crash-looping through a limit it should wait out.
		if t, ok := c.throttle(c.streamErr(), errString(err)); ok {
			if !c.waitOutThrottle(ctx, t) {
				c.setState(StateStopped, "")
				return nil
			}
			backoff = c.cfg.backoffBase
			continue
		}

		switch {
		case fatal:
			// A spent quota window looks like a fatal error and is not one:
			// nothing is wrong with the agent, the code, or the task, and the
			// correct response is to wait. Failing here would cost an operator
			// the time it takes to discover the thing to do was nothing.
			// Restarting into the same wall is how twenty minutes gets spent on
			// an agent that cannot work. A fatal error means stop and say so.
			c.setState(StateFailed, errString(err))
			c.publish(adapter.Event{
				Kind: adapter.EventError, Fatal: true,
				Text: fmt.Sprintf("%s stopped: %s", c.name(), errString(err)),
			})
			return nil
		}

		if ranFor >= stableRun {
			backoff = c.cfg.backoffBase
			c.mu.Lock()
			c.restarts = 0
			c.mu.Unlock()
		}
		c.mu.Lock()
		c.restarts++
		attempts := c.restarts
		c.mu.Unlock()

		c.cfg.Log.Warn("agent exited, restarting",
			"role", c.name(), "after", ranFor, "attempt", attempts,
			"backoff", backoff, "err", errString(err))

		// A blocked role keeps saying blocked while it retries.
		//
		// Overwriting it with "starting" was wrong twice over: a role waiting
		// for a person to fix its PATH is not starting, and the true state was
		// then visible only in the few milliseconds between the check failing
		// and this line — which the cockpit polls far too slowly to catch, so
		// a blocked role read as one perpetually about to start.
		if c.State() != StateBlocked {
			c.setState(StateStarting, errString(err))
		}

		select {
		case <-ctx.Done():
			c.setState(StateStopped, "")
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce spawns the agent and reads it until the process exits. It reports
// whether the failure was fatal and how long the process lasted.
func (c *Cerebrate) runOnce(ctx context.Context) (fatal bool, ranFor time.Duration, err error) {
	// Configuration as the database has it now, not as it was when the swarm
	// started. A role that crashes and respawns must come back as currently
	// configured, or an edit silently applies to some roles and not others.
	if c.cfg.Refresh != nil {
		fresh, err := c.cfg.Refresh(ctx)
		switch {
		case err != nil:
			// Keep going with what we have: a database hiccup should not take
			// a working role down, and the next respawn tries again.
			c.cfg.Log.Warn("re-reading role configuration", "role", c.name(), "err", err)
		case fresh.Gone:
			return true, 0, fmt.Errorf("role %s is no longer part of this team", c.name())
		default:
			c.mu.Lock()
			c.live = liveConfig{
				role:         fresh.Role,
				systemPrompt: fresh.SystemPrompt,
				harnessFlags: fresh.HarnessFlags,
			}
			c.mu.Unlock()
		}
	}

	// One consistent read for the whole spawn: a refresh landing midway would
	// otherwise build a spec from two different configurations.
	c.mu.RLock()
	live := c.live
	c.mu.RUnlock()

	// A private instance for this process. claude latches the model and
	// billing mode a turn actually used, and a shared instance would let three
	// concurrent roles overwrite each other's.
	a := adapter.ForSession(c.cfg.Adapter)
	c.mu.Lock()
	c.session = a
	c.mu.Unlock()

	spec := adapter.Spec{
		Role:     live.role.Name,
		Worktree: c.cfg.Worktree,
		Model:    live.role.Model,
		Thinking: live.role.Thinking,
		// Harness flags first, the role's own args after. Later wins in every
		// CLI here, so the more specific statement is the one that takes
		// effect: a role that sets --permission-mode overrides the default for
		// its harness without having to know what that default was.
		ExtraArgs: append(append([]string{}, live.harnessFlags...), live.role.Args...),
		ConfigDir: c.cfg.ConfigDir,
		Socket:    c.cfg.Socket,
		Token:     c.cfg.Token,
		BinDir:    c.cfg.BinDir,
		Env:       c.cfg.Env,
		// Only where somebody is watching the words appear, and only where the
		// harness can do it. Asking a harness that cannot would either be
		// ignored or refused at spawn, and neither is worth finding out per
		// role at runtime.
		Streaming: c.cfg.Streaming && c.sessionAdapter().Capabilities().Streaming,
	}

	// Preflight runs before every spawn, not only at Start: a token expires, a
	// binary is upgraded, another tool rewrites a shared config.
	if c.cfg.Preflight != nil {
		if err := c.cfg.Preflight.CheckRole(ctx, spec, a); err != nil {
			c.setState(StateBlocked, err.Error())
			c.publish(adapter.Event{Kind: adapter.EventError, Text: err.Error()})
			// Blocked is not fatal: a human fixing the remedy should be picked
			// up on the next attempt rather than needing a full restart.
			return false, 0, err
		}
	}

	promptFile, err := c.writeSystemPrompt()
	if err != nil {
		return true, 0, err
	}
	spec.SystemFile = promptFile

	cmd, err := a.Command(ctx, spec)
	if err != nil {
		return true, 0, err // a command that cannot be built will not build next time either
	}

	// Our own pipe rather than cmd.StdoutPipe().
	//
	// Wait closes a StdoutPipe as soon as the process exits, and the standard
	// library says so: it is incorrect to call Wait before every read from that
	// pipe has completed. This supervisor reads on a goroutine and calls Wait
	// concurrently, which it must, so with a fast-exiting agent the close won
	// the race and the buffered output was discarded unread.
	//
	// What that cost is exactly the thing this file exists to prevent. An agent
	// that prints `fatal: model requires a newer version` and exits could have
	// its fatal line thrown away, leaving a plain non-zero exit, which is a
	// restartable failure. The supervisor then respawned into the same wall.
	// Reproducible at GOMAXPROCS=1, and CI is where it showed.
	//
	// A pipe we own is closed when we say, so the reader drains it after the
	// process is gone.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return false, 0, fmt.Errorf("attaching stdout: %w", err)
	}
	cmd.Stdout = stdoutW
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, 0, fmt.Errorf("attaching stdin: %w", err)
	}

	superviseProcess(cmd, shutdownGrace)

	c.setState(StateStarting, "")
	started := c.cfg.clock()
	if err := cmd.Start(); err != nil {
		return false, 0, fmt.Errorf("starting %s: %w", c.name(), err)
	}

	c.mu.Lock()
	c.stdin = stdin
	c.ready = make(chan struct{})
	c.busy = false
	c.mu.Unlock()

	// The process is up and its input is open, which is the whole of what
	// "ready" means here: it can be handed a turn.
	//
	// This deliberately does not wait for the harness to announce itself.
	// claude emits its init event only in response to the first turn, so an
	// agent with nothing queued would never announce anything and would sit at
	// "starting" indefinitely — which is exactly how a stuck lease came to look
	// like a failed spawn. The init event still arrives, and still latches the
	// model and billing mode; it is just not what gates delivery.
	c.setState(StateReady, "")

	// Read on a goroutine so Wait is always reached. Wait is what enforces
	// WaitDelay, and WaitDelay is what closes the pipes when a descendant is
	// still holding one open — reading inline first would deadlock exactly
	// when that happens.
	readDone := make(chan error, 1)
	go func() { readDone <- c.readStream(stdout) }()

	// The parent's copy of the write end, so the reader sees EOF once the
	// process and anything it spawned have let go of theirs.
	stdoutW.Close()

	waitErr := cmd.Wait()

	// Drain what the process wrote before deciding anything about it. Bounded,
	// because a descendant can still be holding the write end open: this is the
	// case WaitDelay covers for a StdoutPipe, and with a pipe we own it is ours
	// to bound. Closing the read end unblocks the reader.
	var sawFatal error
	select {
	case sawFatal = <-readDone:
	case <-time.After(shutdownGrace):
		c.cfg.Log.Warn("agent output was still open after it exited", "role", c.name())
		stdout.Close()
		sawFatal = <-readDone
	}
	stdout.Close()
	ranFor = c.cfg.clock().Sub(started)

	c.mu.Lock()
	c.stdin = nil
	c.mu.Unlock()
	stdin.Close()

	if sawFatal != nil {
		return true, ranFor, sawFatal
	}
	return false, ranFor, waitErr
}

// readStream consumes the agent's output until it closes, publishing events.
// It returns the first fatal error seen, if any.
func (c *Cerebrate) readStream(stdout io.Reader) error {
	scanner := bufio.NewScanner(stdout)
	// Agent output carries whole file contents and diffs, so the default 64KB
	// line limit is not enough; a truncated line would fail to parse and look
	// like a harness bug.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var fatal error
	for scanner.Scan() {
		events, err := c.sessionAdapter().Parse(scanner.Bytes())
		if err != nil {
			// An unparseable line is worth knowing about but is not grounds to
			// kill a working agent: harnesses print things adapters have not
			// seen yet.
			c.cfg.Log.Debug("unparsed agent output", "role", c.name(), "err", err)
			continue
		}
		for _, ev := range events {
			// A turn stopped on request ends the way a failed one does: the
			// harness reports a result marked as an error, with nothing to say
			// about it. The adapter cannot tell those apart -- it did not ask
			// for the stop -- so it is untangled here, where the asking
			// happened. Without this, pressing stop put "the harness reported
			// an error without describing it" on screen underneath the answer
			// somebody had just chosen to cut short.
			if ev.Kind == adapter.EventError && c.tookInterrupt() {
				c.observe(adapter.Event{Kind: adapter.EventTurnEnd})
				c.publish(adapter.Event{Kind: adapter.EventTurnEnd})
				continue
			}
			c.observe(ev)
			c.publish(ev)
			if ev.Kind == adapter.EventQuota && ev.Quota != nil {
				c.setQuota(ev.Quota)
			}
			if ev.Kind == adapter.EventError {
				// Kept whether or not it is fatal: claude reports a spent
				// quota window as an ordinary error, and the supervisor has
				// to be able to recognise it on the non-fatal path too.
				c.mu.Lock()
				c.lastStreamErr = ev.Text
				c.mu.Unlock()
			}
			if ev.Kind == adapter.EventError && ev.Fatal && fatal == nil {
				fatal = errors.New(ev.Text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		c.cfg.Log.Debug("agent output ended", "role", c.name(), "err", err)
	}
	return fatal
}

// observe moves the cerebrate's state in response to what the agent said.
func (c *Cerebrate) observe(ev adapter.Event) {
	switch ev.Kind {
	case adapter.EventReady:
		c.setState(StateReady, "")
		c.mu.Lock()
		select {
		case <-c.ready: // already closed
		default:
			close(c.ready)
		}
		c.mu.Unlock()
	case adapter.EventToolCall, adapter.EventMessage, adapter.EventThinking:
		if c.State() == StateReady {
			c.setState(StateWorking, "")
		}
	case adapter.EventTurnEnd:
		c.setState(StateReady, "")
		c.setBusy(false)
	case adapter.EventError:
		if ev.Fatal {
			c.setState(StateFailed, ev.Text)
		}
		// A failed turn is a finished turn: leaving the agent marked busy
		// would strand it, holding a lease it can no longer satisfy.
		c.setBusy(false)
	}
}

// tookInterrupt reports whether this error belongs to a stop somebody asked
// for, and clears the flag either way: one interrupt ends one turn.
func (c *Cerebrate) tookInterrupt() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.interrupting
	c.interrupting = false
	return was
}

func (c *Cerebrate) publish(ev adapter.Event) {
	c.mu.Lock()
	c.spoke = c.cfg.clock()
	c.mu.Unlock()

	if c.cfg.Bus == nil {
		return
	}
	c.cfg.Bus.Publish(event.Event{
		Event:     ev,
		ID:        store.NewID(),
		ProjectID: c.cfg.ProjectID,
		Role:      c.name(),
		Harness:   c.Harness(),
		At:        c.cfg.clock(),
	})
}

// Idle reports whether the agent can take work now: its input is open and it
// is not already mid-turn.
func (c *Cerebrate) Idle() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stdin == nil || c.busy {
		return false
	}
	switch c.state {
	case StateFailed, StateBlocked, StateStopped:
		return false
	}
	return true
}

// Submit hands a turn to the running agent.
//
// Encoding is the adapter's job because the harnesses disagree entirely about
// the shape. Nothing here injects keystrokes: this is a write to a pipe, and a
// closed pipe is an error the caller can see, rather than a tmux exit code of 0
// that only ever meant "the keys were accepted".
// Interrupt ends the turn in flight, keeping the session.
//
// The conversation is inside the running harness, so stopping a reply by
// killing the process answers "cancel this" by forgetting everything said so
// far. Where a harness has a message for it, that is what is sent; where it
// does not, the caller is told rather than left believing something happened.
func (c *Cerebrate) Interrupt() error {
	c.mu.RLock()
	w, busy := c.stdin, c.busy
	c.mu.RUnlock()
	if w == nil {
		return fmt.Errorf("%s is not running", c.name())
	}
	// Nothing to stop is not a failure: the answer arrived while the button was
	// being pressed, which is the common case for a short turn.
	if !busy {
		return nil
	}

	payload, err := c.sessionAdapter().EncodeInterrupt()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.interrupting = true
	c.mu.Unlock()

	if _, err := w.Write(payload); err != nil {
		c.mu.Lock()
		c.interrupting = false
		c.mu.Unlock()
		return fmt.Errorf("interrupting %s: %w", c.name(), err)
	}
	// Deliberately not clearing busy here. The turn is over when the harness
	// says it is over, and it reports that the same way it reports any other
	// ending; assuming otherwise would let the next question in while this one
	// is still unwinding.
	return nil
}

func (c *Cerebrate) Submit(text string) error {
	c.mu.RLock()
	w := c.stdin
	c.mu.RUnlock()
	if w == nil {
		return fmt.Errorf("%s is not running", c.name())
	}

	payload, err := c.sessionAdapter().EncodeTurn(text)
	if err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("submitting a turn to %s: %w", c.name(), err)
	}

	c.mu.Lock()
	c.busy = true
	c.mu.Unlock()
	return nil
}

// WaitReady blocks until the agent is accepting turns, or ctx ends.
//
// It re-reads state rather than parking on one channel forever. Each spawn
// installs a fresh ready channel, so a caller that captured the previous one
// would otherwise wait on something nobody will ever close — the exact shape
// of deadlock this supervisor exists to prevent, reproduced inside it.
func (c *Cerebrate) WaitReady(ctx context.Context) error {
	for {
		c.mu.RLock()
		state, ready := c.state, c.ready
		c.mu.RUnlock()

		switch state {
		case StateReady, StateWorking:
			return nil
		case StateFailed:
			return fmt.Errorf("%s failed: %s", c.name(), c.LastError())
		case StateStopped:
			return fmt.Errorf("%s is stopped", c.name())
		}

		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
			// Re-read: the channel may have been swapped by a respawn.
		}
	}
}

// writeSystemPrompt composes the prompt to disk for the harness to read.
//
// It is rewritten on every spawn from what the caller supplied, which came
// from the database. Nothing is copied into the worktree, so there is no
// snapshot to go stale — the failure that had six agents build a task in the
// wrong language because a config edit reached none of them.
func (c *Cerebrate) writeSystemPrompt() (string, error) {
	c.mu.RLock()
	prompt, roleName := c.live.systemPrompt, c.live.role.Name
	c.mu.RUnlock()

	if prompt == "" {
		return "", nil
	}
	dir := c.cfg.StateDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	// And tighten it if it already existed. MkdirAll's mode applies only to
	// directories it creates, so an installation upgraded from a version that
	// used 0755 kept 0755. Best effort — the directory can be one this process
	// does not own, and the file's own mode below is what protects the text.
	if err := os.Chmod(dir, 0o700); err != nil {
		c.cfg.Log.Debug("could not tighten the prompt directory", "dir", dir, "err", err)
	}
	path := filepath.Join(dir, roleName+".system.md")
	// 0600: a composed prompt carries the operator's own instructions, and it
	// sits in a shared temporary directory where anything can read a 0644 file.
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return "", fmt.Errorf("writing the composed prompt: %w", err)
	}
	// WriteFile applies its mode only on creation too, so a prompt file left by
	// an earlier version stays 0644 through every rewrite.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("securing %s: %w", path, err)
	}
	return path, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// throttleRecheck is how long to wait when the harness said the quota was
// spent but not when it lifts. Long enough not to hammer the provider,
// short enough that a window that rolled over is noticed.
const throttleRecheck = 5 * time.Minute

// throttle asks the adapter whether this failure is a provider quota limit.
// Adapters that cannot tell simply do not implement Throttler.
func (c *Cerebrate) throttle(texts ...string) (adapter.Throttle, bool) {
	a, ok := c.cfg.Adapter.(adapter.Throttler)
	if !ok {
		return adapter.Throttle{}, false
	}
	// The agent's own output first: an exit status says a process died, not
	// that a provider refused it.
	for _, text := range texts {
		if text == "" {
			continue
		}
		if t, ok := a.ThrottledBy(text); ok {
			return t, true
		}
	}
	return adapter.Throttle{}, false
}

// streamErr is the last error the agent's output carried.
func (c *Cerebrate) streamErr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastStreamErr
}

// waitOutThrottle holds the role until the quota window rolls over. It reports
// false if the swarm was shut down while waiting.
//
// The role stays configured and its worktree untouched, so nothing has to be
// rebuilt when it resumes — this is a pause, not a teardown.
func (c *Cerebrate) waitOutThrottle(ctx context.Context, t adapter.Throttle) bool {
	wait := throttleRecheck
	if !t.Until.IsZero() {
		// A minute past the stated time: resuming exactly on it races the
		// provider's own clock, and losing that race spends another attempt
		// to learn nothing.
		if d := time.Until(t.Until) + time.Minute; d > 0 {
			wait = d
		}
	}

	c.mu.Lock()
	c.throttledUntil = c.cfg.clock().Add(wait)
	c.mu.Unlock()
	c.setState(StateThrottled, t.Detail)

	detail := t.Detail
	if detail == "" {
		detail = "the provider refused work on quota"
	}
	c.cfg.Log.Warn("provider quota spent, waiting",
		"role", c.name(), "resumes_in", wait.Round(time.Second), "detail", detail)
	c.publish(adapter.Event{
		Kind: adapter.EventError,
		Text: fmt.Sprintf("%s is waiting on a provider limit, resuming in %s: %s",
			c.name(), wait.Round(time.Minute), detail),
	})

	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}

// sessionAdapter is this process's own instance, falling back to the shared
// one before the first spawn.
func (c *Cerebrate) sessionAdapter() adapter.Adapter {
	c.mu.RLock()
	a := c.session
	c.mu.RUnlock()
	if a == nil {
		return c.cfg.Adapter
	}
	return a
}

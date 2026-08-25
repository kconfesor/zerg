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

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/event"
	"github.com/konfessor/zerg/internal/store"
)

// State is what the cockpit shows for a role.
type State string

const (
	StateIdle     State = "idle"     // not started
	StateStarting State = "starting" // spawned, no ready event yet
	StateReady    State = "ready"    // accepting turns
	StateWorking  State = "working"  // mid-turn
	StateBlocked  State = "blocked"  // preflight refused; needs a human
	StateFailed   State = "failed"   // fatal; will not restart
	StateStopped  State = "stopped"  // shut down deliberately
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

	// SystemPrompt is composed fresh by the caller from shared instructions
	// plus the role prompt, so an edit is live on restart and no stale copy
	// can survive in a worktree.
	SystemPrompt string

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
	return &Cerebrate{cfg: cfg, state: StateIdle, ready: make(chan struct{})}
}

func (c *Cerebrate) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// LastError is why a role is blocked or failed, for the cockpit to show.
func (c *Cerebrate) LastError() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

// Restarts counts respawns since the last stable run.
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
		case fatal:
			// Restarting into the same wall is how twenty minutes gets spent on
			// an agent that cannot work. A fatal error means stop and say so.
			c.setState(StateFailed, errString(err))
			c.publish(adapter.Event{
				Kind: adapter.EventError, Fatal: true,
				Text: fmt.Sprintf("%s stopped: %s", c.cfg.Role.Name, errString(err)),
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
			"role", c.cfg.Role.Name, "after", ranFor, "attempt", attempts,
			"backoff", backoff, "err", errString(err))
		c.setState(StateStarting, errString(err))

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
	spec := adapter.Spec{
		Role:      c.cfg.Role.Name,
		Worktree:  c.cfg.Worktree,
		Model:     c.cfg.Role.Model,
		ExtraArgs: c.cfg.Role.Args,
		ConfigDir: c.cfg.ConfigDir,
		Socket:    c.cfg.Socket,
		Token:     c.cfg.Token,
		BinDir:    c.cfg.BinDir,
	}

	// Preflight runs before every spawn, not only at Start: a token expires, a
	// binary is upgraded, another tool rewrites a shared config.
	if c.cfg.Preflight != nil {
		if err := c.cfg.Preflight.CheckRole(ctx, spec, c.cfg.Adapter); err != nil {
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

	cmd, err := c.cfg.Adapter.Command(ctx, spec)
	if err != nil {
		return true, 0, err // a command that cannot be built will not build next time either
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, 0, fmt.Errorf("attaching stdout: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, 0, fmt.Errorf("attaching stdin: %w", err)
	}

	superviseProcess(cmd, shutdownGrace)

	c.setState(StateStarting, "")
	started := c.cfg.clock()
	if err := cmd.Start(); err != nil {
		return false, 0, fmt.Errorf("starting %s: %w", c.cfg.Role.Name, err)
	}

	c.mu.Lock()
	c.stdin = stdin
	c.ready = make(chan struct{})
	c.busy = false
	c.mu.Unlock()

	// Read on a goroutine so Wait is always reached. Wait is what enforces
	// WaitDelay, and WaitDelay is what closes the pipes when a descendant is
	// still holding one open — reading inline first would deadlock exactly
	// when that happens.
	readDone := make(chan error, 1)
	go func() { readDone <- c.readStream(stdout) }()

	waitErr := cmd.Wait()
	sawFatal := <-readDone
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
		events, err := c.cfg.Adapter.Parse(scanner.Bytes())
		if err != nil {
			// An unparseable line is worth knowing about but is not grounds to
			// kill a working agent: harnesses print things adapters have not
			// seen yet.
			c.cfg.Log.Debug("unparsed agent output", "role", c.cfg.Role.Name, "err", err)
			continue
		}
		for _, ev := range events {
			c.observe(ev)
			c.publish(ev)
			if ev.Kind == adapter.EventError && ev.Fatal && fatal == nil {
				fatal = errors.New(ev.Text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		c.cfg.Log.Debug("agent output ended", "role", c.cfg.Role.Name, "err", err)
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

func (c *Cerebrate) publish(ev adapter.Event) {
	if c.cfg.Bus == nil {
		return
	}
	c.cfg.Bus.Publish(event.Event{
		Event:     ev,
		ID:        store.NewID(),
		ProjectID: c.cfg.ProjectID,
		Role:      c.cfg.Role.Name,
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
func (c *Cerebrate) Submit(text string) error {
	c.mu.RLock()
	w := c.stdin
	c.mu.RUnlock()
	if w == nil {
		return fmt.Errorf("%s is not running", c.cfg.Role.Name)
	}

	payload, err := c.cfg.Adapter.EncodeTurn(text)
	if err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("submitting a turn to %s: %w", c.cfg.Role.Name, err)
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
			return fmt.Errorf("%s failed: %s", c.cfg.Role.Name, c.LastError())
		case StateStopped:
			return fmt.Errorf("%s is stopped", c.cfg.Role.Name)
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
	if c.cfg.SystemPrompt == "" {
		return "", nil
	}
	dir := c.cfg.StateDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, c.cfg.Role.Name+".system.md")
	if err := os.WriteFile(path, []byte(c.cfg.SystemPrompt), 0o644); err != nil {
		return "", fmt.Errorf("writing the composed prompt: %w", err)
	}
	return path, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

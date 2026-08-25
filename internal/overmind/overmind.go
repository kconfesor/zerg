// Package overmind starts and stops a project's swarm.
//
// It is the only place that knows the whole sequence: check the team can work,
// prepare worktrees, compose prompts, mint tokens, spawn cerebrates, and keep
// the queue moving. Everything it coordinates is testable on its own; this
// package is the wiring, deliberately thin.
package overmind

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/agent"
	"github.com/konfessor/zerg/internal/cerebrate"
	"github.com/konfessor/zerg/internal/event"
	"github.com/konfessor/zerg/internal/hatchery"
	"github.com/konfessor/zerg/internal/nydus"
	"github.com/konfessor/zerg/internal/preflight"
	"github.com/konfessor/zerg/internal/store"
)

const (
	// tick paces the two background duties: expiring lapsed leases and nudging
	// idle agents that have work waiting.
	tick = 2 * time.Second

	// nudge is what an idle agent is told when work is queued for it.
	//
	// This is the descendant of the predecessor's wake-up, and the differences
	// are the point. That one was a fixed string of keystrokes fired into
	// whichever tmux pane happened to be focused, with one chance to land. This
	// is a structured message on a pipe, driven by durable queue state — a
	// missed nudge is corrected on the next tick, because the work is still
	// there to be found.
	nudge = "You have work queued. Run `zerg next` to claim it."
)

// ErrNotReady is returned when a team cannot work. It carries the readiness
// report so the caller can show which role failed which check, and why.
type ErrNotReady struct{ Report *preflight.Report }

func (e *ErrNotReady) Error() string {
	var blocked []string
	for _, r := range e.Report.Roles {
		if r.Status == preflight.StatusBlocked {
			blocked = append(blocked, r.Role)
		}
	}
	if len(blocked) == 0 {
		return "no roles are enabled for this project"
	}
	return fmt.Sprintf("these roles cannot work: %s", strings.Join(blocked, ", "))
}

// Overmind owns every running project.
type Overmind struct {
	db        *store.DB
	nyd       *nydus.Nydus
	registry  *adapter.Registry
	preflight *preflight.Runner
	bus       *event.Bus
	agents    *agent.Server
	log       *slog.Logger
	stateDir  string

	mu      sync.Mutex
	running map[string]*swarm
}

// swarm is one project's live state.
type swarm struct {
	session    *store.Session
	cancel     context.CancelFunc
	done       chan struct{}
	cerebrates map[string]*cerebrate.Cerebrate
	tokens     []string
}

type Config struct {
	DB        *store.DB
	Nydus     *nydus.Nydus
	Registry  *adapter.Registry
	Preflight *preflight.Runner
	Bus       *event.Bus
	Agents    *agent.Server
	Log       *slog.Logger
	StateDir  string
}

func New(cfg Config) *Overmind {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(os.TempDir(), "zerg")
	}
	return &Overmind{
		db: cfg.DB, nyd: cfg.Nydus, registry: cfg.Registry,
		preflight: cfg.Preflight, bus: cfg.Bus, agents: cfg.Agents,
		log: cfg.Log, stateDir: cfg.StateDir,
		running: map[string]*swarm{},
	}
}

// Status describes one role to the cockpit.
type Status struct {
	Role      string          `json:"role"`
	Harness   string          `json:"harness"`
	Model     string          `json:"model"`
	State     cerebrate.State `json:"state"`
	LastError string          `json:"lastError,omitempty"`
	Restarts  int             `json:"restarts"`
	Terminal  bool            `json:"terminal"`
}

// Running reports whether a project's swarm is up.
func (o *Overmind) Running(projectID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.running[projectID]
	return ok
}

// Status returns each role's live state, or nil when the project is stopped.
func (o *Overmind) Status(ctx context.Context, projectID string) ([]Status, error) {
	o.mu.Lock()
	s, ok := o.running[projectID]
	o.mu.Unlock()
	if !ok {
		return nil, nil
	}

	team, err := o.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out []Status
	for _, role := range team {
		if !role.Enabled {
			continue
		}
		st := Status{
			Role: role.Name, Harness: role.Harness, Model: role.Model,
			State: cerebrate.StateIdle, Terminal: role.Terminal,
		}
		if c, ok := s.cerebrates[role.Name]; ok {
			st.State, st.LastError, st.Restarts = c.State(), c.LastError(), c.Restarts()
		}
		out = append(out, st)
	}
	return out, nil
}

// Start brings a project's swarm up.
//
// The readiness gate comes first and is absolute: a team that cannot work must
// never reach a running board. The predecessor's launches always "succeeded" —
// six sessions up, dashboard green, board drawn — while half the roles sat at a
// prompt they could not pass, and nothing anywhere said so.
func (o *Overmind) Start(ctx context.Context, projectID string) error {
	o.mu.Lock()
	if _, ok := o.running[projectID]; ok {
		o.mu.Unlock()
		return fmt.Errorf("this project is already running")
	}
	o.mu.Unlock()

	project, err := o.db.GetProject(ctx, projectID)
	if err != nil {
		return err
	}

	report, err := o.preflight.Check(ctx, projectID)
	if err != nil {
		return err
	}
	if !report.Ready {
		return &ErrNotReady{Report: report}
	}

	team, err := o.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return err
	}
	shared, err := o.sharedInstructions(ctx)
	if err != nil {
		return err
	}

	// Worktrees before anything spawns: an agent whose directory does not exist
	// fails in a way that reads as a harness problem.
	hatch := hatchery.New(project.Path)
	if err := hatch.EnsureRepo(ctx, project.BaseBranch); err != nil {
		return err
	}

	session, err := o.db.StartSession(ctx, projectID)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	s := &swarm{
		session: session, cancel: cancel, done: make(chan struct{}),
		cerebrates: map[string]*cerebrate.Cerebrate{},
	}

	for _, role := range team {
		if !role.Enabled {
			continue
		}
		a, err := o.registry.Get(role.Harness)
		if err != nil {
			cancel()
			return err
		}
		worktree, err := hatch.EnsureWorktree(ctx, role.Name, project.BaseBranch)
		if err != nil {
			cancel()
			return err
		}

		token := o.agents.Mint(projectID, role.Name)
		s.tokens = append(s.tokens, token)

		c := cerebrate.New(cerebrate.Config{
			ProjectID: projectID,
			Role:      role,
			Adapter:   a,
			Worktree:  worktree,
			// A private harness config directory per role. Two agents racing a
			// read-modify-write of one shared global file is how a config ended
			// up holding three concatenated copies of itself.
			ConfigDir:    o.roleDir(projectID, role.Name, "config"),
			Socket:       o.agents.Path(),
			Token:        token,
			SystemPrompt: composePrompt(shared, role.Prompt),
			Bus:          o.bus,
			Log:          o.log,
			Preflight:    o.preflight,
			StateDir:     o.roleDir(projectID, role.Name, "state"),
		})
		s.cerebrates[role.Name] = c
		go c.Run(runCtx)
	}

	o.mu.Lock()
	o.running[projectID] = s
	o.mu.Unlock()

	go o.keepMoving(runCtx, projectID, s)

	o.log.Info("swarm up", "project", project.Name, "roles", len(s.cerebrates))
	return nil
}

// Stop takes a project's swarm down.
func (o *Overmind) Stop(ctx context.Context, projectID, reason string) error {
	o.mu.Lock()
	s, ok := o.running[projectID]
	if !ok {
		o.mu.Unlock()
		return fmt.Errorf("this project is not running")
	}
	delete(o.running, projectID)
	o.mu.Unlock()

	s.cancel()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		o.log.Warn("swarm did not stop cleanly in time", "project", projectID)
	}

	// Tokens die with the swarm: a leftover token is a capability nobody is
	// watching any more.
	for _, t := range s.tokens {
		o.agents.Revoke(t)
	}
	if err := o.db.EndSession(ctx, s.session.ID, reason); err != nil {
		o.log.Warn("could not close the session record", "err", err)
	}
	o.log.Info("swarm down", "project", projectID, "reason", reason)
	return nil
}

// StopAll shuts every running project down, for daemon shutdown.
func (o *Overmind) StopAll(ctx context.Context, reason string) {
	o.mu.Lock()
	ids := make([]string, 0, len(o.running))
	for id := range o.running {
		ids = append(ids, id)
	}
	o.mu.Unlock()

	for _, id := range ids {
		if err := o.Stop(ctx, id, reason); err != nil {
			o.log.Warn("stopping project", "project", id, "err", err)
		}
	}
}

// keepMoving runs the two duties that keep a pipeline from stalling.
func (o *Overmind) keepMoving(ctx context.Context, projectID string, s *swarm) {
	defer close(s.done)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Lapsed leases first: requeued work should be visible to the
			// nudge that follows in the same tick.
			if n, err := o.nyd.ExpireLeases(ctx); err != nil {
				o.log.Warn("expiring leases", "err", err)
			} else if n > 0 {
				o.log.Info("returned unacknowledged work to the queue", "leases", n)
			}
			o.nudgeIdle(ctx, projectID, s)
		}
	}
}

// nudgeIdle tells agents that are ready, and only ready, to go and claim.
//
// An agent mid-turn is left alone: interrupting it would either be ignored or
// arrive as a second instruction inside work it is already doing. Because the
// queue is durable, skipping a nudge costs nothing — the next tick finds the
// same work still waiting.
func (o *Overmind) nudgeIdle(ctx context.Context, projectID string, s *swarm) {
	for role, c := range s.cerebrates {
		if c.State() != cerebrate.StateReady {
			continue
		}
		n, err := o.db.QueuedCount(ctx, projectID, role)
		if err != nil {
			o.log.Warn("checking the queue", "role", role, "err", err)
			continue
		}
		if n == 0 {
			continue
		}
		if err := c.Submit(nudge); err != nil {
			o.log.Debug("could not nudge", "role", role, "err", err)
		}
	}
}

func (o *Overmind) sharedInstructions(ctx context.Context) (string, error) {
	text, err := o.db.GetSetting(ctx, store.SettingSharedInstructions)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return text, nil
}

// composePrompt joins the shared instructions to a role's own.
//
// Composed fresh at every spawn from the database, never copied into a
// worktree. That is the whole fix for config that silently goes stale: there is
// no second copy to diverge from.
func composePrompt(shared, role string) string {
	switch {
	case shared == "":
		return role
	case role == "":
		return shared
	default:
		return shared + "\n\n---\n\n" + role
	}
}

func (o *Overmind) roleDir(projectID, role, kind string) string {
	return filepath.Join(o.stateDir, projectID, role, kind)
}

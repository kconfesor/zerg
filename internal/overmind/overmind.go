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

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/cerebrate"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/preflight"
	"github.com/kconfesor/zerg/internal/store"
)

const (
	// tick paces the two background duties: expiring lapsed leases and nudging
	// idle agents that have work waiting.
	tick = 2 * time.Second

	// quotaEvery is how often a harness that has to be asked for its
	// subscription quota is asked. Slow on purpose: the figure moves in
	// percent over hours, the call leaves the machine, and a gauge is not
	// worth a request every two seconds. Harnesses that report quota on their
	// own stream are not polled at all.
	quotaEvery = 2 * time.Minute

	// nudge is what an idle agent is told when work is queued for it.
	//
	// The alternative is a fixed string of keystrokes fired into whichever
	// terminal pane happens to be focused, with exactly one chance to land. This
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
	// starting reserves a project between the check and its registration, so
	// two concurrent Starts cannot both get past the check.
	starting map[string]bool
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

	// ThrottledUntil is when a spent provider quota is expected to lift. Set
	// only while the state is throttled, and absent when the harness said the
	// window was spent but not for how long.
	ThrottledUntil *time.Time `json:"throttledUntil,omitempty"`

	// Quota is what this role's subscription has left. Absent for a metered
	// API key, which has no window to report.
	Quota    *QuotaReport `json:"quota,omitempty"`
	Terminal bool         `json:"terminal"`
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
			if until := c.ThrottledUntil(); !until.IsZero() {
				st.ThrottledUntil = &until
			}

		}
		out = append(out, st)
	}
	return out, nil
}

// QuotaReport is a subscription's headroom, shaped for the cockpit: windows
// already labelled and ordered, so the view does no arithmetic.
// Quotas is the account-level view: one report per harness, not per role.
//
// A subscription belongs to the account, and every role on that harness draws
// from the same windows. Reporting it per role showed a gauge under whichever
// role had most recently taken a turn and nothing under the others, which reads
// as one role having headroom the rest lack.
// Keyed by provider, not harness: two harnesses can front the same account,
// and one harness can front several providers with unrelated limits.
type Quotas map[string]*QuotaReport

// QuotaReport struct
type QuotaReport struct {
	Provider string        `json:"provider"`
	Plan     string        `json:"plan,omitempty"`
	Windows  []QuotaWindow `json:"windows"`
	// SeenAt is when this was last learned. A gauge with no age is a gauge you
	// cannot tell is stale.
	SeenAt time.Time `json:"seenAt"`
}

type QuotaWindow struct {
	Label    string     `json:"label"` // "5h", "7d"
	Used     float64    `json:"used"`  // 0..1
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
}

func quotaReport(q *adapter.Quota, seen time.Time) *QuotaReport {
	if q == nil || len(q.Windows) == 0 {
		return nil
	}
	out := &QuotaReport{Provider: q.Provider, Plan: q.Plan, SeenAt: seen}
	for _, w := range q.Windows {
		win := QuotaWindow{Label: w.Label(), Used: w.Used}
		if !w.ResetsAt.IsZero() {
			at := w.ResetsAt
			win.ResetsAt = &at
		}
		out.Windows = append(out.Windows, win)
	}
	return out
}

// Quotas reports subscription headroom per harness for a running project.
//
// The freshest reading wins: every role on one harness draws from the same
// windows, so the newest report is the truest, and a role that has not taken a
// turn yet simply contributes nothing.
func (o *Overmind) Quotas(projectID string) Quotas {
	o.mu.Lock()
	s, ok := o.running[projectID]
	o.mu.Unlock()
	if !ok {
		return nil
	}

	seen := map[string]time.Time{}
	out := Quotas{}
	for _, c := range s.cerebrates {
		q, at := c.Quota()
		if q == nil {
			continue
		}
		key := q.Provider
		if key == "" {
			key = c.Harness() // a harness that did not say; better than dropping it
		}
		if prev, ok := seen[key]; ok && !at.After(prev) {
			continue
		}
		seen[key] = at
		out[key] = quotaReport(q, at)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Start brings a project's swarm up.
//
// The readiness gate comes first and is absolute: a team that cannot work must
// never reach a running board. Without it a launch always "succeeds" — sessions
// up, dashboard green, board drawn — while half the roles sit at a prompt they
// cannot pass and nothing anywhere says so.
func (o *Overmind) Start(ctx context.Context, projectID string) error {
	// Reserve the project before doing anything slow. Preflight, worktrees and
	// spawning all happen outside the lock — they shell out, and holding a
	// mutex across a subprocess is its own bug — so without a reservation two
	// concurrent Starts both pass the check and both bring a swarm up, leaving
	// two sets of agents on one queue.
	o.mu.Lock()
	if _, ok := o.running[projectID]; ok {
		o.mu.Unlock()
		return fmt.Errorf("this project is already running")
	}
	if o.starting == nil {
		o.starting = map[string]bool{}
	}
	if o.starting[projectID] {
		o.mu.Unlock()
		return fmt.Errorf("this project is already starting")
	}
	o.starting[projectID] = true
	o.mu.Unlock()

	// Everything acquired before the swarm is published has to be given back on
	// any failure, or a half-started project leaks agent tokens, an open
	// session and running processes that nothing can stop.
	started := false
	var (
		cancel  context.CancelFunc
		session *store.Session
		tokens  []string
	)
	defer func() {
		o.mu.Lock()
		delete(o.starting, projectID)
		o.mu.Unlock()
		if started {
			return
		}
		if cancel != nil {
			cancel()
		}
		for _, t := range tokens {
			o.agents.Revoke(t)
		}
		if session != nil {
			if err := o.db.EndSession(context.WithoutCancel(ctx), session.ID, "start failed"); err != nil {
				o.log.Warn("closing a session after a failed start", "project", projectID, "err", err)
			}
		}
	}()

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

	binDir, err := o.ensureAgentBin()
	if err != nil {
		return err
	}

	// Harness defaults, read at spawn like the prompts are, so changing them in
	// settings takes effect on the next start rather than the next release.
	cfg, err := o.db.GetConfig(ctx)
	if err != nil {
		return err
	}

	session, err = o.db.StartSession(ctx, projectID)
	if err != nil {
		return err
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	cancel = runCancel
	s := &swarm{
		session: session, cancel: runCancel, done: make(chan struct{}),
		cerebrates: map[string]*cerebrate.Cerebrate{},
	}

	for _, role := range team {
		if !role.Enabled {
			continue
		}
		a, err := o.registry.Get(role.Harness)
		if err != nil {
			return err
		}
		worktree, err := hatch.EnsureWorktree(ctx, role.Name, project.BaseBranch)
		if err != nil {
			return err
		}

		token := o.agents.Mint(projectID, role.Name)
		s.tokens = append(s.tokens, token)
		tokens = s.tokens

		// A private config directory per role, but only where the harness can
		// actually run with one. claude keeps credentials in the OS keychain
		// and a relocated directory leaves it unauthenticated, so isolating it
		// would trade a rare race for an agent that cannot work at all.
		configDir := ""
		if a.Capabilities().PrivateConfigDir {
			configDir = o.roleDir(projectID, role.Name, "config")
		}

		c := cerebrate.New(cerebrate.Config{
			ProjectID:    projectID,
			Role:         role,
			Adapter:      a,
			Worktree:     worktree,
			ConfigDir:    configDir,
			Socket:       o.agents.Path(),
			Token:        token,
			BinDir:       binDir,
			HarnessFlags: cfg.FlagsFor(role.Harness),
			SystemPrompt: composePrompt(shared, role.Prompt),
			Refresh:      o.refreshRole(projectID, role.Name),
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
	started = true

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

	// The agents are gone, so their claims are too. Left to lapse on their own
	// the work sits `claimed` for up to the full lease period, and a swarm
	// started again in the meantime stands idle next to a card that says it is
	// being worked on.
	if n, err := o.nyd.ReclaimLeases(ctx, projectID); err != nil {
		o.log.Warn("could not return in-flight work to the queue", "project", projectID, "err", err)
	} else if n > 0 {
		o.log.Info("returned in-flight work to the queue", "project", projectID, "leases", n)
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
	quota := time.NewTicker(quotaEvery)
	defer quota.Stop()
	// Once at start, so the gauge is populated before the first slow tick
	// rather than blank for two minutes.
	go o.refreshQuotas(ctx, s)

	for {
		select {
		case <-ctx.Done():
			return
		case <-quota.C:
			go o.refreshQuotas(ctx, s)
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

// refreshQuotas asks each harness that has to be asked. Off the tick
// goroutine: this call leaves the machine, and a slow endpoint must not delay
// lease expiry.
func (o *Overmind) refreshQuotas(ctx context.Context, s *swarm) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// One ask per harness, not per role: the windows belong to the account,
	// so three claude roles asking separately is three subprocesses for one
	// answer. The result is copied to the rest so every role reports it.
	//
	// Ranged without a lock, as nudgeIdle does: the map is fully populated
	// before the swarm is published and never written again.
	done := map[string]*cerebrate.Cerebrate{}
	for _, c := range s.cerebrates {
		if _, seen := done[c.Harness()]; seen {
			continue
		}
		done[c.Harness()] = c
		c.RefreshQuota(ctx)
	}
	for _, c := range s.cerebrates {
		if lead, ok := done[c.Harness()]; ok && lead != c {
			if q, at := lead.Quota(); q != nil {
				c.AdoptQuota(q, at)
			}
		}
	}
}

// nudgeIdle tells agents that can take work to go and claim it.
//
// The gate is "not mid-turn", not "has reported ready". A harness that only
// initialises once it receives its first message would otherwise never be sent
// one — no turn, no init, no ready, no turn. An agent already working is left
// alone: a second instruction inside work in progress is at best ignored.
//
// Because the queue is durable, a skipped nudge costs nothing; the next tick
// finds the same work still waiting.
func (o *Overmind) nudgeIdle(ctx context.Context, projectID string, s *swarm) {
	for role, c := range s.cerebrates {
		if !c.Idle() {
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

// refreshRole re-reads one role from the database, for the supervisor to call
// before each spawn.
//
// The team is resolved again rather than the template alone, because what
// matters is this project's view of the role: its position, whether it is still
// enabled, and any model or argument override.
func (o *Overmind) refreshRole(projectID, name string) func(context.Context) (cerebrate.Refreshed, error) {
	return func(ctx context.Context) (cerebrate.Refreshed, error) {
		team, err := o.db.ResolveTeam(ctx, projectID)
		if err != nil {
			return cerebrate.Refreshed{}, err
		}
		for _, r := range team {
			if r.Name != name {
				continue
			}
			if !r.Enabled {
				return cerebrate.Refreshed{Gone: true}, nil
			}
			shared, err := o.sharedInstructions(ctx)
			if err != nil {
				return cerebrate.Refreshed{}, err
			}
			cfg, err := o.db.GetConfig(ctx)
			if err != nil {
				return cerebrate.Refreshed{}, err
			}
			return cerebrate.Refreshed{
				Role:         r,
				SystemPrompt: composePrompt(shared, r.Prompt),
				HarnessFlags: cfg.FlagsFor(r.Harness),
			}, nil
		}
		return cerebrate.Refreshed{Gone: true}, nil
	}
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

// ensureAgentBin puts a `zerg` executable somewhere agents can find it.
//
// The prompts tell agents to run `zerg next`. Whether that resolves depends on
// how the daemon itself was started — by absolute path, from a build
// directory, from anywhere not already on PATH — and when it does not resolve
// the agent simply cannot do what it was asked, with no way to report why. A
// symlink under the state directory makes the name true regardless.
func (o *Overmind) ensureAgentBin() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the zerg binary: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolving the zerg binary: %w", err)
	}

	binDir := filepath.Join(o.stateDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", binDir, err)
	}

	link := filepath.Join(binDir, "zerg")
	// Replace rather than reuse: a link left by a previous build would point
	// at a binary that no longer matches this daemon's protocol.
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("clearing %s: %w", link, err)
	}
	if err := os.Symlink(self, link); err != nil {
		return "", fmt.Errorf("linking %s: %w", link, err)
	}
	return binDir, nil
}

func (o *Overmind) roleDir(projectID, role, kind string) string {
	return filepath.Join(o.stateDir, projectID, role, kind)
}

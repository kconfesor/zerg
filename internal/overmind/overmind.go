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

	// silenceLimit is how long an agent may be mid-turn and produce nothing
	// before the daemon says so.
	//
	// Five minutes: a build, a test suite or an install can legitimately run
	// that long with no output, and nothing legitimate is quiet for longer
	// while holding a card. A lease lasts twenty minutes, so this leaves time
	// to notice and act before the work is silently requeued.
	silenceLimit = 5 * time.Minute

	// quotaEvery is how often a harness that has to be asked for its
	// subscription quota is asked. Slow on purpose: the figure moves in
	// percent over hours, the call leaves the machine, and a gauge is not
	// worth a request every two seconds. Harnesses that report quota on their
	// own stream are not polled at all.
	quotaEvery = 2 * time.Minute

	// stopGrace is how long a stopping swarm has to let its agents exit before
	// the wait is abandoned and logged. Long enough for a harness to finish
	// writing its transcript and close its pipes; short enough that a wedged
	// process cannot hold the cockpit's Stop button.
	stopGrace = 10 * time.Second

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
	// gate serialises the lifecycle of one project: start, stop, reconcile and
	// delete cannot interleave for the same project, and never block another.
	//
	// A reservation flag was not enough. Stop removed the swarm from `running`
	// and then spent up to ten seconds tearing it down, so a Start arriving in
	// that window saw a stopped project, minted tokens, prepared worktrees and
	// spawned a second set of agents into the worktrees the first set was still
	// exiting from — two harnesses in one directory, both committing.
	gate map[string]*sync.Mutex
}

// swarm is one project's live state.
//
// roles is mutable: a team edit adds, removes and replaces cerebrates while the
// swarm runs, so every reader takes the lock rather than assuming the map was
// finished before the swarm was published.
type swarm struct {
	session *store.Session

	// ctx is the swarm's lifetime and cancel ends it. Held here rather than
	// passed around because a reconcile spawns a new role hours after Start
	// returned, and that role has to live and die with the same swarm.
	ctx    context.Context
	cancel context.CancelFunc

	// live counts every goroutine this swarm owns: one per cerebrate, plus
	// keepMoving. Stop waits on it, so "stopped" means the processes are gone
	// rather than that the supervisor loop noticed.
	live sync.WaitGroup

	mu    sync.Mutex
	roles map[string]*roleProc
}

// roleProc is one supervised role, with the handle to stop just that one.
type roleProc struct {
	cerebrate *cerebrate.Cerebrate
	cancel    context.CancelFunc
	token     string
	harness   string

	// reportedSilence stops one quiet agent from writing a line every tick.
	// Guarded by the swarm's lock, like the map that holds this.
	reportedSilence bool
}

// snapshot copies the live roles, so callers iterate without holding the lock
// across database or subprocess work.
func (s *swarm) snapshot() map[string]*roleProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*roleProc, len(s.roles))
	for name, p := range s.roles {
		out[name] = p
	}
	return out
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
		gate:    map[string]*sync.Mutex{},
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
	Quota *QuotaReport `json:"quota,omitempty"`

	// QuietFor is how many seconds this role has been mid-turn without
	// producing anything. Zero unless it has passed the point where silence
	// stops being a long build: "working" and "wedged" look identical on a
	// board, and this is the difference.
	QuietFor int  `json:"quietFor,omitempty"`
	Terminal bool `json:"terminal"`
}

// Running reports whether a project's swarm is up.
func (o *Overmind) Running(projectID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.running[projectID]
	return ok
}

// Interrupt tells a role, mid-turn, that something changed under it.
//
// Stopping a card closed its routes and released its lease, and the agent
// holding it carried on regardless: nothing in the queue reaches a process that
// is already working. Observed: a card stopped at 15:41 whose role was still
// running tool calls at 15:47, spending on work no longer wanted.
//
// This is cooperative and says so. The text is queued as the agent's next turn,
// so it lands when the current one ends rather than killing it — a harness
// interrupted mid-tool-call leaves the worktree in whatever state that call got
// to. The guard in nydus is what makes it safe to be cooperative: whatever the
// agent does after this, its handoff for that card is refused.
//
// Reports whether the role was there to be told.
func (o *Overmind) Interrupt(projectID, role, text string) bool {
	o.mu.Lock()
	s, ok := o.running[projectID]
	o.mu.Unlock()
	if !ok {
		return false
	}
	p, ok := s.snapshot()[role]
	if !ok {
		return false
	}
	if err := p.cerebrate.Submit(text); err != nil {
		o.log.Debug("could not interrupt", "role", role, "err", err)
		return false
	}
	return true
}

// Status returns each role's live state, or nil when the project is stopped.
func (o *Overmind) Status(ctx context.Context, projectID string) ([]Status, error) {
	o.mu.Lock()
	s, ok := o.running[projectID]
	o.mu.Unlock()
	if !ok {
		return nil, nil
	}

	live := s.snapshot()
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
		if p, ok := live[role.Name]; ok {
			c := p.cerebrate
			st.State, st.LastError, st.Restarts = c.State(), c.LastError(), c.Restarts()
			if until := c.ThrottledUntil(); !until.IsZero() {
				st.ThrottledUntil = &until
			}
			if quiet := c.Silence(); quiet >= silenceLimit {
				st.QuietFor = int(quiet.Seconds())
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
	for _, p := range s.snapshot() {
		c := p.cerebrate
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
	// One lifecycle at a time for this project. Preflight, worktrees and
	// spawning all shell out, and holding a lock across a subprocess is its own
	// bug — but the lock this takes is per project, so a slow start blocks only
	// further lifecycle changes to the same project. Two concurrent Starts no
	// longer both pass the check, and a Start can no longer overtake a Stop
	// that is still tearing agents down.
	unlock := o.lock(projectID)
	defer unlock()

	o.mu.Lock()
	_, up := o.running[projectID]
	o.mu.Unlock()
	if up {
		return fmt.Errorf("this project is already running")
	}

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

	report, err := o.preflight.Check(ctx, projectID)
	if err != nil {
		return err
	}
	if !report.Ready {
		return &ErrNotReady{Report: report}
	}

	spawn, err := o.spawnContext(ctx, projectID)
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
		session: session, ctx: runCtx, cancel: runCancel,
		roles: map[string]*roleProc{},
	}

	for _, role := range spawn.team {
		if !role.Enabled {
			continue
		}
		if err := o.spawnRole(runCtx, s, spawn, role); err != nil {
			return err
		}
		tokens = append(tokens, s.roles[role.Name].token)
	}

	o.mu.Lock()
	o.running[projectID] = s
	o.mu.Unlock()
	started = true

	// After the swarm is up, not before: a project that failed preflight was
	// never running, and recording an intent for it would have the next daemon
	// start try again on its own and fail again, unattended, for as long as
	// whatever is broken stays broken.
	if err := o.db.RequestStart(context.WithoutCancel(ctx), projectID); err != nil {
		// Not fatal. The swarm is running either way; what is lost is its
		// coming back by itself after a restart.
		o.log.Warn("could not record that this project should be running",
			"project", projectID, "err", err)
	}

	s.live.Add(1)
	go func() {
		defer s.live.Done()
		o.keepMoving(runCtx, projectID, s)
	}()

	o.log.Info("swarm up", "project", spawn.project.Name, "roles", len(s.roles))
	return nil
}

// lock takes this project's lifecycle lock and returns the release.
func (o *Overmind) lock(projectID string) func() {
	o.mu.Lock()
	m, ok := o.gate[projectID]
	if !ok {
		m = &sync.Mutex{}
		o.gate[projectID] = m
	}
	o.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// spawnEnv is everything a spawn needs that is the same for every role: what
// the database says the team is, where the repository is, and the settings read
// once for this operation.
type spawnEnv struct {
	project *store.Project
	team    []store.ResolvedRole
	hatch   *hatchery.Hatchery
	shared  string
	binDir  string
	cfg     store.Config
}

// spawnContext gathers what a start or a reconcile needs before it can spawn
// anything, reading each of them once.
func (o *Overmind) spawnContext(ctx context.Context, projectID string) (*spawnEnv, error) {
	project, err := o.db.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	team, err := o.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return nil, err
	}
	shared, err := o.sharedInstructions(ctx)
	if err != nil {
		return nil, err
	}

	// Worktrees before anything spawns: an agent whose directory does not exist
	// fails in a way that reads as a harness problem.
	hatch := hatchery.New(project.Path)
	if err := hatch.EnsureRepo(ctx, project.BaseBranch); err != nil {
		return nil, err
	}
	binDir, err := o.ensureAgentBin()
	if err != nil {
		return nil, err
	}
	// Harness defaults, read at spawn like the prompts are, so changing them in
	// settings takes effect on the next start rather than the next release.
	cfg, err := o.db.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &spawnEnv{project: project, team: team, hatch: hatch,
		shared: shared, binDir: binDir, cfg: cfg}, nil
}

// spawnRole brings one role up and registers it on the swarm.
func (o *Overmind) spawnRole(runCtx context.Context, s *swarm, env *spawnEnv, role store.ResolvedRole) error {
	projectID := env.project.ID
	a, err := o.registry.Get(role.Harness)
	if err != nil {
		return err
	}
	worktree, err := env.hatch.EnsureWorktree(runCtx, role.Name, env.project.BaseBranch)
	if err != nil {
		return err
	}

	token := o.agents.Mint(projectID, role.Name)

	// A private config directory per role, but only where the harness can
	// actually run with one. claude keeps credentials in the OS keychain
	// and a relocated directory leaves it unauthenticated, so isolating it
	// would trade a rare race for an agent that cannot work at all.
	configDir := ""
	if a.Capabilities().PrivateConfigDir {
		configDir = o.roleDir(projectID, role.Name, "config")
	}

	// One context per role, derived from the swarm's. Cancelling the swarm
	// still stops everything; a role that the team no longer has can also be
	// stopped on its own, which is what makes a live team edit possible
	// without taking the rest of the pipeline down with it.
	roleCtx, roleCancel := context.WithCancel(runCtx)
	c := cerebrate.New(cerebrate.Config{
		ProjectID:    projectID,
		Role:         role,
		Adapter:      a,
		Worktree:     worktree,
		ConfigDir:    configDir,
		Socket:       o.agents.Path(),
		Token:        token,
		BinDir:       env.binDir,
		HarnessFlags: env.cfg.FlagsFor(role.Harness),
		SystemPrompt: composePrompt(env.shared, role.Prompt),
		Refresh:      o.refreshRole(projectID, role.Name),
		Bus:          o.bus,
		Log:          o.log,
		Preflight:    o.preflight,
		StateDir:     o.roleDir(projectID, role.Name, "state"),
		Continuity:   continuity{db: o.db, projectID: projectID, log: o.log},
	})

	s.mu.Lock()
	s.roles[role.Name] = &roleProc{
		cerebrate: c, cancel: roleCancel, token: token, harness: role.Harness,
	}
	s.mu.Unlock()

	s.live.Add(1)
	go func() {
		defer s.live.Done()
		defer roleCancel()
		_ = c.Run(roleCtx)
	}()
	return nil
}

// continuity is the pipeline's answer to "which conversation was this role
// holding", kept in the database so it outlives the process that asked.
//
// Failures here are logged and swallowed on purpose. Not knowing which session
// to resume costs an agent its memory of the last run, which is the situation
// every spawn was in before this existed; refusing to spawn over it would trade
// a degraded start for no start at all.
type continuity struct {
	db        *store.DB
	projectID string
	log       *slog.Logger
}

func (c continuity) Resume(ctx context.Context, role, harness, fingerprint string) string {
	sess, err := c.db.RoleSessionFor(ctx, c.projectID, role, harness, fingerprint)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Either nothing was ever recorded, or what was recorded belongs to a
		// different configuration. Both mean a cold session, and neither is
		// worth a line in the log on every spawn.
		return ""
	case err != nil:
		c.log.Warn("could not look up a session to resume", "project", c.projectID, "role", role, "err", err)
		return ""
	}
	return sess.SessionID
}

func (c continuity) Record(ctx context.Context, role, harness, sessionID, fingerprint string) {
	if err := c.db.SaveRoleSession(ctx, c.projectID, role, harness, sessionID, fingerprint); err != nil {
		c.log.Warn("could not record the session this role is holding",
			"project", c.projectID, "role", role, "err", err)
	}
}

func (c continuity) Forget(ctx context.Context, role string) {
	if err := c.db.ForgetRoleSession(ctx, c.projectID, role); err != nil {
		c.log.Warn("could not forget a session that is no longer there",
			"project", c.projectID, "role", role, "err", err)
	}
}

// stopRole takes one role down and gives its token back.
//
// It does not wait: the caller either waits on the whole swarm or is replacing
// this role, and a replacement gets its own worktree lock from git rather than
// sharing one with the process it replaces.
func (o *Overmind) stopRole(s *swarm, name string) {
	s.mu.Lock()
	p, ok := s.roles[name]
	delete(s.roles, name)
	s.mu.Unlock()
	if !ok {
		return
	}
	p.cancel()
	o.agents.Revoke(p.token)
}

// Reconcile brings a running swarm back in line with the team as configured.
//
// A team edit used to reach only the roles that happened to respawn: an added
// role got no process at all, so work routed to it sat in a queue nothing was
// reading; a removed role kept working until it next crashed; a renamed one
// stopped with no replacement; and a role whose harness changed kept its
// original adapter and config directory while its model and flags came from the
// new one — running claude with codex's arguments, or the reverse.
//
// Doing nothing is correct when the project is stopped: the next Start reads
// the team as it now is.
func (o *Overmind) Reconcile(ctx context.Context, projectID string) error {
	unlock := o.lock(projectID)
	defer unlock()

	o.mu.Lock()
	s, up := o.running[projectID]
	o.mu.Unlock()
	if !up {
		return nil
	}

	env, err := o.spawnContext(ctx, projectID)
	if err != nil {
		return err
	}

	wanted := map[string]store.ResolvedRole{}
	for _, role := range env.team {
		if role.Enabled {
			wanted[role.Name] = role
		}
	}

	// Gone first, so a rename frees its worktree before the new name asks for
	// one, and so a removed role stops taking work it will never finish.
	for name, p := range s.snapshot() {
		role, keep := wanted[name]
		if keep && role.Harness == p.harness {
			continue
		}
		if keep {
			o.log.Info("role changed harness, replacing it",
				"project", projectID, "role", name, "from", p.harness, "to", role.Harness)
		} else {
			o.log.Info("role left the team, stopping it", "project", projectID, "role", name)
		}
		o.stopRole(s, name)
	}

	var failed []string
	for name, role := range wanted {
		s.mu.Lock()
		_, live := s.roles[name]
		s.mu.Unlock()
		if live {
			continue
		}
		if err := o.spawnRole(s.ctx, s, env, role); err != nil {
			// One role that cannot spawn must not take the rest of the team
			// down: the others are working, and the operator needs to be told
			// which one failed rather than finding the swarm gone.
			o.log.Error("could not bring a role up", "project", projectID, "role", name, "err", err)
			failed = append(failed, name)
			continue
		}
		o.log.Info("role joined the team, starting it", "project", projectID, "role", name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("these roles could not be started: %s", strings.Join(failed, ", "))
	}
	return nil
}

// Stop takes a project's swarm down.
//
// It holds the project's lifecycle lock for the whole teardown, so a Start
// arriving mid-stop waits rather than spawning a second set of agents into
// worktrees the first set has not finished leaving.
// Stop takes a project down because somebody asked for it.
//
// This is the operator's decision and it is recorded as one: the project stops
// wanting to be running, and its roles forget the conversations they were
// holding, so the next Start is a genuinely fresh one. StopAll, which is the
// daemon going down, does neither — see the difference described on
// store.WithdrawStart.
func (o *Overmind) Stop(ctx context.Context, projectID, reason string) error {
	unlock := o.lock(projectID)
	defer unlock()
	if err := o.stop(ctx, projectID, reason); err != nil {
		return err
	}
	o.forgetContinuity(ctx, projectID)
	return nil
}

// forgetContinuity drops everything that would make the next start a resumed
// one. Both failures are warnings: the swarm is already down, and the worst
// case is a project that starts itself once more, or an agent that comes back
// remembering a task that is finished.
func (o *Overmind) forgetContinuity(ctx context.Context, projectID string) {
	ctx = context.WithoutCancel(ctx)
	if err := o.db.WithdrawStart(ctx, projectID); err != nil {
		o.log.Warn("could not record that this project should stay stopped",
			"project", projectID, "err", err)
	}
	if n, err := o.db.ForgetRoleSessions(ctx, projectID); err != nil {
		o.log.Warn("could not clear this project's harness sessions",
			"project", projectID, "err", err)
	} else if n > 0 {
		o.log.Info("cleared harness sessions on stop", "project", projectID, "roles", n)
	}
}

// stop is the body, for callers that already hold the lifecycle lock.
func (o *Overmind) stop(ctx context.Context, projectID, reason string) error {
	o.mu.Lock()
	s, ok := o.running[projectID]
	if !ok {
		o.mu.Unlock()
		return fmt.Errorf("this project is not running")
	}
	delete(o.running, projectID)
	o.mu.Unlock()

	s.cancel()

	// Wait for the processes, not for the supervisor loop.
	//
	// This used to wait on `done`, which keepMoving closes — a goroutine that
	// does nothing but tick, so it returns immediately while harnesses are
	// still shutting down. Stop then reclaimed the leases and returned, and a
	// Start straight afterwards put a new agent into a worktree the old one was
	// still writing to. Every cerebrate is counted in `live`, so waiting on it
	// means what it says.
	stopped := make(chan struct{})
	go func() {
		s.live.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopGrace):
		o.log.Warn("swarm did not stop cleanly in time", "project", projectID)
	}

	// Tokens die with the swarm: a leftover token is a capability nobody is
	// watching any more.
	for _, p := range s.snapshot() {
		o.agents.Revoke(p.token)
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

	// The services those agents started went with them. The rows outlive the
	// processes, and a port that is free again is a port something else can
	// take: a link to a stopped dev server that silently reaches whatever
	// bound 5173 afterwards is worse than a link that says it is gone.
	//
	// The agents' only. A preview the daemon is running belongs to whoever
	// wanted to click the app, and the pipeline finishing is when they want to
	// click it.
	if n, err := o.db.StopServices(ctx, projectID, store.OwnerAgent); err != nil {
		o.log.Warn("could not mark this project's services stopped", "project", projectID, "err", err)
	} else if n > 0 {
		o.log.Info("services stopped with the swarm", "project", projectID, "services", n)
	}

	o.log.Info("swarm down", "project", projectID, "reason", reason)
	return nil
}

// StopFor takes a project's swarm down if it is running, and reports whether
// there was one. For callers that need the project quiet — deleting it — rather
// than callers acting on a button.
func (o *Overmind) StopFor(ctx context.Context, projectID, reason string) (bool, error) {
	unlock := o.lock(projectID)
	defer unlock()

	o.mu.Lock()
	_, up := o.running[projectID]
	o.mu.Unlock()
	if !up {
		// Still an operator saying stop, even with nothing running: a project
		// whose swarm died and was never restarted still holds the intent, and
		// leaving it set would have the daemon start it again.
		o.forgetContinuity(ctx, projectID)
		return false, nil
	}
	if err := o.stop(ctx, projectID, reason); err != nil {
		return true, err
	}
	o.forgetContinuity(ctx, projectID)
	return true, nil
}

// Resume starts the projects the operator left running when the daemon last
// went down, and reports how many came back up.
//
// Deliberately not a stricter thing than Start. Every project goes through the
// same preflight and the same spawn, so a resumed swarm is a started swarm and
// there is no second path to keep correct. What is different is what a failure
// means: nobody is watching, so a project that cannot start is logged and left
// alone rather than reported, and it keeps its intent so the next daemon start
// tries again. A harness that is logged out at boot and logged in at lunchtime
// should not need the operator to remember which projects were running.
func (o *Overmind) Resume(ctx context.Context) (int, error) {
	ids, err := o.db.ProjectsWantingStart(ctx)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return started, ctx.Err()
		}
		name := id
		if p, err := o.db.GetProject(ctx, id); err == nil {
			name = p.Name
		}
		// Counted before the start, because a start that fails still says
		// something worth knowing: these are the conversations that were
		// waiting to be picked up, and a project that cannot come back is a
		// project whose agents are about to lose them to the next edit.
		held := 0
		if sessions, err := o.db.ListRoleSessions(ctx, id); err == nil {
			held = len(sessions)
		}

		switch err := o.Start(ctx, id); {
		case err == nil:
			started++
			o.log.Info("resumed a project that was running before the restart",
				"project", name, "conversations", held)
		default:
			// Including ErrNotReady, which is the common one: a preflight check
			// that fails at boot is usually a harness that has not finished
			// starting or a token that needs a person.
			o.log.Warn("could not resume a project that was running before the restart",
				"project", name, "err", err)
		}
	}
	return started, nil
}

// StopAll shuts every running project down, for daemon shutdown.
//
// Deliberately not Stop, which is the operator's decision and withdraws the
// intent to be running along with the conversations the agents were holding.
// A daemon going down decides nothing: it takes the processes with it and
// leaves every record of what should be running exactly as it found it, which
// is what lets the next start put it all back.
func (o *Overmind) StopAll(ctx context.Context, reason string) {
	o.mu.Lock()
	ids := make([]string, 0, len(o.running))
	for id := range o.running {
		ids = append(ids, id)
	}
	o.mu.Unlock()

	for _, id := range ids {
		unlock := o.lock(id)
		err := o.stop(ctx, id, reason)
		unlock()
		if err != nil {
			o.log.Warn("stopping project", "project", id, "err", err)
		}
	}
}

// keepMoving runs the two duties that keep a pipeline from stalling.
func (o *Overmind) keepMoving(ctx context.Context, projectID string, s *swarm) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	quota := time.NewTicker(quotaEvery)
	defer quota.Stop()
	// Once at start, so the gauge is populated before the first slow tick
	// rather than blank for two minutes.
	//
	// Counted on the swarm like every other goroutine it owns: this one shells
	// out to a harness, and a Stop that returned while it was still running
	// would report the swarm down with a subprocess of it still alive. Safe to
	// Add from here because keepMoving is itself counted, so the total is never
	// zero while this runs.
	refresh := func() {
		s.live.Add(1)
		go func() {
			defer s.live.Done()
			o.refreshQuotas(ctx, s)
		}()
	}
	refresh()

	for {
		select {
		case <-ctx.Done():
			return
		case <-quota.C:
			refresh()
		case <-ticker.C:
			// Lapsed leases first: requeued work should be visible to the
			// nudge that follows in the same tick.
			if n, err := o.nyd.ExpireLeases(ctx); err != nil {
				o.log.Warn("expiring leases", "err", err)
			} else if n > 0 {
				o.log.Info("returned unacknowledged work to the queue", "leases", n)
			}
			o.nudgeIdle(ctx, projectID, s)
			o.watchForSilence(projectID, s)
		}
	}
}

// watchForSilence reports an agent that is mid-turn and has stopped producing
// anything.
//
// Reported, not killed. A long build is indistinguishable from a hung command
// from out here, and the daemon guessing wrong would throw away a turn somebody
// paid for. What this buys is that a person is told: the alternative was
// finding out because a card sat in "working" for eight minutes and somebody
// happened to be watching the board.
//
// Once per stretch of silence, not once per tick, which would be a line every
// two seconds for as long as it lasts.
func (o *Overmind) watchForSilence(projectID string, s *swarm) {
	type report struct {
		role  string
		quiet time.Duration
	}
	var say []report

	s.mu.Lock()
	for role, p := range s.roles {
		quiet := p.cerebrate.Silence()
		switch {
		case quiet < silenceLimit:
			p.reportedSilence = false
		case !p.reportedSilence:
			p.reportedSilence = true
			say = append(say, report{role, quiet})
		}
	}
	s.mu.Unlock()

	for _, r := range say {
		o.log.Warn("an agent is mid-turn and has gone quiet",
			"project", projectID, "role", r.role, "quiet", r.quiet.Round(time.Second),
			"note", "a long build looks like this; so does a command that will never return")
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
	// Over a snapshot: a team edit can add or replace a role while this is out
	// asking a harness, and the map it walks must not change underneath it.
	live := s.snapshot()
	done := map[string]*cerebrate.Cerebrate{}
	for _, p := range live {
		c := p.cerebrate
		if _, seen := done[c.Harness()]; seen {
			continue
		}
		done[c.Harness()] = c
		c.RefreshQuota(ctx)
	}
	for _, p := range live {
		c := p.cerebrate
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
	for role, p := range s.snapshot() {
		c := p.cerebrate
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
// AgentBin stages the zerg binary agents are told to run and returns the
// directory holding it.
//
// Exported because agents outside the pipeline need it too: a runner is
// spawned without a swarm and must still be able to call `zerg artifact
// serve`. Without it the first real session spent part of its turn hunting for
// the binary.
func (o *Overmind) AgentBin() (string, error) { return o.ensureAgentBin() }

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
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", binDir, err)
	}
	// The state directory is the operator's alone, and MkdirAll only sets the
	// mode on directories it creates — so an upgraded installation keeps
	// whatever the previous version made until something tightens it. Best
	// effort: a state directory somewhere this process cannot chmod is a
	// warning, not a reason to refuse to start a swarm.
	for _, dir := range []string{o.stateDir, binDir} {
		if err := os.Chmod(dir, 0o700); err != nil && !os.IsNotExist(err) {
			o.log.Warn("could not tighten a state directory", "dir", dir, "err", err)
		}
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

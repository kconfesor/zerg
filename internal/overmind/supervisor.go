package overmind

import (
	"context"
	"errors"
	"fmt"

	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/cerebrate"
	"github.com/kconfesor/zerg/internal/store"
)

// SupervisorState is what the cockpit needs to tell "an architect is deciding"
// from "an architect was asked for and never started".
//
// The badge used to be drawn from tasks.supervised alone, which is a request,
// not an outcome. A missing supervisor role or a harness that would not spawn
// was logged in the daemon and nowhere else, so the board said a decision was
// being made while nothing was making it. Both causes are operator-fixable,
// which is exactly the kind of failure that has to reach a person.
type SupervisorState struct {
	// Wanted is whether a live card asked for a sidecar.
	Wanted bool `json:"wanted"`
	// Live is whether a process is running right now.
	Live bool `json:"live"`
	// Role is the library role the sidecar runs as, when there is one.
	Role string `json:"role,omitempty"`
	// Error is why there is no process despite the project running and a card
	// wanting one, in words an operator can act on: a missing role, a harness
	// that would not start. Empty when nothing is wrong, and empty on a
	// stopped project, which is not a fault.
	Error string `json:"error,omitempty"`
}

// noSupervisorRole is the operator-fixable cause with no error to report from:
// nothing failed, there is simply nothing in the library to run.
const noSupervisorRole = "no role in the library has the supervisor purpose"

// SupervisorState reports whether the architect sidecar is actually there.
func (o *Overmind) SupervisorState(ctx context.Context, projectID string) SupervisorState {
	var st SupervisorState
	want, err := o.db.HasWorkForSupervisor(ctx, projectID)
	if err != nil {
		return SupervisorState{Error: err.Error()}
	}
	st.Wanted = want

	o.mu.Lock()
	s, up := o.running[projectID]
	o.mu.Unlock()
	// A stopped project has no sidecar and that is not a fault: the operator
	// stopped it and can see so. Error is for the cases they would otherwise
	// never learn about.
	if !up {
		return st
	}

	s.mu.Lock()
	p := s.supervisor
	st.Error = s.supervisorErr
	s.mu.Unlock()
	if p != nil {
		st.Live = true
		st.Role = p.cerebrate.Role().Name
		st.Error = ""
	}
	return st
}

// SyncSupervisor summons or retires the architect sidecar for a running swarm.
//
// Called after a card is marked supervised, after Start, and on the tick, so
// a process is not left idle once every supervised card has finished, and is
// not missing when one is queued while the swarm is already up.
func (o *Overmind) SyncSupervisor(ctx context.Context, projectID string) error {
	unlock := o.lock(projectID)
	defer unlock()
	return o.syncSupervisorLocked(ctx, projectID)
}

// syncSupervisorTick is SyncSupervisor for the swarm's own ticker, which must
// never block on the lifecycle lock. See tryLock.
func (o *Overmind) syncSupervisorTick(ctx context.Context, projectID string) error {
	unlock, got := o.tryLock(projectID)
	if !got {
		return nil
	}
	defer unlock()
	return o.syncSupervisorLocked(ctx, projectID)
}

func (o *Overmind) syncSupervisorLocked(ctx context.Context, projectID string) error {
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
	return o.syncSupervisor(s.ctx, s, env)
}

func (o *Overmind) syncSupervisor(ctx context.Context, s *swarm, env *spawnEnv) error {
	want, err := o.db.HasWorkForSupervisor(ctx, env.project.ID)
	if err != nil {
		return err
	}

	if !want {
		s.mu.Lock()
		live := s.supervisor != nil
		s.mu.Unlock()
		if live {
			o.stopSupervisor(s)
			o.log.Info("no supervised cards left, stopping the architect",
				"project", env.project.ID)
		}
		// A failure belongs to the stretch of work that wanted a sidecar. The
		// next supervised card is a fresh attempt, not a retry of that one.
		o.clearSupervisorFailure(s)
		return nil
	}

	// Read the configuration before looking at the process, because what the
	// process should be is half of whether the one running is still right.
	tpl, err := o.db.RoleFor(ctx, store.PurposeSupervisor)
	if errors.Is(err, store.ErrNotFound) {
		o.failSupervisor(s, env.project.ID, "", noSupervisorRole)
		return nil
	}
	if err != nil {
		return err
	}
	role := store.ResolvedRole{RoleTemplate: *tpl, Enabled: true}
	ident := supervisorIdent(role)

	// A live process is only a live *correct* process if it is running the
	// role and harness the library now names. Refresh re-reads the prompt and
	// the flags but cannot swap the adapter the process was built with, so a
	// harness change left one harness running with another's arguments — the
	// failure Reconcile already handles for pipeline roles.
	s.mu.Lock()
	p := s.supervisor
	failed := s.supervisorErr != ""
	failedFor := s.supervisorErrFor
	s.mu.Unlock()

	if p != nil {
		if supervisorIdentOf(p) == ident {
			return nil
		}
		o.log.Info("the supervisor role changed, replacing the architect",
			"project", env.project.ID, "from", supervisorIdentOf(p), "to", ident)
		o.stopSupervisor(s)
		o.clearSupervisorFailure(s)
	} else if failed {
		// Do not respawn into the same fatal error. cerebrate already refuses
		// to restart a process that died fatally, and a tick that started a
		// replacement every two seconds would undo that from the outside.
		// A configuration change is a new thing to try, so it clears this.
		if failedFor == ident {
			return nil
		}
		o.clearSupervisorFailure(s)
	}

	if err := o.spawnSupervisor(ctx, s, env, role); err != nil {
		o.failSupervisor(s, env.project.ID, ident, err.Error())
		return err
	}
	o.log.Info("architect sidecar up", "project", env.project.ID, "role", role.Name)
	return nil
}

// supervisorIdent is what has to match for a running sidecar to still be the
// right one: which role it is, and which harness it runs on.
func supervisorIdent(role store.ResolvedRole) string {
	return role.Name + "@" + role.Harness
}

func supervisorIdentOf(p *roleProc) string {
	return p.cerebrate.Role().Name + "@" + p.harness
}

// failSupervisor records why a card that asked for an architect has not got
// one, for the cockpit to render and for the tick to stop retrying.
func (o *Overmind) failSupervisor(s *swarm, projectID, ident, reason string) {
	s.mu.Lock()
	first := s.supervisorErr != reason || s.supervisorErrFor != ident
	s.supervisorErr = reason
	s.supervisorErrFor = ident
	s.mu.Unlock()
	if first {
		o.log.Error("a card is supervised but the architect is not running",
			"project", projectID, "reason", reason)
	}
}

func (o *Overmind) clearSupervisorFailure(s *swarm) {
	s.mu.Lock()
	s.supervisorErr, s.supervisorErrFor = "", ""
	s.mu.Unlock()
}

func (o *Overmind) spawnSupervisor(runCtx context.Context, s *swarm, env *spawnEnv, role store.ResolvedRole) error {
	a, err := o.registry.Get(role.Harness)
	if err != nil {
		return err
	}
	worktree, err := env.hatch.EnsureWorktree(runCtx, role.Name, env.project.BaseBranch)
	if err != nil {
		return err
	}

	token := o.agents.MintScoped(env.project.ID, role.Name,
		agent.CanClaim, agent.CanAsk, agent.CanDecide, agent.CanSplit)

	configDir := ""
	if a.Capabilities().PrivateConfigDir {
		configDir = o.roleDir(env.project.ID, role.Name, "config")
	}

	roleCtx, roleCancel := context.WithCancel(runCtx)
	c := cerebrate.New(cerebrate.Config{
		ProjectID:    env.project.ID,
		Role:         role,
		Adapter:      a,
		Worktree:     worktree,
		ConfigDir:    configDir,
		Socket:       o.agents.Path(),
		Token:        token,
		BinDir:       env.binDir,
		HarnessFlags: env.cfg.FlagsFor(role.Harness),
		SystemPrompt: composePrompt(env.shared, role.Prompt),
		Refresh:      o.refreshSupervisor(env.project.ID),
		Bus:          o.bus,
		Log:          o.log,
		Preflight:    o.preflight,
		StateDir:     o.roleDir(env.project.ID, role.Name, "state"),
		Continuity:   continuity{db: o.db, projectID: env.project.ID, log: o.log},
	})

	p := &roleProc{
		cerebrate: c, cancel: roleCancel, token: token, harness: role.Harness,
	}
	ident := supervisorIdent(role)

	s.mu.Lock()
	s.supervisor = p
	s.mu.Unlock()

	projectID := env.project.ID
	s.live.Add(1)
	go func() {
		defer s.live.Done()
		defer roleCancel()
		err := c.Run(roleCtx)

		// Take the pointer back down with the process. Compared against p, so
		// a replacement started while this one was ending is not cleared by
		// its predecessor.
		s.mu.Lock()
		mine := s.supervisor == p
		if mine {
			s.supervisor = nil
		}
		s.mu.Unlock()
		if !mine {
			return
		}
		o.agents.Revoke(token)
		// A cancelled context is the swarm stopping or a deliberate retire,
		// which is not a failure to report.
		if roleCtx.Err() != nil {
			return
		}
		// Run returns nil on a fatal error: it records the reason on the
		// cerebrate and stops, which is deliberate -- restarting into the same
		// wall is what StateFailed exists to prevent. Reading only the return
		// value would have found nothing wrong with a sidecar that had given
		// up.
		reason := ""
		if err != nil {
			reason = err.Error()
		}
		if reason == "" && c.State() == cerebrate.StateFailed {
			reason = c.LastError()
			if reason == "" {
				reason = "the architect harness stopped and will not restart"
			}
		}
		if reason != "" {
			o.failSupervisor(s, projectID, ident, reason)
		}
	}()
	return nil
}

func (o *Overmind) refreshSupervisor(projectID string) func(context.Context) (cerebrate.Refreshed, error) {
	return func(ctx context.Context) (cerebrate.Refreshed, error) {
		tpl, err := o.db.RoleFor(ctx, store.PurposeSupervisor)
		if errors.Is(err, store.ErrNotFound) {
			return cerebrate.Refreshed{Gone: true}, nil
		}
		if err != nil {
			return cerebrate.Refreshed{}, err
		}
		// A harness change is not something a refresh can apply: the adapter
		// belongs to the process. Report it as gone so this one ends, and let
		// syncSupervisor start the replacement on the right harness.
		o.mu.Lock()
		s, up := o.running[projectID]
		o.mu.Unlock()
		if up {
			s.mu.Lock()
			p := s.supervisor
			s.mu.Unlock()
			if p != nil && p.harness != tpl.Harness {
				return cerebrate.Refreshed{}, fmt.Errorf(
					"the supervisor role moved from %s to %s", p.harness, tpl.Harness)
			}
		}
		shared, err := o.sharedInstructions(ctx)
		if err != nil {
			return cerebrate.Refreshed{}, err
		}
		cfg, err := o.db.GetConfig(ctx)
		if err != nil {
			return cerebrate.Refreshed{}, err
		}
		role := store.ResolvedRole{RoleTemplate: *tpl, Enabled: true}
		return cerebrate.Refreshed{
			Role:         role,
			SystemPrompt: composePrompt(shared, role.Prompt),
			HarnessFlags: cfg.FlagsFor(role.Harness),
		}, nil
	}
}

func (o *Overmind) stopSupervisor(s *swarm) {
	s.mu.Lock()
	p := s.supervisor
	s.supervisor = nil
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.cancel()
	o.agents.Revoke(p.token)
}

func (o *Overmind) nudgeSupervisor(ctx context.Context, projectID string, s *swarm) {
	s.mu.Lock()
	p := s.supervisor
	s.mu.Unlock()
	if p == nil || !p.cerebrate.Idle() {
		return
	}
	d, err := o.db.NextDecision(ctx, projectID, p.cerebrate.Role().Name)
	if err != nil || d == nil {
		return
	}
	if err := p.cerebrate.Submit(nudge); err != nil {
		o.log.Debug("could not nudge the architect", "err", err)
	}
}

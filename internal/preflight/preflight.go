// Package preflight decides whether a team can actually work before it reaches
// a running board.
//
// This is the subsystem the predecessor lacked. A day of running it produced
// four separate hangs — a corrupted global config, a CLI too old for its model,
// an unanswered trust dialog, a broken plugin tree — and every one presented
// identically: an agent that looked alive and did nothing. The launch itself
// always succeeded. Six tmux sessions came up, the dashboard served, the board
// drew, and four roles sat at a prompt they could not pass.
//
// So readiness is checked across the whole team before Start, not only per
// spawn. A team that cannot work must never reach a running board.
package preflight

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/store"
)

// Status is the outcome of one check or one role.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusBlocked Status = "blocked"
)

// CheckResult is one probe's verdict, carrying what to do about it.
type CheckResult struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Reason string `json:"reason,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}

// RoleReport is one row of the readiness panel.
type RoleReport struct {
	Role    string        `json:"role"`
	Harness string        `json:"harness"`
	Model   string        `json:"model"`
	Status  Status        `json:"status"`
	Checks  []CheckResult `json:"checks"`
}

// Report is the whole panel. Ready is what gates the Start button.
type Report struct {
	ProjectID string       `json:"projectId"`
	Ready     bool         `json:"ready"`
	Roles     []RoleReport `json:"roles"`
	CheckedAt time.Time    `json:"checkedAt"`
}

// Runner probes a team's roles against the adapter registry.
type Runner struct {
	db       *store.DB
	registry *adapter.Registry
	timeout  time.Duration
	now      func() time.Time
}

type Option func(*Runner)

// WithTimeout bounds each individual check. A probe that hangs is itself a
// finding, and must not hang the panel with it.
func WithTimeout(d time.Duration) Option { return func(r *Runner) { r.timeout = d } }

func WithClock(f func() time.Time) Option { return func(r *Runner) { r.now = f } }

func NewRunner(db *store.DB, reg *adapter.Registry, opts ...Option) *Runner {
	r := &Runner{
		db: db, registry: reg,
		timeout: 10 * time.Second,
		now:     func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Check runs every check for every enabled role in a project's team.
//
// Roles are probed concurrently: a readiness panel that takes eight seconds
// because eight version probes ran in sequence is a panel nobody waits for.
func (r *Runner) Check(ctx context.Context, projectID string) (*Report, error) {
	project, err := r.db.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	team, err := r.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return nil, err
	}

	var enabled []store.ResolvedRole
	for _, role := range team {
		if role.Enabled {
			enabled = append(enabled, role)
		}
	}

	report := &Report{ProjectID: projectID, CheckedAt: r.now(), Roles: make([]RoleReport, len(enabled))}
	if len(enabled) == 0 {
		// An empty pipeline cannot work, and saying so beats a green panel over
		// a board that will never move.
		report.Ready = false
		return report, nil
	}

	var wg sync.WaitGroup
	for i, role := range enabled {
		wg.Add(1)
		go func(i int, role store.ResolvedRole) {
			defer wg.Done()
			report.Roles[i] = r.checkRole(ctx, project, role)
		}(i, role)
	}
	wg.Wait()

	report.Ready = true
	for _, rr := range report.Roles {
		if rr.Status == StatusBlocked {
			report.Ready = false
			break
		}
	}
	return report, nil
}

func (r *Runner) checkRole(ctx context.Context, project *store.Project, role store.ResolvedRole) RoleReport {
	out := RoleReport{Role: role.Name, Harness: role.Harness, Model: role.Model, Status: StatusOK}

	a, err := r.registry.Get(role.Harness)
	if err != nil {
		// An unknown harness is not something a probe can diagnose further:
		// nothing else about this role can be checked, so report it alone.
		out.Status = StatusBlocked
		out.Checks = []CheckResult{{
			Name:   "harness_known",
			Status: StatusBlocked,
			Reason: err.Error(),
			Remedy: "choose a harness this build supports in the role editor",
		}}
		return out
	}

	spec := adapter.Spec{
		Role:      role.Name,
		Worktree:  worktreePath(project.Path, role.Name),
		Model:     role.Model,
		ExtraArgs: role.Args,
	}

	for _, check := range a.Checks() {
		out.Checks = append(out.Checks, r.run(ctx, check, spec))
	}
	for _, c := range out.Checks {
		switch c.Status {
		case StatusBlocked:
			out.Status = StatusBlocked
		case StatusWarn:
			if out.Status == StatusOK {
				out.Status = StatusWarn
			}
		}
	}
	return out
}

// run bounds one check and turns a panic or a timeout into a finding rather
// than letting either take the panel down.
func (r *Runner) run(ctx context.Context, check adapter.Check, spec adapter.Spec) (res CheckResult) {
	res = CheckResult{Name: check.Name}

	checkCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	done := make(chan adapter.Result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- adapter.Result{
					Reason: fmt.Sprintf("the %s check panicked: %v", check.Name, p),
					Remedy: "this is a zerg bug; report it",
				}
			}
		}()
		done <- check.Run(checkCtx, spec)
	}()

	select {
	case out := <-done:
		res.Detail, res.Reason, res.Remedy = out.Detail, out.Reason, out.Remedy
		switch {
		case out.OK:
			res.Status = StatusOK
		case out.Warn:
			res.Status = StatusWarn
		default:
			res.Status = StatusBlocked
		}
	case <-checkCtx.Done():
		// A hung probe is a finding. The predecessor's failure mode was
		// precisely a thing that never answered and never said so.
		res.Status = StatusBlocked
		res.Reason = fmt.Sprintf("the %s check did not finish within %s", check.Name, r.timeout)
		res.Remedy = "the harness may be hung; try running it once by hand"
	}
	return res
}

// worktreePath is where a role works: one linked worktree per role, named for
// the role. The role name is constrained at the point it enters the system, so
// nothing here has to sanitise it again.
func worktreePath(projectPath, role string) string {
	return filepath.Join(projectPath, ".worktrees", role)
}

// CheckRole runs one role's checks and reports the first blocking finding.
//
// This is the spawn guard, the same suite the readiness panel runs. State
// drifts between setup and launch and between one task and the next: a token
// expires, a binary is upgraded, another tool rewrites a shared config. A role
// that cannot work must not be spawned into looking like it can.
func (r *Runner) CheckRole(ctx context.Context, spec adapter.Spec, a adapter.Adapter) error {
	for _, check := range a.Checks() {
		res := r.run(ctx, check, spec)
		if res.Status == StatusBlocked {
			if res.Remedy != "" {
				return fmt.Errorf("%s: %s (%s)", res.Name, res.Reason, res.Remedy)
			}
			return fmt.Errorf("%s: %s", res.Name, res.Reason)
		}
	}
	return nil
}

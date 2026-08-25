package preflight

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/store"
)

// fake is an adapter whose checks a test dictates, so readiness logic can be
// exercised without depending on which harnesses this machine happens to have.
type fake struct {
	name   string
	checks []adapter.Check
}

func (f *fake) Name() string                                    { return f.name }
func (f *fake) Checks() []adapter.Check                         { return f.checks }
func (f *fake) Capabilities() adapter.Caps                      { return adapter.Caps{} }
func (f *fake) ListModels(adapter.Ctx) ([]adapter.Model, error) { return nil, nil }
func (f *fake) Parse([]byte) ([]adapter.Event, error)           { return nil, nil }
func (f *fake) EncodeTurn(s string) ([]byte, error)             { return []byte(s + "\n"), nil }
func (f *fake) Command(context.Context, adapter.Spec) (*exec.Cmd, error) {
	return exec.Command("true"), nil
}

func passing(name string) adapter.Check {
	return adapter.Check{Name: name, Run: func(adapter.Ctx, adapter.Spec) adapter.Result {
		return adapter.Result{OK: true, Detail: "fine"}
	}}
}

func blocking(name, reason, remedy string) adapter.Check {
	return adapter.Check{Name: name, Run: func(adapter.Ctx, adapter.Spec) adapter.Result {
		return adapter.Result{Reason: reason, Remedy: remedy}
	}}
}

func warning(name string) adapter.Check {
	return adapter.Check{Name: name, Run: func(adapter.Ctx, adapter.Spec) adapter.Result {
		return adapter.Result{Warn: true, Reason: "not in the catalog", Remedy: "check the spelling"}
	}}
}

func setup(t *testing.T, checks ...adapter.Check) (*Runner, *store.Project, *store.DB) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "fakeharness"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	p, err := db.CreateProject(ctx, mustDir(t, "repo"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}

	reg := adapter.NewRegistry()
	reg.Register(&fake{name: "fakeharness", checks: checks})
	return NewRunner(db, reg, WithTimeout(2*time.Second)), p, db
}

func TestReadyWhenEveryCheckPasses(t *testing.T) {
	r, p, _ := setup(t, passing("binary_present"), passing("binary_version"))

	report, err := r.Check(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.Ready {
		t.Fatalf("report is not ready: %+v", report.Roles)
	}
	if len(report.Roles) != 2 {
		t.Fatalf("checked %d roles, want the 2 in the default team", len(report.Roles))
	}
	for _, rr := range report.Roles {
		if rr.Status != StatusOK {
			t.Errorf("role %s is %s, want ok", rr.Role, rr.Status)
		}
		if len(rr.Checks) != 2 {
			t.Errorf("role %s ran %d checks, want 2", rr.Role, len(rr.Checks))
		}
	}
}

// This is the whole point of the gate: a team that cannot work must not reach
// a running board, and it must say what to do about it.
func TestOneBlockedRoleBlocksTheTeam(t *testing.T) {
	r, p, _ := setup(t,
		passing("binary_present"),
		blocking("binary_version", "claude 0.13 cannot call this model", "upgrade the claude CLI"),
	)

	report, err := r.Check(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Ready {
		t.Fatal("a blocked role still produced a ready team")
	}

	rr := report.Roles[0]
	if rr.Status != StatusBlocked {
		t.Fatalf("role status = %s, want blocked", rr.Status)
	}
	var found bool
	for _, c := range rr.Checks {
		if c.Name != "binary_version" {
			continue
		}
		found = true
		if c.Reason == "" {
			t.Error("a blocked check must say what is wrong")
		}
		if c.Remedy == "" {
			t.Error("a blocked check must say how to fix it; a bare failure is the thing this replaces")
		}
	}
	if !found {
		t.Error("the failing check is missing from the report")
	}
}

// A warning is not a block: a harness catalog can lag a model that works, and
// refusing to start over that would be wrong.
func TestWarningsDoNotBlock(t *testing.T) {
	r, p, _ := setup(t, passing("binary_present"), warning("model_available"))

	report, err := r.Check(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.Ready {
		t.Fatal("a warning blocked the team; only blocked findings may do that")
	}
	if report.Roles[0].Status != StatusWarn {
		t.Errorf("role status = %s, want warn", report.Roles[0].Status)
	}
}

func TestUnknownHarnessIsBlockedWithARemedy(t *testing.T) {
	ctx := context.Background()
	r, p, db := setup(t, passing("binary_present"))

	// Point a role at a harness this build does not have.
	tpl, err := db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatalf("GetTemplateByName: %v", err)
	}
	tpl.Harness = "nosuchharness"
	if err := db.UpdateTemplate(ctx, tpl); err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}

	report, err := r.Check(ctx, p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Ready {
		t.Fatal("a role with an unknown harness produced a ready team")
	}
	rr := report.Roles[0]
	if rr.Status != StatusBlocked || len(rr.Checks) != 1 {
		t.Fatalf("want one blocking finding, got %+v", rr)
	}
	if rr.Checks[0].Remedy == "" {
		t.Error("an unknown harness must come with a remedy")
	}
}

// A probe that never answers is itself the finding — that is the exact failure
// mode this package exists to catch, so it must not become its own.
func TestHungCheckIsReportedNotWaitedOn(t *testing.T) {
	hang := adapter.Check{Name: "hangs", Run: func(ctx adapter.Ctx, _ adapter.Spec) adapter.Result {
		<-ctx.Done()
		return adapter.Result{OK: true} // arrives too late to count
	}}

	r, p, _ := setup(t, hang)
	r.timeout = 50 * time.Millisecond

	start := time.Now()
	report, err := r.Check(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the panel waited %s on a hung probe", elapsed)
	}
	if report.Ready {
		t.Fatal("a hung check produced a ready team")
	}
	if report.Roles[0].Checks[0].Status != StatusBlocked {
		t.Error("a timed-out check must block")
	}
}

// A panicking check is a zerg bug, and a bug in one probe must not take the
// whole readiness panel down with it.
func TestPanickingCheckBecomesAFinding(t *testing.T) {
	boom := adapter.Check{Name: "boom", Run: func(adapter.Ctx, adapter.Spec) adapter.Result {
		panic("kaboom")
	}}
	r, p, _ := setup(t, boom)

	report, err := r.Check(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Ready {
		t.Fatal("a panicking check produced a ready team")
	}
	if got := report.Roles[0].Checks[0].Reason; got == "" {
		t.Error("the panic was swallowed instead of reported")
	}
}

// An empty pipeline cannot work, and a green panel over a board that will never
// move is exactly the dishonesty this gate exists to prevent.
func TestEmptyTeamIsNotReady(t *testing.T) {
	ctx := context.Background()
	r, p, db := setup(t, passing("binary_present"))

	if err := db.SetTeam(ctx, p.ID, nil); err != nil {
		t.Fatalf("SetTeam: %v", err)
	}
	report, err := r.Check(ctx, p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Ready {
		t.Fatal("a team with no roles reported ready")
	}
}

// Disabled roles are not part of the pipeline, so their readiness is not the
// team's problem.
func TestDisabledRolesAreNotChecked(t *testing.T) {
	ctx := context.Background()
	r, p, db := setup(t, passing("binary_present"))

	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	roles := []store.ProjectRole{
		{TemplateID: team[0].ID, Enabled: true},
		{TemplateID: team[1].ID, Enabled: false},
	}
	if err := db.SetTeam(ctx, p.ID, roles); err != nil {
		t.Fatalf("SetTeam: %v", err)
	}

	report, err := r.Check(ctx, p.ID)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Roles) != 1 {
		t.Fatalf("checked %d roles, want only the enabled one", len(report.Roles))
	}
}

// mustDir makes a directory for a project to point at. Paths are validated on
// creation now, so a test cannot name one that does not exist.
func mustDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	return dir
}

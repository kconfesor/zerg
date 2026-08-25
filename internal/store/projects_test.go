package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// seeded returns a db with the built-in library and one project.
func seeded(t *testing.T) (*DB, *Project) {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	p, err := db.CreateProject(ctx, filepath.Join(t.TempDir(), "repo"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return db, p
}

func templateID(t *testing.T, db *DB, name string) string {
	t.Helper()
	tpl, err := db.GetTemplateByName(context.Background(), name)
	if err != nil {
		t.Fatalf("GetTemplateByName(%q): %v", name, err)
	}
	return tpl.ID
}

func TestCreateProjectDefaults(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	dir := filepath.Join(t.TempDir(), "calc-rs")
	p, err := db.CreateProject(ctx, dir, "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.Name != "calc-rs" {
		t.Errorf("name = %q, want the directory name", p.Name)
	}
	if p.BaseBranch != "main" {
		t.Errorf("baseBranch = %q, want main", p.BaseBranch)
	}
	if !filepath.IsAbs(p.Path) {
		t.Errorf("path %q was not absolutised; the same repo must not become two projects", p.Path)
	}
}

func TestDuplicateProjectPathRejected(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	dir := t.TempDir()

	if _, err := db.CreateProject(ctx, dir, "", ""); err != nil {
		t.Fatalf("first CreateProject: %v", err)
	}
	if _, err := db.CreateProject(ctx, dir, "", ""); err == nil {
		t.Fatal("the same path was registered twice")
	}
}

func TestDefaultTeamIsCoderThenReviewer(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}
	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}

	if len(team) != 2 {
		t.Fatalf("team has %d roles, want 2", len(team))
	}
	if team[0].Name != "coder" || team[1].Name != "reviewer" {
		t.Fatalf("team is %s → %s, want coder → reviewer", team[0].Name, team[1].Name)
	}
	if team[0].Terminal {
		t.Error("coder must not be terminal")
	}
	if !team[1].Terminal {
		t.Error("reviewer is last and enabled, so it must be terminal")
	}
}

func TestSetTeamNormalisesPositions(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	// Whatever a drag produced: no positions supplied at all.
	err := db.SetTeam(ctx, p.ID, []ProjectRole{
		{TemplateID: templateID(t, db, "planner"), Enabled: true},
		{TemplateID: templateID(t, db, "coder"), Enabled: true},
		{TemplateID: templateID(t, db, "reviewer"), Enabled: true},
	})
	if err != nil {
		t.Fatalf("SetTeam: %v", err)
	}

	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	for i, r := range team {
		if r.Position != i {
			t.Errorf("%s has position %d, want %d", r.Name, r.Position, i)
		}
	}
	if team[0].Name != "planner" || team[2].Name != "reviewer" {
		t.Errorf("order not preserved: %s, %s, %s", team[0].Name, team[1].Name, team[2].Name)
	}
	if team[0].Gate != GateApproval {
		t.Error("planner should carry its approval gate into the project")
	}
}

// Terminality follows the last *enabled* role, so disabling the final role
// promotes the one before it with no edit anywhere else. The predecessor took
// this from config-file line order, where reordering a file silently moved the
// end of the pipeline.
func TestTerminalFollowsLastEnabledRole(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	err := db.SetTeam(ctx, p.ID, []ProjectRole{
		{TemplateID: templateID(t, db, "coder"), Enabled: true},
		{TemplateID: templateID(t, db, "reviewer"), Enabled: true},
		{TemplateID: templateID(t, db, "docs"), Enabled: false},
	})
	if err != nil {
		t.Fatalf("SetTeam: %v", err)
	}

	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	if len(team) != 3 {
		t.Fatalf("team has %d roles, want 3", len(team))
	}
	if team[2].Terminal {
		t.Error("a disabled role must never be terminal")
	}
	if !team[1].Terminal {
		t.Error("reviewer is the last enabled role, so it is terminal")
	}

	terminals := 0
	for _, r := range team {
		if r.Terminal {
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("%d roles are terminal, want exactly 1", terminals)
	}
}

func TestOverridesApplyAndAreFlagged(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	coder := templateID(t, db, "coder")
	override := "opus"
	err := db.SetTeam(ctx, p.ID, []ProjectRole{
		{TemplateID: coder, Enabled: true, ModelOverride: &override},
		{TemplateID: templateID(t, db, "reviewer"), Enabled: true},
	})
	if err != nil {
		t.Fatalf("SetTeam: %v", err)
	}

	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	if team[0].Model != "opus" {
		t.Errorf("override not applied: model = %q", team[0].Model)
	}
	if !team[0].Overridden {
		t.Error("an overridden role must be flagged so the UI can badge it")
	}
	if team[1].Overridden {
		t.Error("reviewer has no override and must not be flagged")
	}

	// The library is untouched: an override belongs to the pairing.
	tpl, err := db.GetTemplate(ctx, coder)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if tpl.Model != "sonnet" {
		t.Errorf("the override leaked into the library: template model = %q", tpl.Model)
	}
}

// An override equal to the template value is not a divergence, so it must not
// light up the badge — otherwise every round-trip through a UI that always
// sends the field would mark the project as drifted.
func TestOverrideEqualToTemplateIsNotFlagged(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	same := "sonnet"
	err := db.SetTeam(ctx, p.ID, []ProjectRole{
		{TemplateID: templateID(t, db, "coder"), Enabled: true, ModelOverride: &same},
	})
	if err != nil {
		t.Fatalf("SetTeam: %v", err)
	}
	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	if team[0].Overridden {
		t.Error("an override identical to the template is not a divergence")
	}
}

func TestSetTeamRejectsDuplicateAndUnknownRoles(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	coder := templateID(t, db, "coder")

	err := db.SetTeam(ctx, p.ID, []ProjectRole{
		{TemplateID: coder, Enabled: true},
		{TemplateID: coder, Enabled: true},
	})
	if err == nil {
		t.Error("a role was allowed to join the same pipeline twice")
	}

	err = db.SetTeam(ctx, p.ID, []ProjectRole{{TemplateID: "NOSUCHID", Enabled: true}})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound for an unknown template, got %v", err)
	}
}

// SetTeam replaces the whole pipeline in one transaction, so a rejected update
// must leave the previous team exactly as it was.
func TestFailedSetTeamLeavesPreviousTeamIntact(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}
	if err := db.SetTeam(ctx, p.ID, []ProjectRole{{TemplateID: "NOSUCHID"}}); err == nil {
		t.Fatal("expected the bad update to fail")
	}

	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	if len(team) != 2 {
		t.Fatalf("a failed update damaged the team: %d roles remain", len(team))
	}
}

func TestDeletingATemplateRemovesItFromTeams(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}
	if err := db.DeleteTemplate(ctx, templateID(t, db, "reviewer")); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	if len(team) != 1 || team[0].Name != "coder" {
		t.Fatalf("cascade left the team wrong: %+v", team)
	}
	// Terminality must move to whatever is now last.
	if !team[0].Terminal {
		t.Error("coder is now the last enabled role and must be terminal")
	}
}

func TestDeletingAProjectLeavesTheLibrary(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}
	if err := db.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	tpls, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(tpls) != len(builtinRoles) {
		t.Errorf("deleting a project took %d library roles with it", len(builtinRoles)-len(tpls))
	}
	if _, err := db.ResolveTeam(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound for a deleted project, got %v", err)
	}
}

func TestListProjectsOrdersByRecency(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	older, err := db.CreateProject(ctx, filepath.Join(t.TempDir(), "older"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := db.CreateProject(ctx, filepath.Join(t.TempDir(), "newer"), "", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := db.TouchProject(ctx, older.ID); err != nil {
		t.Fatalf("TouchProject: %v", err)
	}

	list, err := db.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(list) != 2 || list[0].Name != "older" {
		t.Errorf("opening a project did not float it to the top: %+v", list)
	}
}

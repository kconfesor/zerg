package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func strp(v string) *string { return &v }
func intp(v int) *int       { return &v }

func TestPresetAndProjectOverridesAreLayeredAndLive(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	coder := templateID(t, db, "coder")
	preset, err := db.CreateTeamPreset(ctx, &TeamPreset{Name: "Batch team", Roles: []TeamPresetRole{{
		TemplateID: coder, Enabled: true, RoleOverrides: RoleOverrides{
			HarnessOverride: strp("pi"), ModelOverride: strp("preset-model"), ArgsOverride: []string{},
			ReceiveOverride: strp(ReceiveBatch), BatchMaxItemsOverride: intp(3), BatchMaxAgeSecOverride: intp(4),
			PromptOverride: strp("preset prompt"), GateOverride: strp(GateApproval),
		},
	}}})
	if err != nil {
		t.Fatalf("CreateTeamPreset: %v", err)
	}
	p, err := db.CreateProject(ctx, repoDir(t, "layered"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	localPrompt := "project prompt"
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, false, []ProjectRole{{
		TemplateID: coder, RoleOverrides: RoleOverrides{PromptOverride: &localPrompt},
	}}); err != nil {
		t.Fatalf("SetProjectTeam: %v", err)
	}
	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := team[0]
	if r.Harness != "pi" || r.Model != "preset-model" || r.Receive != ReceiveBatch || r.BatchMaxItems != 3 || r.BatchMaxAgeSec != 4 || r.Gate != GateApproval || r.Prompt != localPrompt {
		t.Fatalf("layered role is wrong: %+v", r)
	}
	if r.ArgsOverride != nil {
		t.Fatalf("project invented an args override: %#v", r.ArgsOverride)
	}

	preset.Roles[0].ModelOverride = strp("changed-model")
	preset.Roles[0].PromptOverride = strp("changed preset prompt")
	if err := db.UpdateTeamPreset(ctx, preset); err != nil {
		t.Fatal(err)
	}
	team, _ = db.ResolveTeam(ctx, p.ID)
	if team[0].Model != "changed-model" || team[0].Prompt != localPrompt {
		t.Fatalf("live preset/local override behavior wrong: %+v", team[0])
	}
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	team, _ = db.ResolveTeam(ctx, p.ID)
	if team[0].Prompt != "changed preset prompt" || team[0].Overridden {
		t.Fatalf("reset did not restore preset defaults: %+v", team[0])
	}
}

func TestExplicitEmptyArgsSurvivesAndNilResets(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	coder := templateID(t, db, "coder")
	preset, err := db.CreateTeamPreset(ctx, &TeamPreset{Name: "Args", Roles: []TeamPresetRole{{
		TemplateID: coder, Enabled: true, RoleOverrides: RoleOverrides{ArgsOverride: []string{"--preset"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.CreateProject(ctx, repoDir(t, "args"), "", "")
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, false, []ProjectRole{{
		TemplateID: coder, RoleOverrides: RoleOverrides{ArgsOverride: []string{}},
	}}); err != nil {
		t.Fatal(err)
	}
	team, _ := db.ResolveTeam(ctx, p.ID)
	if team[0].ArgsOverride == nil || len(team[0].Args) != 0 {
		t.Fatalf("explicit empty args were lost: %+v", team[0])
	}
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	team, _ = db.ResolveTeam(ctx, p.ID)
	if len(team[0].Args) != 1 || team[0].Args[0] != "--preset" || team[0].ArgsOverride != nil {
		t.Fatalf("nil did not reset args: %+v", team[0])
	}
}

func TestExplicitEmptyPromptSurvivesAndNilResets(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	coder := templateID(t, db, "coder")
	presetPrompt := "preset prompt"
	preset, err := db.CreateTeamPreset(ctx, &TeamPreset{Name: "Prompt", Roles: []TeamPresetRole{{
		TemplateID: coder, Enabled: true, RoleOverrides: RoleOverrides{PromptOverride: &presetPrompt},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := db.CreateProject(ctx, repoDir(t, "prompt"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, false, []ProjectRole{{
		TemplateID: coder, RoleOverrides: RoleOverrides{PromptOverride: &empty},
	}}); err != nil {
		t.Fatal(err)
	}
	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if team[0].Prompt != "" || team[0].PromptOverride == nil {
		t.Fatalf("explicit empty prompt was lost: %+v", team[0])
	}
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	team, err = db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if team[0].Prompt != presetPrompt || team[0].PromptOverride != nil {
		t.Fatalf("nil did not restore the preset prompt: %+v", team[0])
	}
}

func TestPresetTopologyIsLiveUntilProjectOverridesIt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	coder, reviewer := templateID(t, db, "coder"), templateID(t, db, "reviewer")
	preset, err := db.CreateTeamPreset(ctx, &TeamPreset{Name: "Growing", Roles: []TeamPresetRole{{TemplateID: coder, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.CreateProject(ctx, repoDir(t, "topology"), "", "")
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	preset.Roles = append(preset.Roles, TeamPresetRole{TemplateID: reviewer, Enabled: true})
	if err := db.UpdateTeamPreset(ctx, preset); err != nil {
		t.Fatal(err)
	}
	team, _ := db.ResolveTeam(ctx, p.ID)
	if len(team) != 2 {
		t.Fatalf("inherited topology did not update: %+v", team)
	}
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, true, []ProjectRole{{TemplateID: coder, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	preset.Roles = nil
	if err := db.UpdateTeamPreset(ctx, preset); err != nil {
		t.Fatal(err)
	}
	team, _ = db.ResolveTeam(ctx, p.ID)
	if len(team) != 1 || team[0].Name != "coder" {
		t.Fatalf("local topology changed with preset: %+v", team)
	}
}

func TestMigrationFromV12PreservesEffectiveProjectTeam(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v12.db")
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version=12`); err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	if _, err := raw.ExecContext(ctx, `INSERT INTO role_templates (id,name,harness,model,args,receive,batch_max_items,batch_max_age_sec,prompt,gate,builtin,created_at,updated_at) VALUES ('c','coder','claude','sonnet','[]','task',8,300,'old','none',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	dir := repoDir(t, "legacy")
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects (id,path,name,base_branch,created_at) VALUES ('p',?,'legacy','main',?)`, dir, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO project_roles (project_id,template_id,position,enabled,model_override,args_override) VALUES ('p','c',0,1,'opus','[]')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	team, err := db.ResolveTeam(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(team) != 1 || team[0].Model != "opus" || team[0].ArgsOverride == nil {
		t.Fatalf("v12 team changed: %+v", team)
	}
	p, _ := db.GetProject(ctx, "p")
	if !p.TeamTopologyOverride || p.TeamPresetID != nil {
		t.Fatalf("legacy project source changed: %+v", p)
	}
}

// A team with no roles is an ordinary state, and must not serialise as null.
//
// Unchecking the last role produces one. A nil slice marshals as
// `"roles": null`, which the cockpit dereferenced in a dozen places — so one
// empty team anywhere in the list threw and took the Team page down for every
// project, with no way back except deleting the row by hand.
func TestEmptyPresetRolesAreAnArrayNotNull(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}

	p, err := db.CreateTeamPreset(ctx, &TeamPreset{Name: "Empty"})
	if err != nil {
		t.Fatalf("CreateTeamPreset: %v", err)
	}
	got, err := db.GetTeamPreset(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Roles == nil {
		t.Fatal("an empty team came back with nil roles; it marshals as null")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"roles":[]`) {
		t.Errorf("empty team serialised as %s", raw)
	}
}

// Seeding must not reinstate a team the operator deliberately emptied.
//
// cmd/zerg/main.go states the invariant: seeding "never clobbers an edited
// role ... configuration lives in the database precisely so that a restart does
// not overwrite what the user changed." Keying the check on emptiness rather
// than on having-been-seeded broke it for the built-in team.
func TestSeedDoesNotRefillAnEmptiedDefaultTeam(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}

	def, err := db.GetTeamPreset(ctx, DefaultTeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Roles) == 0 {
		t.Fatal("the built-in team seeded empty")
	}

	// The operator clears it.
	def.Roles = nil
	if err := db.UpdateTeamPreset(ctx, def); err != nil {
		t.Fatalf("UpdateTeamPreset: %v", err)
	}

	// A restart.
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetTeamPreset(ctx, DefaultTeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Roles) != 0 {
		t.Errorf("restarting put %d roles back into a team that was emptied on purpose", len(after.Roles))
	}
}

// Resolving a team is a read, and a read must not fail on data already stored.
//
// A template edit that passes its own validation can still invalidate a role
// once a preset's overrides are layered on it — neither write path sees both
// sides. Returning that as an error from ResolveTeam took out the board,
// preflight, the spawn path, routing and chat at once, leaving the project
// impossible to open or to repair.
func TestResolveTeamSurvivesAnInvalidMergedRole(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	p, err := db.CreateProjectWithDefaultTeam(ctx, repoDir(t, "resolve"), "resolve", "main")
	if err != nil {
		t.Fatal(err)
	}

	coder, err := db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	// A preset that batches, on a template that does not — so the batch bounds
	// are never checked when the template itself is validated.
	batch := ReceiveBatch
	def, err := db.GetTeamPreset(ctx, DefaultTeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range def.Roles {
		if def.Roles[i].TemplateID == coder.ID {
			def.Roles[i].ReceiveOverride = &batch
		}
	}
	if err := db.UpdateTeamPreset(ctx, def); err != nil {
		t.Fatal(err)
	}

	// The template edit that makes the merge invalid, and which its own
	// validation accepts because the template receives one task at a time.
	coder.Receive = ReceiveTask
	coder.BatchMaxItems = 0
	if err := db.UpdateTemplate(ctx, coder); err != nil {
		t.Fatalf("the template edit was refused, so the case cannot arise: %v", err)
	}

	team, err := db.ResolveTeam(ctx, p.ID)
	if err != nil {
		t.Fatalf("ResolveTeam failed on stored data, taking the project with it: %v", err)
	}
	if len(team) == 0 {
		t.Error("the team resolved to nothing")
	}
}

// A team can belong to one project.
//
// Teams were global, so a pipeline built around one repository's prompts and
// models appeared in every other repository's picker, and editing it there
// changed the first one. These are the rules that separate them.
func TestATeamCanBelongToOneProject(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	x, err := db.CreateProject(ctx, repoDir(t, "own-x"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	y, err := db.CreateProject(ctx, repoDir(t, "own-y"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	coder, err := db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	roles := []TeamPresetRole{{TemplateID: coder.ID, Enabled: true}}

	mine, err := db.CreateTeamPreset(ctx, &TeamPreset{Name: "X team", ProjectID: &x.ID, Roles: roles})
	if err != nil {
		t.Fatalf("creating a project's team: %v", err)
	}
	if mine.ProjectID == nil || *mine.ProjectID != x.ID {
		t.Fatalf("team came back owned by %v, want %s", mine.ProjectID, x.ID)
	}

	// X sees the shared teams and its own; Y sees the shared ones only.
	forX, err := db.ListTeamPresetsFor(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	forY, err := db.ListTeamPresetsFor(ctx, y.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(forX, func(p TeamPreset) bool { return p.ID == mine.ID }) {
		t.Error("a project cannot see its own team")
	}
	if slices.ContainsFunc(forY, func(p TeamPreset) bool { return p.ID == mine.ID }) {
		t.Error("another project's team is in this project's picker")
	}
	if !slices.ContainsFunc(forY, func(p TeamPreset) bool { return p.ID == DefaultTeamPresetID }) {
		t.Error("the shared Default team is missing from a project's picker")
	}

	// The picker is not the only way in: the daemon refuses the id outright.
	if err := db.SetProjectTeam(ctx, y.ID, &mine.ID, false, nil); !isInvalid(err) {
		t.Errorf("putting Y on X's team: %v, want a 400-class refusal", err)
	}
	if err := db.SetProjectTeam(ctx, x.ID, &mine.ID, false, nil); err != nil {
		t.Errorf("putting X on its own team: %v", err)
	}

	// Deleting the project takes its team with it, and leaves the shared ones.
	if err := db.DeleteProject(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetTeamPreset(ctx, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted project's team outlived it: %v", err)
	}
	if _, err := db.GetTeamPreset(ctx, DefaultTeamPresetID); err != nil {
		t.Errorf("deleting a project took a shared team with it: %v", err)
	}
}

// Moving a team between owners must not take a pipeline away from a project
// that is running it.
func TestClaimingATeamOtherProjectsRunIsRefused(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	x, err := db.CreateProject(ctx, repoDir(t, "claim-x"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	y, err := db.CreateProject(ctx, repoDir(t, "claim-y"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	coder, err := db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := db.CreateTeamPreset(ctx, &TeamPreset{
		Name:  "Shared review",
		Roles: []TeamPresetRole{{TemplateID: coder.ID, Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectTeam(ctx, y.ID, &shared.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	claim := *shared
	claim.ProjectID = &x.ID
	if err := db.UpdateTeamPreset(ctx, &claim); !isInvalid(err) {
		t.Errorf("claiming a team Y runs: %v, want a refusal naming Y", err)
	}

	// With nobody else on it, the same claim goes through, and sharing it
	// back is always allowed since that strands nobody.
	if err := db.SelectDefaultTeam(ctx, y.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTeamPreset(ctx, &claim); err != nil {
		t.Fatalf("claiming an unused team: %v", err)
	}
	back := claim
	back.ProjectID = nil
	if err := db.UpdateTeamPreset(ctx, &back); err != nil {
		t.Errorf("sharing a project's team back: %v", err)
	}

	// Default is where a new project starts, so it cannot become one project's.
	def, err := db.GetTeamPreset(ctx, DefaultTeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	def.ProjectID = &x.ID
	if err := db.UpdateTeamPreset(ctx, def); !isInvalid(err) {
		t.Errorf("claiming the built-in team: %v, want a refusal", err)
	}
}

// isInvalid reports whether an error is the kind the API renders as a 400: a
// problem with what was asked for, not a fault.
func isInvalid(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

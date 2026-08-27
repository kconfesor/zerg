package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, []ProjectRole{{
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
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, nil); err != nil {
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
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, []ProjectRole{{
		TemplateID: coder, RoleOverrides: RoleOverrides{ArgsOverride: []string{}},
	}}); err != nil {
		t.Fatal(err)
	}
	team, _ := db.ResolveTeam(ctx, p.ID)
	if team[0].ArgsOverride == nil || len(team[0].Args) != 0 {
		t.Fatalf("explicit empty args were lost: %+v", team[0])
	}
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, nil); err != nil {
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
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, []ProjectRole{{
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
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, nil); err != nil {
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

// A project's pipeline is its team's, and stays that way.
//
// It used to be its team's until the project froze a copy of the shape, which
// is what schema 16 removed: a project running something else now runs a team
// of its own, and the way to tell is that it is a different team.
func TestAProjectsPipelineFollowsItsTeam(t *testing.T) {
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
	if err := db.SetProjectTeam(ctx, p.ID, &preset.ID, nil); err != nil {
		t.Fatal(err)
	}
	preset.Roles = append(preset.Roles, TeamPresetRole{TemplateID: reviewer, Enabled: true})
	if err := db.UpdateTeamPreset(ctx, preset); err != nil {
		t.Fatal(err)
	}
	team, _ := db.ResolveTeam(ctx, p.ID)
	if len(team) != 2 {
		t.Fatalf("a role added to the team did not reach the project on it: %+v", team)
	}

	// Wanting a different shape gives the project a team of its own, named
	// after it and belonging to it.
	if err := db.SetTeam(ctx, p.ID, []TeamPresetRole{{TemplateID: coder, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetProjectTeam(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PresetID == nil || *after.PresetID == preset.ID {
		t.Fatalf("the project is still on Growing: %+v", after.PresetID)
	}
	own, err := db.GetTeamPreset(ctx, *after.PresetID)
	if err != nil {
		t.Fatal(err)
	}
	if own.ProjectID == nil || *own.ProjectID != p.ID {
		t.Errorf("the project's own pipeline landed in a team owned by %v", own.ProjectID)
	}

	// And the team it left carries on without it.
	preset.Roles = nil
	if err := db.UpdateTeamPreset(ctx, preset); err != nil {
		t.Fatal(err)
	}
	team, _ = db.ResolveTeam(ctx, p.ID)
	if len(team) != 1 || team[0].Name != "coder" {
		t.Fatalf("emptying the old team reached a project that had left it: %+v", team)
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
	// The pipeline it had is now a team of its own rather than a layer over
	// nothing: schema 16 materialised it, keeping the project on what it ran.
	p, _ := db.GetProject(ctx, "p")
	if p.TeamPresetID == nil {
		t.Fatalf("a legacy project ended up on no team at all: %+v", p)
	}
	own, err := db.GetTeamPreset(ctx, *p.TeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	if own.ProjectID == nil || *own.ProjectID != "p" {
		t.Errorf("its pipeline landed in a team owned by %v, want the project's own", own.ProjectID)
	}
	if own.Name != "legacy team" {
		t.Errorf("the materialised team is called %q, want it named after the project", own.Name)
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
	if err := db.SetProjectTeam(ctx, y.ID, &mine.ID, nil); !isInvalid(err) {
		t.Errorf("putting Y on X's team: %v, want a 400-class refusal", err)
	}
	if err := db.SetProjectTeam(ctx, x.ID, &mine.ID, nil); err != nil {
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
	if err := db.SetProjectTeam(ctx, y.ID, &shared.ID, nil); err != nil {
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

// Migration 16 turns a project's own shape into a team the project owns, and
// the pipeline that resolves out the other side is the one that went in.
//
// The shape came from project_roles while the per-role settings came from the
// team the project was naming, so the copy has to carry both or a role quietly
// changes model or prompt on upgrade.
func TestMigrationFromV15MaterialisesAnOwnPipelineAsATeam(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v15.db")
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version=15`); err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	for _, name := range []string{"coder", "reviewer", "docs"} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO role_templates
			(id,name,harness,model,args,receive,batch_max_items,batch_max_age_sec,prompt,gate,builtin,created_at,updated_at)
			VALUES (?,?,'claude','sonnet','[]','task',8,300,'p','none',1,?,?)`, name, name, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO team_presets (id,name,builtin,created_at,updated_at) VALUES ('shared','Calc pipeline',0,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	// The team's own per-role setting: the reviewer it runs is on opus.
	if _, err := raw.ExecContext(ctx, `INSERT INTO team_preset_roles (preset_id,template_id,position,enabled,model_override) VALUES
		('shared','coder',0,1,NULL), ('shared','reviewer',1,1,'opus')`); err != nil {
		t.Fatal(err)
	}
	dir := repoDir(t, "frozen")
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects (id,path,name,base_branch,created_at,team_preset_id,team_topology_override)
		VALUES ('p',?,'Credix','main',?,'shared',1)`, dir, now); err != nil {
		t.Fatal(err)
	}
	// The project ran something else: docs first, its own order, reviewer off.
	if _, err := raw.ExecContext(ctx, `INSERT INTO project_roles (project_id,template_id,position,enabled) VALUES
		('p','docs',0,1), ('p','coder',1,1), ('p','reviewer',2,0)`); err != nil {
		t.Fatal(err)
	}
	// And its own setting on top of the team's.
	if _, err := raw.ExecContext(ctx, `INSERT INTO project_role_overrides (project_id,template_id,prompt_override) VALUES ('p','coder','this repo only')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	defer db.Close()

	team, err := db.ResolveTeam(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	got := make([][2]any, 0, len(team))
	for _, r := range team {
		got = append(got, [2]any{r.Name, r.Enabled})
	}
	want := [][2]any{{"docs", true}, {"coder", true}, {"reviewer", false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("pipeline came out %v, want %v", got, want)
	}
	if team[1].Prompt != "this repo only" {
		t.Errorf("the project's own prompt override was lost: %q", team[1].Prompt)
	}
	// Carried from the team the project was naming, which still applied to
	// roles it also had while the order was the project's.
	if team[2].Model != "opus" {
		t.Errorf("reviewer runs %q, want the opus its team gave it", team[2].Model)
	}

	p, err := db.GetProject(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	own, err := db.GetTeamPreset(ctx, *p.TeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	if own.ProjectID == nil || *own.ProjectID != "p" || own.Name != "Credix team" {
		t.Errorf("materialised team is %+v, want one named Credix team owned by the project", own)
	}
	// The shared team it left is untouched, and still shared.
	shared, err := db.GetTeamPreset(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if shared.ProjectID != nil || len(shared.Roles) != 2 {
		t.Errorf("the shared team changed: %+v", shared)
	}

	// The layer itself is gone, not merely unused.
	var n int
	if err := db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('projects') WHERE name='team_topology_override'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("projects still carries team_topology_override")
	}
	if err := db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='project_roles'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("project_roles is still there")
	}
}

// A frozen shape identical to its team's was a layer doing nothing, and the
// project keeps the team it is on rather than collecting a copy of it.
func TestMigrationLeavesANoOpLayerWithoutADuplicateTeam(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "noop.db")
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version=15`); err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	for _, name := range []string{"coder", "reviewer"} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO role_templates
			(id,name,harness,model,args,receive,batch_max_items,batch_max_age_sec,prompt,gate,builtin,created_at,updated_at)
			VALUES (?,?,'claude','sonnet','[]','task',8,300,'p','none',1,?,?)`, name, name, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO team_presets (id,name,builtin,created_at,updated_at) VALUES ('shared','Shared',0,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO team_preset_roles (preset_id,template_id,position,enabled) VALUES
		('shared','coder',0,1), ('shared','reviewer',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects (id,path,name,base_branch,created_at,team_preset_id,team_topology_override)
		VALUES ('p',?,'Same','main',?,'shared',1)`, repoDir(t, "noop"), now); err != nil {
		t.Fatal(err)
	}
	// The same membership, order and flags the team already has.
	if _, err := raw.ExecContext(ctx, `INSERT INTO project_roles (project_id,template_id,position,enabled) VALUES
		('p','coder',0,1), ('p','reviewer',1,1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	defer db.Close()

	p, err := db.GetProject(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if p.TeamPresetID == nil || *p.TeamPresetID != "shared" {
		t.Errorf("project moved to %v, want the team it was already running", p.TeamPresetID)
	}
	teams, err := db.ListTeamPresets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, team := range teams {
		if team.ProjectID != nil {
			t.Errorf("a copy was made for a layer that changed nothing: %s", team.Name)
		}
	}
}

// v15 builds a database one migration short of the topology removal, with a
// shared team of coder then reviewer, for the migration tests below.
func v15DB(t *testing.T, name string) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), name)
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version=15`); err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	for _, r := range []string{"coder", "reviewer"} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO role_templates
			(id,name,harness,model,args,receive,batch_max_items,batch_max_age_sec,prompt,gate,builtin,created_at,updated_at)
			VALUES (?,?,'claude','sonnet','[]','task',8,300,'p','none',1,?,?)`, r, r, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO team_presets (id,name,builtin,created_at,updated_at) VALUES ('shared','Shared',0,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO team_preset_roles (preset_id,template_id,position,enabled) VALUES ('shared','coder',0,1),('shared','reviewer',1,1)`); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

// A pipeline of no roles is a decision, and the migration has to carry it.
//
// It was left out of the materialisation for having nothing to copy, which sent
// the project back to whichever team it nominally named: a database where
// nothing ran came up running a coder and a reviewer.
func TestMigrationKeepsAnEmptyPipelineEmpty(t *testing.T) {
	ctx := context.Background()
	path, raw := v15DB(t, "empty.db")
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects (id,path,name,base_branch,created_at,team_preset_id,team_topology_override)
		VALUES ('p',?,'Empty','main','2026-01-01T00:00:00Z','shared',1)`, repoDir(t, "empty")); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	defer db.Close()

	team, err := db.ResolveTeam(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(team) != 0 {
		names := make([]string, 0, len(team))
		for _, r := range team {
			names = append(names, r.Name)
		}
		t.Errorf("a pipeline that ran nothing came up running %v", names)
	}
	p, err := db.GetProject(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	own, err := db.GetTeamPreset(ctx, *p.TeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	if own.ProjectID == nil || *own.ProjectID != "p" {
		t.Errorf("it landed on team %+v, want an empty one of its own", own)
	}
}

// A name the migration wanted, twice over.
//
// Two candidates were not enough: with both taken, the INSERT hit the UNIQUE on
// team_presets.name, the migration failed, and the daemon could not open that
// database at all. An awkward team name is the better failure.
func TestMigrationSurvivesTakenTeamNames(t *testing.T) {
	ctx := context.Background()
	path, raw := v15DB(t, "collide.db")
	now := "2026-01-01T00:00:00Z"
	const id = "01M0000000000000000123456"
	if _, err := raw.ExecContext(ctx, `INSERT INTO team_presets (id,name,builtin,created_at,updated_at) VALUES
		('a','Calc team',0,?,?), ('b','Calc team 123456',0,?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects (id,path,name,base_branch,created_at,team_preset_id,team_topology_override)
		VALUES (?,?,'Calc','main',?,'shared',1)`, id, repoDir(t, "collide"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO project_roles (project_id,template_id,position,enabled) VALUES (?,'coder',0,1)`, id); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("the daemon cannot open a database whose team names were taken: %v", err)
	}
	defer db.Close()

	p, err := db.GetProject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	own, err := db.GetTeamPreset(ctx, *p.TeamPresetID)
	if err != nil {
		t.Fatal(err)
	}
	if own.Name != "Calc team "+id {
		t.Errorf("materialised team is called %q, want the candidate nothing else can hold", own.Name)
	}
	team, err := db.ResolveTeam(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(team) != 1 || team[0].Name != "coder" {
		t.Errorf("pipeline came out %+v, want the one coder it had", team)
	}
}

// Two projects of the same name both needing a team of their own.
func TestMigrationNamesTwoProjectsCalledTheSameThing(t *testing.T) {
	ctx := context.Background()
	path, raw := v15DB(t, "twins.db")
	now := "2026-01-01T00:00:00Z"
	for _, id := range []string{"aaa", "bbb"} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO projects (id,path,name,base_branch,created_at,team_topology_override)
			VALUES (?,?,'Twin','main',?,1)`, id, repoDir(t, "twin-"+id), now); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO project_roles (project_id,template_id,position,enabled) VALUES (?,'coder',0,1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrating two projects with one name: %v", err)
	}
	defer db.Close()

	seen := map[string]bool{}
	for _, id := range []string{"aaa", "bbb"} {
		p, err := db.GetProject(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		own, err := db.GetTeamPreset(ctx, *p.TeamPresetID)
		if err != nil {
			t.Fatal(err)
		}
		if own.ProjectID == nil || *own.ProjectID != id {
			t.Errorf("project %s is on %+v, want a team of its own", id, own)
		}
		if seen[own.Name] {
			t.Errorf("both projects got a team called %q", own.Name)
		}
		seen[own.Name] = true
	}
}

// The ownership rules are conditions on the writes, not only checks before
// them.
//
// Validation reads on a different pool and before the transaction opens, so two
// requests can both pass and then interleave: one assigns a shared team to a
// project while the other hands that team to a different project. Simulated
// here by validating against state that then changes underneath, which is what
// the interleaving amounts to.
func TestOwnershipIsEnforcedByTheWriteItself(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	x, err := db.CreateProject(ctx, repoDir(t, "race-x"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	y, err := db.CreateProject(ctx, repoDir(t, "race-y"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	coder := templateID(t, db, "coder")
	shared, err := db.CreateTeamPreset(ctx, &TeamPreset{
		Name:  "Contested",
		Roles: []TeamPresetRole{{TemplateID: coder, Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// X reads the team as shared and is about to put itself on it. Y claims it
	// first. The write X was about to make must not go through.
	claim := *shared
	claim.ProjectID = &y.ID
	if err := db.UpdateTeamPreset(ctx, &claim); err != nil {
		t.Fatalf("Y claiming an unused team: %v", err)
	}
	if err := db.SetProjectTeam(ctx, x.ID, &shared.ID, nil); !isInvalid(err) {
		t.Errorf("X was put on Y's team: %v", err)
	}

	// And the other way: Y reads nobody else on its team, X gets there first.
	back := claim
	back.ProjectID = nil
	if err := db.UpdateTeamPreset(ctx, &back); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectTeam(ctx, x.ID, &shared.ID, nil); err != nil {
		t.Fatal(err)
	}
	steal := *shared
	steal.ProjectID = &y.ID
	if err := db.UpdateTeamPreset(ctx, &steal); !isInvalid(err) {
		t.Errorf("Y took a team X is running: %v", err)
	}
	p, err := db.GetProject(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.TeamPresetID == nil || *p.TeamPresetID != shared.ID {
		t.Errorf("X ended up on %v, want the team it was running", p.TeamPresetID)
	}
}

// The finisher is chosen and stays chosen.
//
// It used to be whichever enabled role sat last, so adding a role to the end of
// a pipeline handed the job of integrating to it, silently, taking it from the
// role that had been doing it.
func TestTheTerminalRoleIsAFlagAndStaysLast(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	coder, reviewer, docs := templateID(t, db, "coder"), templateID(t, db, "reviewer"), templateID(t, db, "docs")

	preset, err := db.CreateTeamPreset(ctx, &TeamPreset{Name: "Pipeline", Roles: []TeamPresetRole{
		{TemplateID: coder, Enabled: true},
		{TemplateID: reviewer, Enabled: true, Terminal: true},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Appending a role does not take terminality with it: the finisher moves
	// back to the end, and the new role lands in front of it. This is why
	// adding a role does not have to know anything about terminality.
	preset.Roles = append(preset.Roles, TeamPresetRole{TemplateID: docs, Enabled: true})
	if err := db.UpdateTeamPreset(ctx, preset); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetTeamPreset(ctx, preset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := roleOrder(t, db, ctx, after); got != "coder,docs,reviewer*" {
		t.Errorf("pipeline is %s, want the finisher still last", got)
	}

	// Flagging another role moves that one to the end instead, and the old
	// finisher keeps its place in the order.
	for i := range after.Roles {
		after.Roles[i].Terminal = after.Roles[i].TemplateID == docs
	}
	if err := db.UpdateTeamPreset(ctx, after); err != nil {
		t.Fatal(err)
	}
	after, err = db.GetTeamPreset(ctx, preset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := roleOrder(t, db, ctx, after); got != "coder,reviewer,docs*" {
		t.Errorf("pipeline is %s, want docs finishing", got)
	}

	// Parking the finisher hands the job to the last role still running,
	// because a pipeline whose only finisher is off has nowhere to deliver.
	for i := range after.Roles {
		if after.Roles[i].TemplateID == docs {
			after.Roles[i].Enabled = false
		}
	}
	if err := db.UpdateTeamPreset(ctx, after); err != nil {
		t.Fatal(err)
	}
	after, err = db.GetTeamPreset(ctx, preset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := roleOrder(t, db, ctx, after); got != "coder,reviewer*,docs(off)" {
		t.Errorf("pipeline is %s, want reviewer finishing and docs parked where it was", got)
	}
}

// roleOrder renders a team as "a,b*,c(off)": order, the finisher starred, and
// what is parked.
func roleOrder(t *testing.T, db *DB, ctx context.Context, preset *TeamPreset) string {
	t.Helper()
	var parts []string
	for _, r := range preset.Roles {
		tpl, err := db.GetTemplate(ctx, r.TemplateID)
		if err != nil {
			t.Fatal(err)
		}
		s := tpl.Name
		if r.Terminal {
			s += "*"
		}
		if !r.Enabled {
			s += "(off)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

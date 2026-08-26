package store

import (
	"context"
	"database/sql"
	"path/filepath"
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

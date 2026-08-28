package store

import (
	"context"
	"strings"
	"testing"
)

// The runner is a role, which is the whole point: an agent whose harness,
// model, thinking level and prompt are rows in the same table as every other
// agent's, edited in the same screen.
func TestTheRunnerIsAConfigurableRoleAndNotAPipelineOne(t *testing.T) {
	ctx := context.Background()
	db, _ := seeded(t)

	found, err := db.RoleFor(ctx, PurposeRunner)
	if err != nil {
		t.Fatalf("no runner in the seeded library: %v", err)
	}
	if found.Prompt == "" || found.Harness == "" {
		t.Errorf("the runner arrived unconfigured: %+v", found)
	}

	// Configurable like any other: this is the thing that was impossible when
	// its prompt lived in the binary.
	found.Model = "opus"
	found.Thinking = "high"
	found.Prompt = "Serve the admin portal, never the customer one."
	if err := db.UpdateTemplate(ctx, found); err != nil {
		t.Fatalf("editing the runner: %v", err)
	}
	again, err := db.RoleFor(ctx, PurposeRunner)
	if err != nil {
		t.Fatal(err)
	}
	if again.Model != "opus" || again.Thinking != "high" ||
		again.Prompt != "Serve the admin portal, never the customer one." {
		t.Errorf("the edit did not stick: %+v", again)
	}

	// And it is not something a pipeline can contain.
	all, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pipeline int
	for _, r := range all {
		if r.Purpose == PurposePipeline {
			pipeline++
		}
		if r.Name == "runner" && r.Purpose != PurposeRunner {
			t.Errorf("the runner's purpose is %q", r.Purpose)
		}
	}
	if pipeline != len(all)-1 {
		t.Errorf("%d of %d roles are pipeline roles; only the runner should not be", pipeline, len(all))
	}
}

// Every role that existed before purpose did is a pipeline role, without
// anything being rewritten.
func TestARoleWithNoPurposeIsAPipelineRole(t *testing.T) {
	ctx := context.Background()
	db, _ := seeded(t)

	made, err := db.CreateTemplate(ctx, &RoleTemplate{
		Name: "tester", Harness: "claude", Receive: ReceiveTask, Gate: GateNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if made.Purpose != PurposePipeline {
		t.Errorf("purpose = %q, want the pipeline by default", made.Purpose)
	}
}

// The runner cannot be put in a pipeline, and it is the store that says so.
//
// It was said only by the cockpit, in one filter, in one of the two lists that
// show the library -- so the sidebar offered it, and nothing between that list
// and the table objected. In a team it would get a lane, be routed cards, and
// be minted the unscoped token a team role gets: a preview agent able to claim
// and finish somebody's work.
func TestATeamCannotContainARoleThePipelineDoesNotRun(t *testing.T) {
	ctx := context.Background()
	db, project := seeded(t)

	runner, err := db.RoleFor(ctx, PurposeRunner)
	if err != nil {
		t.Fatal(err)
	}
	coder, err := db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.CreateTeamPreset(ctx, &TeamPreset{
		Name: "With a runner in it", ProjectID: &project.ID,
		Roles: []TeamPresetRole{
			{TemplateID: coder.ID, Enabled: true},
			{TemplateID: runner.ID, Enabled: true},
		},
	})
	if err == nil {
		t.Fatal("a team was created with the runner in its pipeline")
	}
	if !isInvalid(err) {
		t.Errorf("err = %v, want one the API renders as a 400", err)
	}
	if !strings.Contains(err.Error(), "runner") {
		t.Errorf("err = %v, want it to name the role", err)
	}

	// And the team was not half-written: the refusal happens inside the same
	// transaction as the insert, so a rejected team leaves nothing behind.
	presets, err := db.ListTeamPresets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range presets {
		if p.Name == "With a runner in it" {
			t.Error("the rejected team was stored anyway")
		}
	}
}

// A row that got in before the rule existed, or by hand, still never runs.
//
// The write path only guards new mistakes. This is the half that guards a
// database that already has one -- including a role whose purpose changed
// after it joined a team, which no write to the team would catch.
func TestATeamRowForANonPipelineRoleNeverResolves(t *testing.T) {
	ctx := context.Background()
	db, project := seeded(t)

	before, err := db.ResolveTeam(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Straight into the table, which is the state this defends against.
	runner, err := db.RoleFor(ctx, PurposeRunner)
	if err != nil {
		t.Fatal(err)
	}
	var presetID string
	if err := db.read.QueryRowContext(ctx,
		`SELECT preset_id FROM team_preset_roles LIMIT 1`).Scan(&presetID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO team_preset_roles (preset_id, template_id, position, enabled)
		 VALUES (?,?,?,1)`, presetID, runner.ID, 99); err != nil {
		t.Fatal(err)
	}

	after, err := db.ResolveTeam(ctx, project.ID)
	if err != nil {
		t.Fatalf("the team stopped resolving: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("team resolved to %d roles, want the same %d as before the bad row",
			len(after), len(before))
	}
	for _, r := range after {
		if r.Name == runner.Name {
			t.Error("the runner resolved into the pipeline and would be minted a full token")
		}
	}
}

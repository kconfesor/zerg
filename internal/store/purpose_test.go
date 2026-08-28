package store

import (
	"context"
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

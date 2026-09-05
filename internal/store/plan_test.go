package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSubmitPlanIsInert(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	feat, err := db.CreateFeature(ctx, p.ID, "Billing", "rewrite invoicing")
	if err != nil {
		t.Fatal(err)
	}

	rev, err := db.SubmitPlan(ctx, p.ID, feat.ID, []PlanDraft{
		{Name: "Schema", Body: "the tables", Priority: 10},
		{Name: "API", Body: "the handlers", After: []string{"Schema"}},
	}, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if rev.State != PlanPending || rev.ItemCount != 2 || rev.Digest == "" || rev.ProseSHA != "abc123" {
		t.Fatalf("revision = %+v", rev)
	}

	board, err := db.ListTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 0 {
		t.Fatalf("board has %d cards after a split; a plan must not create work", len(board))
	}

	pending, err := db.ListPendingPlans(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != rev.ID {
		t.Fatalf("pending = %d, want the revision", len(pending))
	}
	if len(pending[0].Items) != 2 {
		t.Fatalf("items = %d, want 2", len(pending[0].Items))
	}
	var api PlanItem
	for _, it := range pending[0].Items {
		if it.Name == "API" {
			api = it
		}
	}
	if len(api.After) != 1 || api.After[0] != "Schema" {
		t.Errorf("API.after = %v, want [Schema]", api.After)
	}

	if _, err := db.SubmitPlan(ctx, p.ID, feat.ID, []PlanDraft{{Name: "Other"}}, ""); err == nil {
		t.Error("a second plan was accepted while one is pending")
	}

	if _, err := db.RejectPlan(ctx, rev.ID, "", OperatorRole); err == nil {
		t.Error("a rejection without a note was accepted")
	}
	if _, err := db.RejectPlan(ctx, rev.ID, "drop API, keep schema", OperatorRole); err != nil {
		t.Fatal(err)
	}

	need, note, err := db.NextFeatureToPlan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if need == nil || need.ID != feat.ID {
		t.Fatal("a rejected plan should put the feature back in front of the architect")
	}
	if note != "drop API, keep schema" {
		t.Errorf("note = %q, want the rejection", note)
	}

	rev2, err := db.SubmitPlan(ctx, p.ID, feat.ID, []PlanDraft{{Name: "Schema", Body: "just the tables"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rev2.N != 2 {
		t.Errorf("n = %d, want 2", rev2.N)
	}
	if rev2.Digest == rev.Digest {
		t.Error("a different plan hashed the same")
	}
	// Accepting is nydus.AcceptPlan, which materialises; this is the state it
	// leaves behind, written directly because the store cannot reach it.
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE feature_plan_revisions SET state = ? WHERE id = ?`, PlanApproved, rev2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SubmitPlan(ctx, p.ID, feat.ID, []PlanDraft{{Name: "More"}}, ""); err == nil {
		t.Error("a split after acceptance was allowed")
	}
	need, _, err = db.NextFeatureToPlan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if need != nil {
		t.Error("an accepted plan is still being offered to the architect")
	}
}

func TestSubmitPlanRefusesACycle(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	feat, err := db.CreateFeature(ctx, p.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SubmitPlan(ctx, p.ID, feat.ID, []PlanDraft{
		{Name: "A", After: []string{"B"}},
		{Name: "B", After: []string{"A"}},
	}, "")
	if err == nil {
		t.Fatal("a cycle was accepted")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want it to name the cycle", err)
	}
}

func TestSubmitPlanRefusesAWorkCard(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	card, err := db.CreateTask(ctx, p.ID, "Typo", "", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SubmitPlan(ctx, p.ID, card.ID, []PlanDraft{{Name: "X"}}, ""); err == nil {
		t.Error("split on a work card was allowed")
	}
}

func TestHasWorkForSupervisorIncludesAFeatureToPlan(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	want, err := db.HasWorkForSupervisor(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want {
		t.Fatal("an empty project wanted a sidecar")
	}
	if _, err := db.CreateFeature(ctx, p.ID, "Billing", ""); err != nil {
		t.Fatal(err)
	}
	want, err = db.HasWorkForSupervisor(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !want {
		t.Error("a feature with no plan did not want a sidecar")
	}
}

// Naming a dependency twice means the same edge. Left as two, the second insert
// broke the primary key and came back as a 500 with SQLite's wording, on the
// one path where the caller is an agent that could have fixed its own JSON.
func TestADependencyNamedTwiceIsOneEdge(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	feat, err := db.CreateFeature(ctx, p.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := db.SubmitPlan(ctx, p.ID, feat.ID, []PlanDraft{
		{Name: "Schema"},
		{Name: "API", After: []string{"Schema", "Schema"}},
	}, "")
	if err != nil {
		t.Fatalf("a plan naming one dependency twice was refused: %v", err)
	}
	got, err := db.GetPlan(ctx, rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range got.Items {
		if it.Name == "API" && len(it.After) != 1 {
			t.Errorf("API.after = %v, want one edge", it.After)
		}
	}
}

// A feature with every card done is waiting to be landed, and deleting it there
// cascaded the run, the plan and the review away: the land vanished out of
// Attention and the branch it was about stayed on disk with nothing pointing
// at it.
func TestAFeatureWaitingToLandCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	feat, err := db.CreateFeature(ctx, p.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO feature_runs (feature_id, branch, base_sha, head_sha, state, created_at)
		 VALUES (?,?,?,?,?,?)`,
		feat.ID, "zerg-feature/"+feat.ID, "aaaa", "bbbb", FeatureRunning, "2026-09-05T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	err = db.DeleteTask(ctx, p.ID, feat.ID)
	if err == nil {
		t.Fatal("a feature with work waiting to land was deleted")
	}
	var bad *ValidationError
	if !errors.As(err, &bad) {
		t.Errorf("error was %v, want one the operator can act on", err)
	}
	if !strings.Contains(err.Error(), "zerg-feature/"+feat.ID) {
		t.Errorf("refusal was %q, which does not name the branch the work is on", err)
	}

	// Cancelled, it can go.
	if err := db.CancelFeatureRun(ctx, feat.ID, "zerg-feature/"+feat.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteTask(ctx, p.ID, feat.ID); err != nil {
		t.Errorf("a cancelled feature could not be deleted: %v", err)
	}
}

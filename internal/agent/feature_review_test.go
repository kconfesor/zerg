package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/store"
)

func TestWholeReviewCarriesTheAcceptedScopeAndOriginalBrief(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feature, err := f.db.CreateFeature(ctx, f.project.ID, "Feature", "The original acceptance requirements")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.db.SubmitPlan(ctx, f.project.ID, feature.ID, []store.PlanDraft{{Name: "Implementation", Body: "The agreed scope"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.nyd.NewTaskWith(ctx, nydus.NewTaskOpts{ProjectID: f.project.ID, ParentID: feature.ID, Name: "Implementation"})
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's integrator does not run git; arrange an already integrated
	// plan. Real git and acceptance transitions are covered by nydus and api.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE feature_plan_revisions SET state = 'approved' WHERE id = ?`, []any{plan.ID}},
		{`UPDATE feature_plan_items SET child_task_id = ? WHERE revision_id = ?`, []any{child.ID, plan.ID}},
		{`UPDATE tasks SET state = 'done' WHERE id = ?`, []any{child.ID}},
		{`INSERT INTO feature_runs (feature_id, branch, base_sha, head_sha, state, created_at) VALUES (?, 'feature', 'base', 'reviewed-head', 'running', '2026-07-01')`, []any{feature.ID}},
	} {
		if _, err := f.db.SQL().ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	sup := NewClient(f.socket, f.srv.MintScoped(f.project.ID, "supervisor", CanClaim, CanDecide))
	work, err := sup.Next(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if work.Kind != "review" || work.Task == nil || work.Task.Body != feature.Body || work.Plan == nil || work.Plan.ID != plan.ID || len(work.Plan.Items) != 1 || work.Plan.Items[0].ChildTaskID != child.ID {
		t.Fatalf("review lost the accepted requirements: %+v", work)
	}
	if work.Commit != "reviewed-head" || !strings.Contains(work.Body, "commands, observed results, missing requirements") {
		t.Fatalf("review is not bound to observable acceptance: %+v", work)
	}
	owned, err := f.db.CurrentTaskFor(ctx, f.project.ID, "supervisor")
	if err != nil || owned == nil || *owned != feature.ID {
		t.Fatalf("review spend has no owner: %v %v", owned, err)
	}
}

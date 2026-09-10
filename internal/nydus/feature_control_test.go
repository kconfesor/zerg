package nydus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

// Existing integration tests still exercise the whole send/decision boundary.
// Gate-specific tests call Send alone to assert nothing integrates before it.
func (f *fixture) finishChild(ctx context.Context, project, role string, req SendRequest) (*store.Message, error) {
	msg, err := f.n.Send(ctx, project, role, req)
	if err != nil {
		return msg, err
	}
	approvals, err := f.db.ListPendingApprovals(ctx, project)
	if err != nil {
		return msg, err
	}
	for _, a := range approvals {
		if a.MessageID == msg.ID && a.FeatureID != "" {
			err = f.n.ApproveBy(ctx, store.DecisionScope{ProjectID: project, Role: "supervisor"}, a.ID, "checked the subtask", "")
			return msg, err
		}
	}
	return msg, nil
}

func featureHead(t *testing.T, f *fixture, id string) string {
	t.Helper()
	run, err := f.db.GetFeatureRun(context.Background(), id)
	if err != nil || run == nil {
		t.Fatalf("feature run: %v %v", run, err)
	}
	return run.HeadSHA
}

func featureTree(t *testing.T, f *fixture) string {
	t.Helper()
	ctx := context.Background()
	for _, kv := range [][2]string{{"user.email", "test@example.com"}, {"user.name", "Test"}} {
		if _, err := runGit(ctx, f.project.Path, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := hatchery.New(f.project.Path).EnsureWorktree(ctx, "reviewer", "main")
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func finishedFeature(t *testing.T) (*fixture, *store.Task, store.Task, string) {
	t.Helper()
	f := newFixture(t, WithIntegrator(Git{}))
	feat, child := acceptOne(t, f)
	tree := featureTree(t, f)
	write(t, tree, "feature.txt", "feature\n")
	sha := commitAll(t, tree, "feature work")
	if _, err := f.finishChild(context.Background(), f.project.ID, "reviewer", SendRequest{TaskID: child.ID, Commit: sha, Body: "implemented the feature"}); err != nil {
		t.Fatal(err)
	}
	return f, feat, child, sha
}

func TestAcceptedScopeCannotBeRemovedOrExpanded(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, child := acceptOne(t, f)
	for _, parent := range []string{"", feat.ID} {
		err := f.db.SetTaskParent(ctx, child.ID, parent)
		if parent == "" && err == nil {
			t.Fatal("detached approved work")
		}
		if parent == feat.ID && err != nil {
			t.Fatal("idempotent membership failed:", err)
		}
	}
	if err := f.db.DeleteTask(ctx, f.project.ID, child.ID); err == nil {
		t.Fatal("deleted approved work")
	}
	if _, err := f.n.NewTaskWith(ctx, NewTaskOpts{ProjectID: f.project.ID, Name: "Unapproved", ParentID: feat.ID}); err == nil {
		t.Fatal("expanded an accepted plan")
	}
	ordinary := f.task(t, "Ordinary")
	if err := f.db.SetTaskParent(ctx, ordinary.ID, feat.ID); err == nil {
		t.Fatal("grouped unapproved work into a live run")
	}
	got, err := f.db.GetTask(ctx, child.ID)
	if err != nil || got.ParentID != feat.ID {
		t.Fatalf("membership changed: %v %v", got, err)
	}

	// A database left by the buggy version can already be missing a requirement.
	// Even an existing OK review must not turn that damaged scope into a land.
	if _, err := f.finishChild(ctx, f.project.ID, "reviewer", SendRequest{TaskID: child.ID, Commit: "aaaaaaaaaa", Body: "done"}); err != nil {
		t.Fatal(err)
	}
	head := featureHead(t, f, feat.ID)
	if _, err := f.db.SubmitReview(ctx, feat.ID, head, store.ReviewOK, "checked", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL().ExecContext(ctx, `UPDATE tasks SET parent_id = NULL WHERE id = ?`, child.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID, head); err == nil {
		t.Fatal("landed an incomplete accepted scope")
	}
	if len(f.git.merges) != 0 {
		t.Fatal("base changed despite missing planned work")
	}
}

func TestSubtaskIntegrationWaitsForItsDelegatedGate(t *testing.T) {
	for _, gate := range []string{store.GateNone, store.GateApproval} {
		t.Run(gate, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, WithIntegrator(Git{}))
			feat, err := f.db.CreateFeature(ctx, f.project.ID, "Feature", "A before B")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{{Name: "A"}, {Name: "B", After: []string{"A"}}}, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.n.AcceptPlan(ctx, plan.ID); err != nil {
				t.Fatal(err)
			}
			a, _ := f.db.GetTaskByName(ctx, f.project.ID, "A")
			b, _ := f.db.GetTaskByName(ctx, f.project.ID, "B")
			if _, err := f.db.SQL().ExecContext(ctx, `UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, gate); err != nil {
				t.Fatal(err)
			}
			tree := featureTree(t, f)
			hat := hatchery.New(f.project.Path)
			if _, err := hat.EnsureWorktree(ctx, "planner", "main"); err != nil {
				t.Fatal(err)
			}
			first, err := f.n.Claim(ctx, f.project.ID, "planner")
			if err != nil || first == nil {
				t.Fatal("claim:", err)
			}
			write(t, tree, "dependency.txt", "A\n")
			sha := commitAll(t, tree, "A")
			msg, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{TaskID: a.ID, Commit: sha, Body: "A completed"})
			if err != nil {
				t.Fatal(err)
			}
			if msg.Terminal {
				t.Fatal("a feature integration is not a base land")
			}
			before, err := (Git{}).Contains(ctx, f.project.Path, featureHead(t, f, feat.ID), sha)
			if err != nil || before {
				t.Fatal("integrated before decision:", err)
			}
			b, err = f.db.GetTask(ctx, b.ID)
			if err != nil || !b.Blocked {
				t.Fatal("released B before decision:", err)
			}
			d, err := f.db.NextDecision(ctx, f.project.ID, "supervisor")
			if err != nil || d == nil || d.Approval == nil || d.Approval.FeatureID != feat.ID {
				t.Fatalf("missing delegated decision: %+v %v", d, err)
			}
			if err := f.n.ApproveBy(ctx, store.DecisionScope{ProjectID: f.project.ID, Role: "supervisor", Model: "test-model"}, d.Approval.ID, "A meets its requirement", ""); err != nil {
				t.Fatal(err)
			}
			if err := f.n.Ack(ctx, first.ID); err != nil {
				t.Fatal(err)
			}
			next, err := f.n.Claim(ctx, f.project.ID, "planner")
			if err != nil || next == nil || *next.Items[0].TaskID != b.ID {
				t.Fatalf("dependent not claimable: %+v %v", next, err)
			}
			body, err := os.ReadFile(filepath.Join(hat.Path("planner"), "dependency.txt"))
			if err != nil || string(body) != "A\n" {
				t.Fatalf("dependent did not receive A: %q %v", body, err)
			}
			if _, err := os.Stat(filepath.Join(f.project.Path, "dependency.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("subtask reached base")
			}
		})
	}
}

func TestFeatureRefreshAndHumanReworkKeepTheWholeReviewable(t *testing.T) {
	ctx := context.Background()
	f, feat, child, sha := finishedFeature(t)
	if _, err := f.db.SubmitReview(ctx, feat.ID, sha, store.ReviewOK, "checked original brief", ""); err != nil {
		t.Fatal(err)
	}
	write(t, f.project.Path, "unrelated.txt", "base moved\n")
	commitAll(t, f.project.Path, "unrelated base work")
	if err := f.n.LandFeature(ctx, feat.ID, sha); err == nil {
		t.Fatal("expected a non-fast-forward")
	}
	if err := f.n.RefreshFeature(ctx, feat.ID, sha); err != nil {
		t.Fatal(err)
	}
	updated := featureHead(t, f, feat.ID)
	if updated == sha {
		t.Fatal("refresh did not record a new head")
	}
	if review, err := f.db.CurrentReview(ctx, feat.ID); err != nil || review != nil {
		t.Fatalf("old verdict survived refresh: %v %v", review, err)
	}
	if _, err := f.db.SubmitReview(ctx, feat.ID, sha, store.ReviewOK, "late old review", ""); err == nil {
		t.Fatal("accepted stale review")
	}
	if _, err := f.db.SubmitReview(ctx, feat.ID, updated, store.ReviewOK, "checked refreshed tree", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID, sha); err == nil {
		t.Fatal("landed on stale human consent")
	}
	if err := f.n.RejectFeature(ctx, feat.ID, updated, "the operator found a missing acceptance check"); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID, updated); err == nil {
		t.Fatal("ignored human rejection")
	}
	if err := f.n.RetryChild(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	tree := hatchery.New(f.project.Path).Path("reviewer")
	if err := (Git{}).Switch(ctx, tree, SubtaskBranch("reviewer"), updated); err != nil {
		t.Fatal(err)
	}
	write(t, tree, "acceptance.txt", "checked\n")
	repaired := commitAll(t, tree, "repair")
	if _, err := f.finishChild(ctx, f.project.ID, "reviewer", SendRequest{TaskID: child.ID, Commit: repaired, Body: "added the missing check"}); err != nil {
		t.Fatal(err)
	}
	head := featureHead(t, f, feat.ID)
	if _, err := f.db.SubmitReview(ctx, feat.ID, head, store.ReviewOK, "checked the acceptance result", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID, head); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"feature.txt", "unrelated.txt", "acceptance.txt"} {
		if _, err := os.Stat(filepath.Join(f.project.Path, file)); err != nil {
			t.Fatal(file, err)
		}
	}
}

func TestCancelledFeatureRefusesALateSplit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Cancelled", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.n.CancelFeature(ctx, feat.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.n.SubmitFeaturePlan(ctx, store.DecisionScope{ProjectID: f.project.ID, Role: "supervisor"}, feat.ID, []store.PlanDraft{{Name: "Too late"}}, "")
	var bad *store.ValidationError
	if !errors.As(err, &bad) {
		t.Fatalf("late split error = %v; want operator-fixable refusal", err)
	}
	plans, err := f.db.ListPendingPlans(ctx, f.project.ID)
	if err != nil || len(plans) != 0 {
		t.Fatalf("cancelled feature asked for a decision: %v %v", plans, err)
	}
}

func TestFeatureHistoryAndCostSurviveLandingAndChildDeletion(t *testing.T) {
	ctx := context.Background()
	f, feat, child, sha := finishedFeature(t)
	for _, entry := range []struct {
		id   string
		cost float64
	}{{child.ID, 3}, {feat.ID, 2}} {
		if err := f.db.RecordUsage(ctx, store.UsageTurn{ProjectID: f.project.ID, TaskID: &entry.id, Role: "supervisor", Harness: "claude", Provider: "anthropic", Model: "test", CostUSD: entry.cost, Billing: "metered", CostSource: store.CostFromHarness}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.SubmitReview(ctx, feat.ID, sha, store.ReviewOK, "verified the original requirement", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID, sha); err != nil {
		t.Fatal(err)
	}
	if err := f.db.DeleteTask(ctx, f.project.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	detail, err := f.db.FeatureDetail(ctx, feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Usage.CostUSD != 5 || len(detail.Plans) != 1 || len(detail.Reviews) != 1 {
		t.Fatalf("incomplete durable feature: %+v", detail)
	}
	var landed bool
	for _, step := range detail.History {
		landed = landed || strings.Contains(step.Body, "approved by the operator")
	}
	if !landed {
		t.Fatal("feature trail omits the human's land")
	}
	history, _, err := f.db.ListHistory(ctx, f.project.ID, store.HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range history {
		if entry.ID == feat.ID {
			found = true
			if entry.CostUSD != 5 {
				t.Fatal("history lost child cost:", entry.CostUSD)
			}
		}
	}
	if !found {
		t.Fatal("landed feature disappeared from history")
	}
}

func TestRejectedIntegrationReturnsTheUnintegratedWork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, WithIntegrator(Git{}))
	feat, child := acceptOne(t, f)
	tree := featureTree(t, f)
	base := featureHead(t, f, feat.ID)
	write(t, tree, "repair-me.txt", "retain this work\n")
	sha := commitAll(t, tree, "work needing correction")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{TaskID: child.ID, Commit: sha, Body: "check this"}); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v %v", pending, err)
	}
	if err := f.n.Reject(ctx, pending[0].ID, "add the missing check"); err != nil {
		t.Fatal(err)
	}
	if err := (Git{}).Switch(ctx, tree, "zerg-reviewer", base); err != nil {
		t.Fatal(err)
	}
	work, err := f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil || work == nil {
		t.Fatalf("no correction delivered: %v %v", work, err)
	}
	body, err := os.ReadFile(filepath.Join(tree, "repair-me.txt"))
	if err != nil || string(body) != "retain this work\n" {
		t.Fatalf("the correction reset lost its source: %q %v", body, err)
	}
	if featureHead(t, f, feat.ID) != base {
		t.Fatal("rejected work entered the feature")
	}
}

func TestInterruptedSubtaskApprovalRecoversExactlyOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, WithIntegrator(Git{}))
	feat, child := acceptOne(t, f)
	tree := featureTree(t, f)
	write(t, tree, "recover.txt", "durable\n")
	sha := commitAll(t, tree, "work")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{TaskID: child.ID, Commit: sha, Body: "finished"}); err != nil {
		t.Fatal(err)
	}
	approvals, err := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("pending: %v %v", approvals, err)
	}
	if _, err := f.db.SQL().ExecContext(ctx, `UPDATE approvals SET state = ?, decided_by = 'supervisor', note = 'checked before crash' WHERE id = ?`, store.ApprovalIntegrating, approvals[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.n.CancelFeature(ctx, feat.ID); err == nil {
		t.Fatal("cancel discarded a claimed integration")
	}
	if err := f.db.StopTask(ctx, f.project.ID, child.ID); err == nil {
		t.Fatal("stop discarded a claimed integration")
	}
	if err := (Git{}).MergeInto(ctx, hatchery.New(f.project.Path).Path(FeatureWorktree(feat.ID)), sha); err != nil {
		t.Fatal(err)
	}
	settled, _, err := f.n.ReconcileIntegrating(ctx)
	if err != nil || settled != 1 {
		t.Fatalf("recovery: %d %v", settled, err)
	}
	if featureHead(t, f, feat.ID) != sha {
		t.Fatal("recorded head did not catch up with git")
	}
	got, err := f.db.GetTask(ctx, child.ID)
	if err != nil || got.State != store.TaskDone {
		t.Fatalf("child: %v %v", got, err)
	}
	approval, err := f.db.GetApproval(ctx, approvals[0].ID)
	if err != nil || approval.State != store.ApprovalApproved {
		t.Fatalf("approval: %v %v", approval, err)
	}
	if settled, _, err := f.n.ReconcileIntegrating(ctx); err != nil || settled != 0 {
		t.Fatal("recovery was not idempotent", settled, err)
	}
}

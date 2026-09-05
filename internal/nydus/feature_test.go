package nydus

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

func TestAcceptPlanQueuesIndependentsAndBlocksDependents(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "rewrite")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{
		{Name: "Schema", Body: "the tables", Priority: 10},
		{Name: "API", Body: "the handlers", After: []string{"Schema"}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.AcceptPlan(ctx, rev.ID); err != nil {
		t.Fatal(err)
	}

	board, err := f.db.ListTasks(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 2 {
		t.Fatalf("board has %d cards, want 2 subtasks", len(board))
	}
	byName := map[string]store.Task{}
	for _, c := range board {
		if c.Kind == store.TaskKindFeature {
			t.Fatal("the feature appeared in a lane")
		}
		byName[c.Name] = c
	}
	if byName["Schema"].Blocked || byName["Schema"].Priority != 10 {
		t.Errorf("Schema blocked=%v priority=%d, want queued at 10", byName["Schema"].Blocked, byName["Schema"].Priority)
	}
	if !byName["API"].Blocked {
		t.Error("API was queued before Schema was integrated")
	}
	if byName["Schema"].ParentID != feat.ID || byName["API"].ParentID != feat.ID {
		t.Error("subtasks lost their feature")
	}
}

func TestFinishingASubtaskIntegratesIntoTheFeatureNotBase(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{
		{Name: "Schema", Body: "the tables"},
		{Name: "API", Body: "the handlers", After: []string{"Schema"}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.AcceptPlan(ctx, rev.ID); err != nil {
		t.Fatal(err)
	}
	board, err := f.db.ListTasks(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var schema, api store.Task
	for _, c := range board {
		switch c.Name {
		case "Schema":
			schema = c
		case "API":
			api = c
		}
	}

	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: schema.ID, Commit: "aaaaaaaaaa", Body: "schema is in",
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.git.merges) != 0 {
		t.Errorf("base merged %v; a subtask must not land", f.git.merges)
	}
	if len(f.git.into) == 0 {
		t.Error("the feature head was not merged into")
	}

	got, err := f.db.GetTask(ctx, schema.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.TaskDone {
		t.Errorf("Schema state = %s, want done", got.State)
	}
	featGot, err := f.db.GetTask(ctx, feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if featGot.State == store.TaskDone {
		t.Error("the feature was marked done because its children finished")
	}

	apiGot, err := f.db.GetTask(ctx, api.ID)
	if err != nil {
		t.Fatal(err)
	}
	if apiGot.Blocked {
		t.Error("API is still blocked after its dependency was integrated")
	}
}

func TestAFeatureDoesNotLandBecauseItsChildrenFinished(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, schema := acceptOne(t, f)
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: schema.ID, Commit: "aaaaaaaaaa", Body: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID); err == nil {
		t.Fatal("landed without a review")
	}
	if _, err := f.db.SubmitReview(ctx, feat.ID, store.ReviewOK, "looks whole", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.git.merges) == 0 {
		t.Error("base was not merged; the feature never landed")
	}
	got, err := f.db.GetTask(ctx, feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.TaskDone {
		t.Errorf("feature state = %s, want done", got.State)
	}
}

func TestAStaleReviewCannotLand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, schema := acceptOne(t, f)
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: schema.ID, Commit: "aaaaaaaaaa", Body: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SubmitReview(ctx, feat.ID, store.ReviewOK, "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE feature_runs SET head_sha = ? WHERE feature_id = ?`, "bbbbbbbbbb", feat.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.n.LandFeature(ctx, feat.ID); err == nil {
		t.Fatal("a review of a previous head was allowed to land")
	}
}

func TestCancelFeatureStopsChildren(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, schema := acceptOne(t, f)
	if err := f.n.CancelFeature(ctx, feat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: schema.ID, Commit: "aaaaaaaaaa", Body: "too late",
	}); err == nil {
		t.Fatal("a cancelled feature still accepted a subtask")
	}
}

// A subtask runs on a branch of its own, started at the feature head, and the
// next ordinary card goes back to the role's branch. Leaving the role sitting
// on the feature branch is how an unrelated card came to carry a whole
// unreviewed feature onto the base branch: base is an ancestor of the feature,
// so `merge --ff-only` accepted it and took every commit under it.
func TestAFeatureSubtaskRunsOnItsOwnBranchAndTheNextCardDoesNot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, schema := acceptOne(t, f)
	run, err := f.db.GetFeatureRun(ctx, feat.ID)
	if err != nil || run == nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hatchery.New(f.project.Path).Path("planner"), 0o755); err != nil {
		t.Fatal(err)
	}
	lease, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil {
		t.Fatal("planner had no work")
	}
	want := SubtaskBranch("planner") + "@" + run.HeadSHA
	if len(f.git.switched) == 0 || f.git.switched[0] != want {
		t.Fatalf("switched = %v, want %s", f.git.switched, want)
	}

	// Finish the subtask, then put an ordinary card in front of the same role.
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: schema.ID, Commit: "aaaaaaaaaa", Body: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.NewTask(ctx, f.project.ID, "Unrelated", "nothing to do with the feature", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.Claim(ctx, f.project.ID, "planner"); err != nil {
		t.Fatal(err)
	}
	last := f.git.switched[len(f.git.switched)-1]
	if last != hatchery.BranchPrefix+"planner@" {
		t.Errorf("an ordinary card claimed on %s, want the role's own branch", last)
	}
}

// One conflict must not end the feature. Left in the tree, git refuses every
// later merge in that worktree, and no agent is in there to resolve it.
func TestAConflictedIntegrationIsClearedAndSurfaced(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, schema := acceptOne(t, f)
	f.git.intoErr = errors.New("CONFLICT (content): Merge conflict in f")

	_, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: schema.ID, Commit: "aaaaaaaaaa", Body: "done",
	})
	if err == nil {
		t.Fatal("a conflicted integration reported success")
	}
	if !strings.Contains(err.Error(), "resolve it there") {
		t.Errorf("error was %q, which does not say how to fix it", err)
	}
	if len(f.git.aborted) != 1 {
		t.Errorf("the conflicted merge was left in the feature worktree: %v", f.git.aborted)
	}
	run, err := f.db.GetFeatureRun(ctx, feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.FeatureConflict {
		t.Errorf("run state = %s, want conflict", run.State)
	}
	stalls, err := f.db.ListFeatureStalls(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalls) != 1 || stalls[0].Reason != store.StallConflict {
		t.Fatalf("stalls = %+v, want the feature reported as conflicted", stalls)
	}

	// Resolving it and sending again clears the conflict rather than leaving
	// the feature marked for a problem that is gone.
	f.git.intoErr = nil
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: schema.ID, Commit: "bbbbbbbbbb", Body: "merged the feature head",
	}); err != nil {
		t.Fatal(err)
	}
	run, err = f.db.GetFeatureRun(ctx, feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.FeatureRunning {
		t.Errorf("run state = %s, want running once the merge went in", run.State)
	}
}

// A card grouped under a feature nobody planned is an ordinary card. Routing
// its completion into an integration branch that does not exist left it unable
// to finish at all.
func TestACardGroupedUnderAnUnplannedFeatureStillFinishes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.n.NewTaskWith(ctx, NewTaskOpts{
		ProjectID: f.project.ID, Name: "A card", Body: "grouped by hand",
		ParentID: feat.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "done",
	}); err != nil {
		t.Fatalf("a grouped card could not finish: %v", err)
	}
	got, err := f.db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.TaskDone {
		t.Errorf("state = %s, want done", got.State)
	}
	if len(f.git.merges) != 1 {
		t.Errorf("merges = %v, want the card to land the way any other does", f.git.merges)
	}
}

// The guard behind the branch: even if a role's worktree ends up on a feature
// branch, the commit does not reach the base branch under another card's name.
func TestACardCarryingAFeatureCannotLand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, _ := acceptOne(t, f)
	run, err := f.db.GetFeatureRun(ctx, feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.n.NewTask(ctx, f.project.ID, "Unrelated", "nothing to do with the feature", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.git.contains = map[string]bool{"cccccccccc<-" + run.HeadSHA: true}

	_, err = f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "cccccccccc", Body: "done",
	})
	if err == nil {
		t.Fatal("a commit carrying an unlanded feature was allowed onto base")
	}
	if !strings.Contains(err.Error(), "Billing") {
		t.Errorf("error was %q, which does not name the feature it would have taken", err)
	}
	if len(f.git.merges) != 0 {
		t.Errorf("merged %v anyway", f.git.merges)
	}
}

// Decision 10's named actions, and the list that makes them reachable.
func TestAFailedSubtaskStallsTheFeatureAndCanBeRetried(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{
		{Name: "Schema", Body: "the tables"},
		{Name: "API", Body: "the handlers", After: []string{"Schema"}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.AcceptPlan(ctx, rev.ID); err != nil {
		t.Fatal(err)
	}
	board, err := f.db.ListTasks(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var schema, api store.Task
	for _, c := range board {
		switch c.Name {
		case "Schema":
			schema = c
		case "API":
			api = c
		}
	}
	if err := f.db.StopTask(ctx, f.project.ID, schema.ID); err != nil {
		t.Fatal(err)
	}

	stalls, err := f.db.ListFeatureStalls(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalls) != 1 || stalls[0].Reason != store.StallFailed {
		t.Fatalf("stalls = %+v, want the feature reported as failed", stalls)
	}
	actions := map[string]string{}
	for _, c := range stalls[0].Cards {
		actions[c.Name] = c.Action
	}
	if actions["Schema"] != store.ActionRetry || actions["API"] != store.ActionWaive {
		t.Fatalf("actions = %v, want retry on the failed card and waive on the blocked one", actions)
	}

	if err := f.n.RetryChild(ctx, schema.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.db.GetTask(ctx, schema.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.TaskQueued {
		t.Errorf("state = %s, want the card back in the queue", got.State)
	}
	if _, err := f.n.Claim(ctx, f.project.ID, "planner"); err != nil {
		t.Fatal(err)
	}

	// And a dependency that is not coming can be waived, with the reason on
	// the message the role claims.
	if err := f.n.WaiveDependency(ctx, api.ID, ""); err == nil {
		t.Error("a waiver with no rationale was accepted")
	}
	if err := f.n.WaiveDependency(ctx, api.ID, "the schema is already in main"); err != nil {
		t.Fatal(err)
	}
	got, err = f.db.GetTask(ctx, api.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked {
		t.Error("API is still blocked after the dependency was waived")
	}
}

func acceptOne(t *testing.T, f *fixture) (*store.Task, store.Task) {
	t.Helper()
	ctx := context.Background()
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{
		{Name: "Schema", Body: "the tables"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.AcceptPlan(ctx, rev.ID); err != nil {
		t.Fatal(err)
	}
	board, err := f.db.ListTasks(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 1 {
		t.Fatalf("board has %d, want 1", len(board))
	}
	return feat, board[0]
}

// Two independent subtasks finishing at once is the only parallelism a feature
// has, and it lost one of them: both merged, both resolved, and the later
// transaction wrote the earlier head, so the run pointed below a subtask whose
// card read done. The review and the land then shipped the feature without it.
func TestTwoSubtasksFinishingAtOnceKeepBothInTheHead(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{
		{Name: "Schema", Body: "the tables"},
		{Name: "Docs", Body: "the prose"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.AcceptPlan(ctx, rev.ID); err != nil {
		t.Fatal(err)
	}
	board, err := f.db.ListTasks(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 2 {
		t.Fatalf("board = %d cards, want 2 independent subtasks", len(board))
	}

	// The first finisher is parked between reading the feature head and
	// recording it, which is the window the second one used to slip through.
	release := make(chan struct{})
	f.git.slowResolve = release

	var wg sync.WaitGroup
	errs := make([]error, len(board))
	finish := func(i int, id string) {
		defer wg.Done()
		_, errs[i] = f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
			TaskID: id, Commit: "aaaaaaaaaa" + id[:2], Body: "done",
		})
	}
	wg.Add(2)
	go finish(0, board[0].ID)
	time.Sleep(150 * time.Millisecond) // the first one is now parked
	go finish(1, board[1].ID)
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("subtask %d: %v", i, err)
		}
	}

	run, err := f.db.GetFeatureRun(ctx, feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The fake tree answers HEAD with how many merges it has taken, so the
	// head after both is the second one. Recording the first would be the
	// feature quietly losing a subtask that says it is done.
	if run.HeadSHA != "featurehead-2" {
		t.Errorf("head = %s, want featurehead-2: a finished subtask is not in the head", run.HeadSHA)
	}
	if len(f.git.into) != 2 {
		t.Errorf("merged %v, want both subtasks integrated", f.git.into)
	}
}

// Cancelling before anyone accepted a plan recorded nothing but a rejected
// revision, and a rejected revision is how the architect is asked to try again.
// The operator cancelled the feature and watched it come back.
func TestCancellingBeforeAcceptStopsTheArchitect(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{
		{Name: "Schema", Body: "the tables"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.n.CancelFeature(ctx, feat.ID); err != nil {
		t.Fatal(err)
	}

	next, _, err := f.db.NextFeatureToPlan(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Errorf("the architect was handed %q again after it was cancelled", next.Name)
	}
	want, err := f.db.HasWorkForSupervisor(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want {
		t.Error("the architect is still wanted for a cancelled feature")
	}
	pending, err := f.db.ListPendingPlans(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("a cancelled feature still has %d plans waiting on the operator", len(pending))
	}
	features, err := f.db.ListFeatures(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != 0 {
		t.Error("a cancelled feature is still in the board's strip")
	}
}

// The rows are what the queue reads and the prose is what a person reads;
// nothing bound them if the rows moved between the split and the accept.
func TestAcceptRefusesAPlanWhoseRowsMoved(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.db.SubmitPlan(ctx, f.project.ID, feat.ID, []store.PlanDraft{
		{Name: "Schema", Body: "the tables"},
	}, "cafecafecafe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE feature_plan_items SET body = ? WHERE revision_id = ?`,
		"and everything else as well", rev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.AcceptPlan(ctx, rev.ID); err == nil {
		t.Fatal("a plan whose rows had changed was accepted")
	}
	board, err := f.db.ListTasks(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 0 {
		t.Errorf("cards were created anyway: %v", board)
	}
}

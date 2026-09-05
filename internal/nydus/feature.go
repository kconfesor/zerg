package nydus

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

// FeatureBranch names the branch subtask work integrates into.
func FeatureBranch(featureID string) string { return "zerg-feature/" + featureID }

// FeatureWorktree is the hatchery name for that branch's checkout.
func FeatureWorktree(featureID string) string { return "feature-" + featureID }

// SubtaskBranch names the branch a role commits a feature subtask on.
//
// One per role rather than one per feature: nothing durable lives on it, since
// a subtask's commit is merged into the feature branch the moment it finishes,
// and a branch per feature per role would be a ref left behind by every
// feature this project ever ran.
func SubtaskBranch(role string) string { return hatchery.BranchPrefix + "subtask/" + role }

// liveRun is the feature's integration run when there is still somewhere to
// put work: running, or stopped on a conflict a subtask can still resolve.
func (n *Nydus) liveRun(ctx context.Context, featureID string) (*store.FeatureRun, error) {
	run, err := n.db.GetFeatureRun(ctx, featureID)
	if err != nil || run == nil {
		return nil, err
	}
	if run.State != store.FeatureRunning && run.State != store.FeatureConflict {
		return nil, nil
	}
	return run, nil
}

// AcceptPlan materialises an approved split: the feature branch, the child
// cards, independent ones queued, the rest blocked. One step, because creating
// the children without somewhere for their work to go puts it on base.
func (n *Nydus) AcceptPlan(ctx context.Context, id string) (string, error) {
	rev, err := n.db.GetPlan(ctx, id)
	if err != nil {
		return "", err
	}
	if rev.State != store.PlanPending {
		return "", invalid("that plan is not waiting for a decision")
	}
	if err := n.db.VerifyPlanDigest(ctx, rev); err != nil {
		return "", err
	}
	feature, err := n.db.GetTask(ctx, rev.FeatureID)
	if err != nil {
		return "", err
	}
	project, err := n.db.GetProject(ctx, feature.ProjectID)
	if err != nil {
		return "", err
	}
	team, err := n.db.ResolveTeam(ctx, feature.ProjectID)
	if err != nil {
		return "", err
	}
	first, ok := firstEnabled(team)
	if !ok {
		return "", errNoEnabledRoles(feature.ProjectID)
	}

	baseBranch := project.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	hat := hatchery.New(project.Path)
	if err := hat.EnsureRepo(ctx, baseBranch); err != nil {
		return "", err
	}
	base, err := hat.Resolve(ctx, baseBranch)
	if err != nil {
		return "", err
	}
	branch := FeatureBranch(feature.ID)
	if _, err := hat.EnsureWorktreeAt(ctx, FeatureWorktree(feature.ID), branch, base); err != nil {
		return "", err
	}

	now := n.now()
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("beginning materialise: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE feature_plan_revisions SET state = ?, decided_at = ?, decided_by = ?
		  WHERE id = ? AND state = ?`,
		store.PlanApproved, now.Format(time.RFC3339Nano), store.OperatorRole, id, store.PlanPending)
	if err != nil {
		return "", fmt.Errorf("accepting the plan: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", err
	} else if n == 0 {
		return "", invalid("that plan is not waiting for a decision")
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_runs (feature_id, branch, base_sha, head_sha, state, created_at)
		 VALUES (?,?,?,?,?,?)`,
		feature.ID, branch, base, base, "running", now.Format(time.RFC3339Nano)); err != nil {
		return "", fmt.Errorf("recording the feature run: %w", err)
	}

	for i := range rev.Items {
		it := &rev.Items[i]
		blocked := 0
		if len(it.After) > 0 {
			blocked = 1
		}
		priority := it.Priority
		if priority == 0 {
			priority = 50
		}
		child := store.NewID()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks (id, project_id, name, body, lane, state, created_at,
			                    supervised, kind, parent_id, priority, blocked)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			child, feature.ProjectID, it.Name, it.Body, first.Name, store.TaskQueued,
			now.Format(time.RFC3339Nano), 1, store.TaskKindWork, feature.ID, priority, blocked); err != nil {
			return "", fmt.Errorf("creating subtask %q: %w", it.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE feature_plan_items SET child_task_id = ? WHERE id = ?`, child, it.ID); err != nil {
			return "", fmt.Errorf("linking subtask %q: %w", it.Name, err)
		}
		it.ChildTaskID = child
		if blocked == 1 {
			continue
		}
		if err := n.queueChild(ctx, tx, feature.ProjectID, child, it.Body, first.Name, priority, "", now); err != nil {
			return "", err
		}
	}
	return feature.ProjectID, tx.Commit()
}

func (n *Nydus) queueChild(ctx context.Context, tx *sql.Tx, projectID, taskID, body, first string, priority int, commit string, now time.Time) error {
	kind := store.KindNote
	if commit != "" {
		kind = store.KindHandoff
		if body == "" {
			body = "Unblocked: the feature head includes the work this card depends on."
		}
	}
	msg := &store.Message{
		ID: store.NewID(), ProjectID: projectID, TaskID: &taskID,
		FromRole: store.OperatorRole, Kind: kind, Priority: priority,
		Body: body, CreatedAt: now,
	}
	if commit != "" {
		c := commit
		msg.CommitSHA = &c
	}
	return n.sendIn(ctx, tx, msg, sendReq{
		ProjectID: projectID,
		TaskID:    &taskID,
		FromRole:  store.OperatorRole,
		ToRoles:   []string{first},
		Kind:      kind,
		Priority:  priority,
		Commit:    commit,
		Body:      body,
		gate:      store.GateNone,
	}, now)
}

// integrateChild folds a finished subtask into the feature head and unblocks
// dependents whose work is now in that head. Not a land: the project's
// integration policy is not applied.
func (n *Nydus) integrateChild(ctx context.Context, projectID string, sender store.ResolvedRole, req SendRequest, key string, run *store.FeatureRun) (*store.Message, error) {
	if req.Commit == "" {
		return nil, invalid("finishing a subtask requires the commit to integrate")
	}
	task, err := n.db.GetTaskIn(ctx, projectID, req.TaskID)
	if err != nil {
		return nil, err
	}

	// Everything from here to the commit is one critical section.
	//
	// The merge is a git subprocess and cannot run inside the write
	// transaction, so recording where the feature got to is a second step.
	// Two roles finishing independent subtasks at the same moment ran it
	// interleaved: both merged, both resolved, and the later transaction
	// wrote the earlier head. The branch held both commits and the row held
	// one, so the review and the land shipped a feature with a subtask
	// missing from it while that card read done. Independent subtasks in
	// different roles are the only parallelism this has, so that is the
	// ordinary case rather than an unlucky one.
	n.integrate.Lock()
	defer n.integrate.Unlock()

	// Re-read under the lock: the run was fetched before it was taken, and
	// the head it carried may be a merge behind by now.
	run, err = n.liveRun(ctx, task.ParentID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, invalid("that feature is no longer running")
	}
	expected := run.HeadSHA

	head := req.Commit
	if n.integrator != nil {
		project, err := n.db.GetProject(ctx, projectID)
		if err != nil {
			return nil, err
		}
		tree := hatchery.New(project.Path).Path(FeatureWorktree(task.ParentID))
		if err := n.integrator.MergeInto(ctx, tree, req.Commit); err != nil {
			// A repository someone else is holding is not a conflict, and
			// saying it is would mark the whole feature conflicted and send an
			// agent looking for conflict markers that are not there. Measured:
			// git does not wait for the lock, it fails with
			// "Unable to create '.../index.lock': File exists".
			if busyRepo(err) {
				return nil, fmt.Errorf("the feature's repository is busy; try again: %w", err)
			}
			// Cleared, not left. Nobody works in a feature's integration tree,
			// and git refuses every later merge while the conflict sits in it,
			// so one conflict would end every remaining subtask of the feature
			// with an error none of them caused. Best effort: the merge failure
			// below is the answer either way.
			_ = n.integrator.AbortMerge(ctx, tree)
			if err := n.db.SetFeatureRunState(ctx, task.ParentID, store.FeatureConflict); err != nil {
				return nil, err
			}
			return nil, invalid(
				"%s does not merge into the feature: %v. Merge %s into your worktree, "+
					"resolve it there, commit, and send again",
				short(req.Commit), err, short(run.HeadSHA))
		}
		if sha, err := n.integrator.Resolve(ctx, tree, "HEAD"); err == nil && sha != "" {
			head = sha
		}
	}

	now := n.now()
	msg := &store.Message{
		ID: store.NewID(), ProjectID: projectID, TaskID: &task.ID,
		FromRole: sender.Name, Kind: store.KindHandoff, Priority: task.Priority,
		Body: req.Body, CreatedAt: now,
	}
	if msg.Priority == 0 {
		msg.Priority = 50
	}
	c := req.Commit
	msg.CommitSHA = &c

	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning integration: %w", err)
	}
	defer tx.Rollback()

	if err := ensureOpen(ctx, tx, projectID, task.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, task_id, from_role, kind, priority,
		   commit_sha, body, terminal, created_at, source_lease_id, op_key)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		msg.ID, msg.ProjectID, msg.TaskID, msg.FromRole, msg.Kind, msg.Priority,
		msg.CommitSHA, msg.Body, false, now.Format(time.RFC3339Nano),
		nullable(req.SourceLease), nullable(key)); err != nil {
		return nil, fmt.Errorf("recording integration: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET lane = ?, state = ?, completed_at = ? WHERE id = ? AND project_id = ?`,
		store.LaneDone, store.TaskDone, now.Format(time.RFC3339Nano), task.ID, projectID); err != nil {
		return nil, fmt.Errorf("closing the subtask: %w", err)
	}
	// Back to running in the same statement that moves the head: a subtask
	// that resolved the conflict has cleared it, and leaving the run marked
	// conflicted would keep the feature in Attention for a problem that is
	// gone.
	//
	// Guarded on the head this merge was built on. The lock above is what
	// makes that hold; this is what says so if it ever does not, rather than
	// letting the row quietly disagree with the branch.
	res, err := tx.ExecContext(ctx,
		`UPDATE feature_runs SET head_sha = ?, state = ? WHERE feature_id = ? AND head_sha = ?`,
		head, store.FeatureRunning, task.ParentID, expected)
	if err != nil {
		return nil, fmt.Errorf("advancing the feature head: %w", err)
	}
	if moved, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if moved == 0 {
		return nil, fmt.Errorf(
			"the feature moved while %s was being integrated; nothing was recorded", short(req.Commit))
	}
	if err := n.releaseDependents(ctx, tx, projectID, task.ID, head, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing integration: %w", err)
	}
	return msg, nil
}

func (n *Nydus) releaseDependents(ctx context.Context, tx *sql.Tx, projectID, finishedID, head string, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT item.child_task_id, t.name, t.body, t.priority, t.lane
		   FROM feature_plan_items finished
		   JOIN feature_plan_deps d ON d.from_item = finished.id
		   JOIN feature_plan_items item ON item.id = d.to_item
		   JOIN tasks t ON t.id = item.child_task_id
		  WHERE finished.child_task_id = ? AND t.blocked = 1
		    AND NOT EXISTS (
		        SELECT 1 FROM feature_plan_deps d2
		          JOIN feature_plan_items p2 ON p2.id = d2.from_item
		          JOIN tasks t2 ON t2.id = p2.child_task_id
		         WHERE d2.to_item = item.id AND t2.state <> ?
		    )`, finishedID, store.TaskDone)
	if err != nil {
		return fmt.Errorf("finding dependents: %w", err)
	}
	type ready struct {
		id, body, lane string
		priority       int
	}
	var out []ready
	for rows.Next() {
		var r ready
		var name string
		if err := rows.Scan(&r.id, &name, &r.body, &r.priority, &r.lane); err != nil {
			rows.Close()
			return err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range out {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET blocked = 0 WHERE id = ?`, r.id); err != nil {
			return fmt.Errorf("unblocking %s: %w", r.id, err)
		}
		if err := n.queueChild(ctx, tx, projectID, r.id, r.body, r.lane, r.priority, head, now); err != nil {
			return err
		}
	}
	return nil
}

// SubmitFeaturePlan writes a split, with --commit resolved in the architect's
// own worktree.
//
// The prose commit is the other half of decision 8, and it was being stored as
// the agent typed it: "HEAD", which names a different commit in every tree that
// reads it and none at all once the architect's worktree moves on. That is the
// failure ARCHITECTURE records for handoffs, on a new path.
func (n *Nydus) SubmitFeaturePlan(ctx context.Context, scope store.DecisionScope, featureID string, items []store.PlanDraft, prose string) (*store.PlanRevision, error) {
	prose, err := n.resolveEvidence(ctx, scope, prose)
	if err != nil {
		return nil, err
	}
	return n.db.SubmitPlan(ctx, scope.ProjectID, featureID, items, prose)
}

// SubmitFeatureReview records the architect's verdict, with --commit resolved
// in that role's worktree the way an approval's evidence is.
func (n *Nydus) SubmitFeatureReview(ctx context.Context, scope store.DecisionScope, featureID, verdict, note, evidence string) (*store.FeatureReview, error) {
	evidence, err := n.resolveEvidence(ctx, scope, evidence)
	if err != nil {
		return nil, err
	}
	return n.db.SubmitReview(ctx, featureID, verdict, note, evidence)
}

// LandFeature merges the reviewed head onto base. The architect cannot call
// this: ok on a review is a recommendation, not a merge.
func (n *Nydus) LandFeature(ctx context.Context, featureID string) error {
	feat, err := n.db.GetTask(ctx, featureID)
	if err != nil {
		return err
	}
	if feat.Kind != store.TaskKindFeature {
		return invalid("land is for a feature, not a card")
	}
	run, err := n.db.GetFeatureRun(ctx, featureID)
	if err != nil {
		return err
	}
	if run == nil || run.State != store.FeatureRunning {
		return invalid("that feature is not running")
	}
	review, err := n.db.CurrentReview(ctx, featureID)
	if err != nil {
		return err
	}
	if review == nil || review.Verdict != store.ReviewOK || review.HeadSHA != run.HeadSHA {
		return invalid("this feature has no current review of this head; the architect has to look at it first")
	}
	live, err := n.db.HasLiveChildren(ctx, featureID)
	if err != nil {
		return err
	}
	if live {
		return invalid("this feature still has cards being worked")
	}

	_, outcome, ref, err := n.landApproved(ctx, feat.ProjectID, featureID, run.HeadSHA, review.Note)
	if err != nil {
		return err
	}
	now := n.now().Format(time.RFC3339Nano)
	if _, err := n.db.SQL().ExecContext(ctx,
		`UPDATE tasks SET lane = ?, state = ?, completed_at = ?, outcome = ?, outcome_ref = ? WHERE id = ?`,
		store.LaneDone, store.TaskDone, now, outcome, ref, featureID); err != nil {
		return fmt.Errorf("closing the feature: %w", err)
	}
	if err := n.db.SetFeatureRunState(ctx, featureID, store.FeatureDone); err != nil {
		return err
	}
	n.retireFeatureTree(ctx, feat.ProjectID, featureID)
	// The same hook a card's completion runs: the sweep reclaims what the
	// build left behind, and a landed feature is exactly when there is
	// something to reclaim.
	if n.onTaskDone != nil {
		n.onTaskDone(ctx, feat.ProjectID, featureID, run.HeadSHA)
	}
	return nil
}

// retireFeatureTree removes a finished feature's integration worktree.
//
// Best effort, and the branch is left alone: `PruneMergedBranches` deletes it
// with -d once it has reached the base branch, which is the same rule that
// keeps a cancelled feature's commits reachable from somewhere.
func (n *Nydus) retireFeatureTree(ctx context.Context, projectID, featureID string) {
	project, err := n.db.GetProject(ctx, projectID)
	if err != nil {
		return
	}
	_ = hatchery.New(project.Path).RemoveWorktree(ctx, FeatureWorktree(featureID))
}

// CancelFeature is the named out for a live hierarchy. Children are stopped;
// the feature row stays, so a late write can see it was cancelled rather than
// vanishing into a cascade.
func (n *Nydus) CancelFeature(ctx context.Context, featureID string) error {
	feat, err := n.db.GetTask(ctx, featureID)
	if err != nil {
		return err
	}
	if feat.Kind != store.TaskKindFeature {
		return invalid("that is not a feature")
	}
	run, err := n.db.GetFeatureRun(ctx, featureID)
	if err != nil {
		return err
	}
	if run != nil && run.State == store.FeatureDone {
		return invalid("that feature has already landed")
	}
	children, err := n.db.ListTasks(ctx, feat.ProjectID)
	if err != nil {
		return err
	}
	for _, c := range children {
		if c.ParentID != featureID {
			continue
		}
		if c.State == store.TaskQueued || c.State == store.TaskWorking {
			if err := n.db.StopTask(ctx, feat.ProjectID, c.ID); err != nil {
				return err
			}
		}
	}
	// A split still waiting on the operator goes with it. Left pending it
	// would keep asking for a decision about a feature that was abandoned.
	if err := n.db.RejectPendingPlan(ctx, featureID, "the feature was cancelled"); err != nil {
		return err
	}
	// Recorded even when there is no integration to stop, which is every
	// cancellation before a plan was accepted. Without the row the architect
	// was handed the feature again the moment its plan was rejected, and the
	// operator who cancelled it watched it come back.
	if err := n.db.CancelFeatureRun(ctx, featureID, FeatureBranch(featureID)); err != nil {
		return err
	}
	if err := n.db.CloseFeature(ctx, featureID, store.TaskRejected); err != nil {
		return err
	}
	if run != nil {
		n.retireFeatureTree(ctx, feat.ProjectID, featureID)
	}
	return nil
}

// RetryChild puts a failed subtask back in front of the pipeline, starting from
// the feature head as it stands now.
//
// One of decision 10's named actions, and the one the refusal to review a
// feature with a failed subtask tells the reader to take. From the first role
// rather than the lane it died in: the card is being attempted again, not
// handed on, and the role that produced the work is the one that fixes it.
func (n *Nydus) RetryChild(ctx context.Context, taskID string) error {
	task, err := n.db.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.ParentID == "" {
		return invalid("that card is not part of a feature")
	}
	if task.State != store.TaskRejected {
		return invalid("that card has not failed; only a stopped or rejected card is retried")
	}
	run, err := n.liveRun(ctx, task.ParentID)
	if err != nil {
		return err
	}
	if run == nil {
		return invalid("that feature is not running")
	}
	team, err := n.db.ResolveTeam(ctx, task.ProjectID)
	if err != nil {
		return err
	}
	first, ok := firstEnabled(team)
	if !ok {
		return errNoEnabledRoles(task.ProjectID)
	}

	now := n.now()
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning retry: %w", err)
	}
	defer tx.Rollback()

	// rework_count too: decision 10 says a stall reuses the threshold that
	// already surfaces a card going backward too often, and only agent-driven
	// handoffs were counted. An operator retrying the same card ten times is
	// the same card looping, and should reach the same list.
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET state = ?, lane = ?, blocked = 0, stopped_at = NULL,
		   completed_at = NULL, rework_count = rework_count + 1
		  WHERE id = ?`, store.TaskQueued, first.Name, taskID); err != nil {
		return fmt.Errorf("requeueing %s: %w", taskID, err)
	}
	body := "Retried by the operator. The feature head is where this starts from; " +
		"read the trail above for what failed."
	if err := n.queueChild(ctx, tx, task.ProjectID, taskID, body, first.Name,
		priorityOr(task.Priority), run.HeadSHA, now); err != nil {
		return err
	}
	return tx.Commit()
}

// WaiveDependency releases a blocked subtask whose dependency is not coming.
//
// The rationale is carried on the message the card is claimed with, because the
// role that picks it up is the one that needs to know it is starting without
// something the plan said it would have.
func (n *Nydus) WaiveDependency(ctx context.Context, taskID, note string) error {
	if strings.TrimSpace(note) == "" {
		return invalid("waiving a dependency needs a note: why this card can start without it")
	}
	task, err := n.db.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.ParentID == "" {
		return invalid("that card is not part of a feature")
	}
	if !task.Blocked {
		return invalid("that card is not waiting on a dependency")
	}
	run, err := n.liveRun(ctx, task.ParentID)
	if err != nil {
		return err
	}
	if run == nil {
		return invalid("that feature is not running")
	}

	now := n.now()
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning waive: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET blocked = 0 WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("releasing %s: %w", taskID, err)
	}
	body := "Released by the operator without the work this card was planned to depend on: " + note
	if err := n.queueChild(ctx, tx, task.ProjectID, taskID, body, task.Lane,
		priorityOr(task.Priority), run.HeadSHA, now); err != nil {
		return err
	}
	return tx.Commit()
}

// priorityOr keeps the queue's default in one place: messages.priority is what
// claims order by, and zero there would sort ahead of everything.
func priorityOr(p int) int {
	if p == 0 {
		return 50
	}
	return p
}

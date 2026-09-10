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
	n.integrate.Lock()
	defer n.integrate.Unlock()
	rev, err := n.db.GetPlan(ctx, id)
	if err != nil {
		return "", err
	}
	if rev.State != store.PlanPending {
		return "", invalid("that plan is not waiting for a decision")
	}
	if err := n.db.FeatureCanPlan(ctx, rev.FeatureID); err != nil {
		return "", err
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

	if err := n.db.FeatureCanPlan(ctx, feature.ID); err != nil {
		return "", err
	}
	var grouped int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE parent_id = ?`, feature.ID).Scan(&grouped); err != nil {
		return "", err
	}
	if grouped != 0 {
		return "", invalid("this feature has manually grouped cards; detach them before accepting a split so its scope is exactly the approved plan")
	}
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
	if err := store.RecordFeatureEvent(ctx, tx, feature, store.OperatorRole, "Plan accepted; subtasks created", base); err != nil {
		return "", err
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

// integrateChild consumes an approval already claimed as integrating. The
// caller holds n.integrate across git and SQLite: interleaved merges once left
// the row behind the branch and shipped a feature missing a finished subtask.
// True means git may have changed; a recording failure must then keep the claim
// for recovery, not let a rejection pretend that merge never happened.
func (n *Nydus) integrateChild(ctx context.Context, a *store.Approval) (bool, error) {
	task, err := n.db.GetTaskIn(ctx, a.ProjectID, a.TaskID)
	if err != nil {
		return false, err
	}
	if a.Commit == "" || task.ParentID != a.FeatureID {
		return false, invalid("the integration no longer matches this card's feature")
	}
	if task.State != store.TaskQueued && task.State != store.TaskWorking {
		return false, invalid("that card is already closed; nothing more is integrated for it")
	}
	run, err := n.liveRun(ctx, a.FeatureID)
	if err != nil {
		return false, err
	}
	if run == nil {
		return false, invalid("that feature is no longer running")
	}
	head := a.Commit
	if n.integrator != nil {
		project, err := n.db.GetProject(ctx, a.ProjectID)
		if err != nil {
			return false, err
		}
		tree := hatchery.New(project.Path).Path(FeatureWorktree(a.FeatureID))
		if err := n.integrator.MergeInto(ctx, tree, a.Commit); err != nil {
			// An index lock is not a merge conflict. Nobody works in this tree,
			// so a real conflict must be aborted or it poisons every later merge.
			if busyRepo(err) {
				return false, fmt.Errorf("the feature's repository is busy; try again: %w", err)
			}
			_ = n.integrator.AbortMerge(ctx, tree)
			if err := n.db.SetFeatureRunState(ctx, a.FeatureID, store.FeatureConflict); err != nil {
				return false, err
			}
			return false, invalid("%s does not merge into the feature: %v. Reject this handoff with a note to merge %s and resolve it there before sending again", short(a.Commit), err, run.HeadSHA)
		}
		head, err = n.integrator.Resolve(ctx, tree, "HEAD")
		if err != nil {
			return true, fmt.Errorf("reading the integrated head: %w", err)
		}
		if head == "" {
			return true, fmt.Errorf("git returned no integrated head")
		}
	}

	now := n.now()
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	if err := ensureOpen(ctx, tx, a.ProjectID, task.ID); err != nil {
		return true, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET state = ?, decided_at = ? WHERE id = ? AND state = ?`,
		store.ApprovalApproved, now.Format(time.RFC3339Nano), a.ID, store.ApprovalIntegrating)
	if err != nil {
		return true, err
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return true, fmt.Errorf("the integration approval is no longer claimed: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET lane = ?, state = ?, completed_at = ? WHERE id = ?`,
		store.LaneDone, store.TaskDone, now.Format(time.RFC3339Nano), task.ID); err != nil {
		return true, err
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE feature_runs SET head_sha = ?, state = ? WHERE feature_id = ? AND head_sha = ?`,
		head, store.FeatureRunning, a.FeatureID, run.HeadSHA)
	if err != nil {
		return true, err
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return true, fmt.Errorf("the feature moved during integration: %v", err)
	}
	if err := n.releaseDependents(ctx, tx, a.ProjectID, task.ID, head, now); err != nil {
		return true, err
	}
	return true, tx.Commit()
}

// releaseDependents queues the cards whose every prerequisite is now in the
// feature head.
//
// The dependency join is a LEFT JOIN, and a missing card counts as unfinished.
// `feature_plan_items.child_task_id` is SET NULL when a card is deleted, so an
// inner join dropped the deleted prerequisite from the row set and read its
// absence as satisfaction: deleting one card of a plan silently released
// everything waiting on it, with neither its work nor a waiver saying why.
func (n *Nydus) releaseDependents(ctx context.Context, tx *sql.Tx, projectID, finishedID, head string, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT item.child_task_id, t.name, t.body, t.priority, t.lane
		   FROM feature_plan_items finished
		   JOIN feature_plan_deps d ON d.from_item = finished.id
		   JOIN feature_plan_items item ON item.id = d.to_item
		   JOIN tasks t ON t.id = item.child_task_id
		  WHERE finished.child_task_id = ? AND t.blocked = 1 AND t.state = 'queued'
		    AND NOT EXISTS (
		        SELECT 1 FROM feature_plan_deps d2
		          JOIN feature_plan_items p2 ON p2.id = d2.from_item
		          LEFT JOIN tasks t2 ON t2.id = p2.child_task_id
		         WHERE d2.to_item = item.id AND (t2.id IS NULL OR t2.state <> ? OR t2.parent_id IS NULL OR t2.parent_id <> t.parent_id)
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
	n.integrate.Lock()
	defer n.integrate.Unlock()
	prose, err := n.resolveEvidence(ctx, scope, prose)
	if err != nil {
		return nil, err
	}
	return n.db.SubmitPlan(ctx, scope.ProjectID, featureID, items, prose)
}

// SubmitFeatureReview records the architect's verdict, with --commit resolved
// in that role's worktree the way an approval's evidence is.
//
// head is not resolved: it is the sha the review envelope handed out, and the
// architect's worktree is not the feature's, so a ref read there would name a
// different commit. It is compared, not translated.
func (n *Nydus) SubmitFeatureReview(ctx context.Context, scope store.DecisionScope, featureID, head, verdict, note, evidence string) (*store.FeatureReview, error) {
	n.integrate.Lock()
	defer n.integrate.Unlock()
	evidence, err := n.resolveEvidence(ctx, scope, evidence)
	if err != nil {
		return nil, err
	}
	return n.db.SubmitReviewBy(ctx, scope, featureID, head, verdict, note, evidence)
}

// LandFeature merges the reviewed head onto base. The architect cannot call
// this: ok on a review is a recommendation, not a merge.
func (n *Nydus) LandFeature(ctx context.Context, featureID, head string) error {
	n.integrate.Lock()
	defer n.integrate.Unlock()
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
	if head == "" || head != run.HeadSHA {
		return invalid("the feature changed; read its current head before landing")
	}
	review, err := n.db.CurrentReview(ctx, featureID)
	if err != nil {
		return err
	}
	if review == nil || review.Verdict != store.ReviewOK || review.HeadSHA != run.HeadSHA {
		return invalid("this feature has no current review of this head; the architect has to look at it first")
	}
	ready, err := n.db.FeatureReady(ctx, featureID)
	if err != nil {
		return err
	}
	if !ready {
		return invalid("this feature still has unfinished or missing planned work")
	}

	_, outcome, ref, err := n.landApproved(ctx, feat.ProjectID, featureID, run.HeadSHA, review.Note)
	if err != nil {
		return err
	}
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := n.now().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET lane = ?, state = ?, completed_at = ?, outcome = ?, outcome_ref = ? WHERE id = ?`,
		store.LaneDone, store.TaskDone, now, outcome, ref, featureID); err != nil {
		return fmt.Errorf("closing the feature: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feature_runs SET state = ? WHERE feature_id = ?`, store.FeatureDone, featureID); err != nil {
		return err
	}
	if err := store.RecordFeatureEvent(ctx, tx, feat, store.OperatorRole, "Feature approved by the operator: "+outcome, run.HeadSHA); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
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

// RefreshFeature folds current base into the integration checkout and records
// the new head. A changed head requires a new review; merging by hand without
// recording it stranded the operator behind the old reviewed SHA.
func (n *Nydus) RefreshFeature(ctx context.Context, featureID, head string) error {
	n.integrate.Lock()
	defer n.integrate.Unlock()
	feat, err := n.db.GetTask(ctx, featureID)
	if err != nil {
		return err
	}
	run, err := n.liveRun(ctx, featureID)
	if err != nil {
		return err
	}
	if run == nil || head == "" || head != run.HeadSHA {
		return invalid("read the current head of a running feature before refreshing it")
	}
	if err := n.featureSettled(ctx, featureID); err != nil {
		return err
	}
	if n.integrator == nil {
		return invalid("this build cannot refresh a feature branch")
	}
	project, err := n.db.GetProject(ctx, feat.ProjectID)
	if err != nil {
		return err
	}
	base, err := n.integrator.Resolve(ctx, project.Path, project.BaseBranch)
	if err != nil {
		return err
	}
	tree := hatchery.New(project.Path).Path(FeatureWorktree(featureID))
	if err := n.integrator.MergeInto(ctx, tree, base); err != nil {
		if busyRepo(err) {
			return fmt.Errorf("the feature repository is busy; retry the refresh: %w", err)
		}
		_ = n.integrator.AbortMerge(ctx, tree)
		return invalid("refresh from %s conflicted: %v. Send the feature back and retry a card with instructions to merge %s and resolve the conflict in its worktree", project.BaseBranch, err, base)
	}
	updated, err := n.integrator.Resolve(ctx, tree, "HEAD")
	if err != nil {
		return err
	}
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE feature_runs SET head_sha = ?, base_sha = ? WHERE feature_id = ?`, updated, base, featureID); err != nil {
		return err
	}
	if err := store.RecordFeatureEvent(ctx, tx, feat, store.OperatorRole, "Refreshed from "+project.BaseBranch+" at "+base, updated); err != nil {
		return err
	}
	return tx.Commit()
}

func (n *Nydus) RejectFeature(ctx context.Context, featureID, head, note string) error {
	n.integrate.Lock()
	defer n.integrate.Unlock()
	_, err := n.db.RejectFeatureReview(ctx, featureID, head, note)
	return err
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
	n.integrate.Lock()
	defer n.integrate.Unlock()
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
	if run != nil && run.State == store.FeatureCancelled {
		return nil
	}
	if err := n.featureSettled(ctx, featureID); err != nil {
		return err
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
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.RecordFeatureEvent(ctx, tx, feat, store.OperatorRole, "Feature cancelled; work remains on its branch", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if run != nil {
		n.retireFeatureTree(ctx, feat.ProjectID, featureID)
	}
	return nil
}

// A claimed integration may already be in git even if its SQLite write failed.
// Cancellation or refresh must not erase that recovery boundary.
func (n *Nydus) featureSettled(ctx context.Context, featureID string) error {
	var pending int
	if err := n.db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE feature_id = ? AND state = ?`, featureID, store.ApprovalIntegrating).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return invalid("a subtask integration has not finished recording; restart the daemon to recover it before changing the feature")
	}
	return nil
}

// RetryChild puts a subtask back in front of the pipeline, starting from the
// feature head as it stands now.
//
// One of decision 10's named actions, and the one the refusal to review a
// feature with a failed subtask tells the reader to take. From the first role
// rather than the lane it died in: the card is being attempted again, not
// handed on, and the role that produced the work is the one that fixes it.
//
// A finished card is retried too, and only then: a rejection is answered by
// moving the head so the feature earns a fresh review, and a feature reaches
// review only once every child is done. Taking rejected cards alone left an
// architect's reject with no action at all — not the retry the panel offered,
// and not the one this project's own description named as the answer to it.
func (n *Nydus) RetryChild(ctx context.Context, taskID string) error {
	n.integrate.Lock()
	defer n.integrate.Unlock()
	task, err := n.db.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.ParentID == "" {
		return invalid("that card is not part of a feature")
	}
	body := "Retried by the operator. The feature head is where this starts from; " +
		"read the trail above for what failed."
	switch task.State {
	case store.TaskRejected:
	case store.TaskDone:
		review, err := n.db.CurrentReview(ctx, task.ParentID)
		if err != nil {
			return err
		}
		if review == nil || review.Verdict != store.ReviewReject {
			return invalid("that card is finished; a finished card is retried to answer a rejected " +
				"feature, and this feature has not been rejected")
		}
		// The rejection travels with the card. Its own trail says it finished,
		// so without this the role picks up work it already did and is told
		// nothing about why it is doing it again.
		body = "Retried by the operator to answer the rejection of this feature: " +
			review.Note + "\n\nThe feature head is where this starts from."
	default:
		return invalid("that card has not failed; only a stopped, rejected or finished card is retried")
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

	// Retrying a stopped blocked card is not an implicit dependency waiver.
	blocked := false
	if task.Blocked {
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM feature_plan_items i
			 JOIN feature_plan_deps d ON d.to_item = i.id
			 JOIN feature_plan_items p ON p.id = d.from_item
			 LEFT JOIN tasks c ON c.id = p.child_task_id
			 WHERE i.child_task_id = ? AND (c.id IS NULL OR c.state <> 'done' OR c.parent_id IS NULL OR c.parent_id <> ?))`, taskID, task.ParentID).Scan(&blocked); err != nil {
			return err
		}
	}
	// rework_count too: decision 10 says a stall reuses the threshold that
	// already surfaces a card going backward too often, and only agent-driven
	// handoffs were counted. An operator retrying the same card ten times is
	// the same card looping, and should reach the same list.
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET state = ?, lane = ?, blocked = ?, stopped_at = NULL,
		   completed_at = NULL, rework_count = rework_count + 1
		  WHERE id = ?`, store.TaskQueued, first.Name, blocked, taskID); err != nil {
		return fmt.Errorf("requeueing %s: %w", taskID, err)
	}
	if !blocked {
		if err := n.queueChild(ctx, tx, task.ProjectID, taskID, body, first.Name,
			priorityOr(task.Priority), run.HeadSHA, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WaiveDependency releases a blocked subtask whose dependency is not coming.
//
// The rationale is carried on the message the card is claimed with, because the
// role that picks it up is the one that needs to know it is starting without
// something the plan said it would have.
func (n *Nydus) WaiveDependency(ctx context.Context, taskID, note string) error {
	n.integrate.Lock()
	defer n.integrate.Unlock()
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

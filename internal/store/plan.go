package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Plan states. A rejected plan produces a new revision rather than an edit.
const (
	PlanPending  = "pending"
	PlanApproved = "approved"
	PlanRejected = "rejected"
)

// PlanDraft is one subtask as the architect submitted it. After names
// dependencies in this same plan; they are resolved to ids at write.
type PlanDraft struct {
	Name     string   `json:"name"`
	Body     string   `json:"body"`
	Priority int      `json:"priority"`
	After    []string `json:"after"`
}

// PlanItem is a subtask as stored. ChildTaskID stays empty until materialisation.
type PlanItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Body        string   `json:"body"`
	Priority    int      `json:"priority"`
	Position    int      `json:"position"`
	After       []string `json:"after,omitempty"`
	ChildTaskID string   `json:"childTaskId,omitempty"`
}

// FeatureRun is the live integration of one feature: the branch work lands on
// until the whole is reviewed.
type FeatureRun struct {
	FeatureID string `json:"featureId"`
	Branch    string `json:"branch"`
	BaseSHA   string `json:"baseSha"`
	HeadSHA   string `json:"headSha"`
	State     string `json:"state"`
	// Name is the feature's, filled in by the queries that have to name it in
	// a sentence a person reads. Empty from GetFeatureRun.
	Name string `json:"name,omitempty"`
}

// FeatureStall is a feature that has stopped needing an agent and started
// needing a person: nothing in it will move on its own.
//
// Decision 10 gave the operator named actions; without a list of what is
// stalled they were unreachable, and a feature whose subtask failed showed
// nowhere at all — not in the review gates, which need a review, and not on
// the architect's queue, which skips a feature with a failed child.
type FeatureStall struct {
	FeatureID string      `json:"featureId"`
	Name      string      `json:"name"`
	Reason    string      `json:"reason"`
	Note      string      `json:"note,omitempty"`
	HeadSHA   string      `json:"headSha,omitempty"`
	Cards     []StallCard `json:"cards,omitempty"`
}

// StallCard is one card in a stalled feature and what can be done about it.
type StallCard struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Action string `json:"action"`
}

// Why a feature is stalled, and what a person can do about each.
const (
	StallConflict = "conflict" // integration stopped on a conflict
	StallFailed   = "failed"   // a subtask was stopped or rejected
	StallBlocked  = "blocked"  // everything left is waiting on work that is not coming
	StallRejected = "rejected" // the architect sent the whole thing back

	ActionRetry = "retry"
	ActionWaive = "waive"
)

const (
	FeatureRunning   = "running"
	FeatureDone      = "done"
	FeatureCancelled = "cancelled"
	FeatureConflict  = "conflict"

	ReviewOK     = "ok"
	ReviewReject = "reject"
)

// FeatureReview is the architect's verdict about one head. ok is a
// recommendation; only the operator lands.
type FeatureReview struct {
	ID          string    `json:"id"`
	FeatureID   string    `json:"featureId"`
	FeatureName string    `json:"featureName,omitempty"`
	FeatureBody string    `json:"featureBody,omitempty"`
	HeadSHA     string    `json:"headSha"`
	Verdict     string    `json:"verdict"`
	Note        string    `json:"note,omitempty"`
	EvidenceSHA string    `json:"evidenceSha,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// PlanRevision is one immutable split of a feature.
type PlanRevision struct {
	ID              string     `json:"id"`
	FeatureID       string     `json:"featureId"`
	FeatureName     string     `json:"featureName,omitempty"`
	FeatureBody     string     `json:"featureBody,omitempty"`
	N               int        `json:"n"`
	Digest          string     `json:"digest"`
	ProseSHA        string     `json:"proseSha,omitempty"`
	State           string     `json:"state"`
	ItemCount       int        `json:"itemCount"`
	EstimateTokens  int64      `json:"estimateTokens"`
	EstimateCostUSD float64    `json:"estimateCostUsd"`
	Note            string     `json:"note,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	DecidedAt       *time.Time `json:"decidedAt,omitempty"`
	DecidedBy       string     `json:"decidedBy,omitempty"`
	Items           []PlanItem `json:"items,omitempty"`
}

// SubmitPlan writes a pending revision. Nothing is queued: this is the
// expensive-decision surface, and creating the children waits on the operator.
func (db *DB) SubmitPlan(ctx context.Context, projectID, featureID string, drafts []PlanDraft, proseSHA string) (*PlanRevision, error) {
	feature, err := db.GetTaskIn(ctx, projectID, featureID)
	if err != nil {
		return nil, err
	}
	if feature.Kind != TaskKindFeature {
		return nil, invalid("split is for a feature, not a card")
	}
	if len(drafts) == 0 {
		return nil, invalid("a plan needs at least one subtask")
	}

	items, edges, err := validatePlan(drafts)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		existing, err := db.GetTaskByName(ctx, projectID, it.Name)
		if err == nil {
			return nil, invalid("a card named %q already exists; the plan cannot use that name", existing.Name)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}

	estTokens, estCost, err := db.planEstimate(ctx, projectID, len(items))
	if err != nil {
		return nil, err
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning plan: %w", err)
	}
	defer tx.Rollback()

	var latestState string
	var latestN int
	err = tx.QueryRowContext(ctx,
		`SELECT n, state FROM feature_plan_revisions WHERE feature_id = ? ORDER BY n DESC LIMIT 1`,
		featureID).Scan(&latestN, &latestState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reading the current plan: %w", err)
	}
	if err == nil {
		switch latestState {
		case PlanPending:
			return nil, invalid("this feature already has a plan waiting for approval")
		case PlanApproved:
			return nil, invalid("this plan was already accepted")
		}
	}

	now := time.Now().UTC()
	rev := &PlanRevision{
		ID:              NewID(),
		FeatureID:       featureID,
		FeatureName:     feature.Name,
		FeatureBody:     feature.Body,
		N:               latestN + 1,
		ProseSHA:        proseSHA,
		State:           PlanPending,
		ItemCount:       len(items),
		EstimateTokens:  estTokens,
		EstimateCostUSD: estCost,
		CreatedAt:       now,
		Items:           items,
	}
	rev.Digest = planDigest(items, edges)

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_plan_revisions
		   (id, feature_id, n, digest, prose_sha, state, item_count,
		    estimate_tokens, estimate_cost_usd, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		rev.ID, rev.FeatureID, rev.N, rev.Digest, rev.ProseSHA, rev.State, rev.ItemCount,
		rev.EstimateTokens, rev.EstimateCostUSD, rev.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("writing the plan: %w", err)
	}

	idsByName := make(map[string]string, len(items))
	for i := range items {
		items[i].ID = NewID()
		idsByName[items[i].Name] = items[i].ID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO feature_plan_items (id, revision_id, position, name, body, priority)
			 VALUES (?,?,?,?,?,?)`,
			items[i].ID, rev.ID, items[i].Position, items[i].Name, items[i].Body, items[i].Priority); err != nil {
			return nil, fmt.Errorf("writing plan item %q: %w", items[i].Name, err)
		}
	}
	for from, tos := range edges {
		fromID := idsByName[from]
		for _, to := range tos {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO feature_plan_deps (revision_id, from_item, to_item) VALUES (?,?,?)`,
				rev.ID, fromID, idsByName[to]); err != nil {
				return nil, fmt.Errorf("writing plan dependency %s → %s: %w", from, to, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing the plan: %w", err)
	}
	rev.Items = items
	return rev, nil
}

// RejectPlan sends a pending revision back. Rejecting needs a note; the next
// split produces a new revision rather than editing this one. The project id is
// returned so the caller can summon or retire the sidecar.
//
// Rejecting only. Accepting is nydus.AcceptPlan, because approval is not a row
// change: it creates the branch, the worktree and every child card in one
// transaction. A second path that only flipped the state left a feature with an
// approved plan, nothing queued, and no way back — the architect is not offered
// a feature whose plan was accepted.
func (db *DB) RejectPlan(ctx context.Context, id, note, by string) (string, error) {
	if strings.TrimSpace(note) == "" {
		return "", invalid("rejecting a plan needs a note: what to change")
	}
	if by == "" {
		by = OperatorRole
	}
	var projectID string
	err := db.read.QueryRowContext(ctx,
		`SELECT t.project_id FROM feature_plan_revisions r JOIN tasks t ON t.id = r.feature_id WHERE r.id = ?`,
		id).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalid("that plan is not waiting for a decision")
	}
	if err != nil {
		return "", fmt.Errorf("reading plan %s: %w", id, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := db.sql.ExecContext(ctx,
		`UPDATE feature_plan_revisions
		    SET state = ?, note = ?, decided_at = ?, decided_by = ?
		  WHERE id = ? AND state = ?`,
		PlanRejected, note, now, by, id, PlanPending)
	if err != nil {
		return "", fmt.Errorf("deciding plan %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", invalid("that plan is not waiting for a decision")
	}
	return projectID, nil
}

// ListPendingPlans is what Attention shows: splits waiting on the operator,
// with the items so the count is not the only thing they can read.
func (db *DB) ListPendingPlans(ctx context.Context, projectID string) ([]PlanRevision, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT r.id, r.feature_id, t.name, t.body, r.n, r.digest, r.prose_sha, r.state,
		        r.item_count, r.estimate_tokens, r.estimate_cost_usd, r.note, r.created_at
		   FROM feature_plan_revisions r
		   JOIN tasks t ON t.id = r.feature_id
		  WHERE t.project_id = ? AND r.state = ?
		  ORDER BY r.created_at`, projectID, PlanPending)
	if err != nil {
		return nil, fmt.Errorf("listing plans: %w", err)
	}
	defer rows.Close()

	var out []PlanRevision
	for rows.Next() {
		var r PlanRevision
		var created string
		if err := rows.Scan(&r.ID, &r.FeatureID, &r.FeatureName, &r.FeatureBody, &r.N,
			&r.Digest, &r.ProseSHA, &r.State, &r.ItemCount, &r.EstimateTokens, &r.EstimateCostUSD,
			&r.Note, &created); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = parseStored(created); err != nil {
			return nil, fmt.Errorf("plan %s has an unreadable created_at: %w", r.ID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	ids := make([]string, 0, len(out))
	for _, r := range out {
		ids = append(ids, r.ID)
	}
	items, deps, err := db.planParts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		list := items[out[i].ID]
		for j := range list {
			list[j].After = deps[list[j].ID]
		}
		out[i].Items = list
	}
	return out, nil
}

// NextFeatureToPlan is the oldest feature that still needs a split: none yet,
// or the latest was rejected. A pending or accepted plan is not the architect's
// to rewrite.
func (db *DB) NextFeatureToPlan(ctx context.Context, projectID string) (*Task, string, error) {
	var id string
	err := db.read.QueryRowContext(ctx,
		`SELECT t.id FROM tasks t
		  WHERE t.project_id = ? AND t.kind = ?
		    AND NOT EXISTS (
		        SELECT 1 FROM feature_plan_revisions r
		         WHERE r.feature_id = t.id AND r.state IN (?, ?)
		    )
		  ORDER BY t.created_at LIMIT 1`,
		projectID, TaskKindFeature, PlanPending, PlanApproved).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("looking for a feature to plan: %w", err)
	}
	t, err := db.GetTask(ctx, id)
	if err != nil {
		return nil, "", err
	}
	var note string
	err = db.read.QueryRowContext(ctx,
		`SELECT note FROM feature_plan_revisions
		  WHERE feature_id = ? AND state = ? ORDER BY n DESC LIMIT 1`,
		id, PlanRejected).Scan(&note)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("reading the last rejection: %w", err)
	}
	return t, note, nil
}

// HasWorkForSupervisor is whether the architect sidecar has anything to do:
// a live supervised card, a feature that still needs a plan, or a finished
// feature that still needs a review of its current head.
func (db *DB) HasWorkForSupervisor(ctx context.Context, projectID string) (bool, error) {
	want, err := db.HasOpenSupervised(ctx, projectID)
	if want || err != nil {
		return want, err
	}
	var n int
	err = db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks t
		  WHERE t.project_id = ? AND t.kind = ?
		    AND NOT EXISTS (
		        SELECT 1 FROM feature_plan_revisions r
		         WHERE r.feature_id = t.id AND r.state IN (?, ?)
		    )`,
		projectID, TaskKindFeature, PlanPending, PlanApproved).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("looking for features to plan: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	feat, _, err := db.NextFeatureToReview(ctx, projectID)
	return feat != nil, err
}

func (db *DB) planParts(ctx context.Context, revisionIDs []string) (map[string][]PlanItem, map[string][]string, error) {
	placeholders := strings.Repeat("?,", len(revisionIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(revisionIDs))
	for i, id := range revisionIDs {
		args[i] = id
	}

	rows, err := db.read.QueryContext(ctx,
		`SELECT id, revision_id, position, name, body, priority, COALESCE(child_task_id, '')
		   FROM feature_plan_items WHERE revision_id IN (`+placeholders+`)
		   ORDER BY position`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("reading plan items: %w", err)
	}
	defer rows.Close()

	items := map[string][]PlanItem{}
	for rows.Next() {
		var it PlanItem
		var revID string
		if err := rows.Scan(&it.ID, &revID, &it.Position, &it.Name, &it.Body, &it.Priority, &it.ChildTaskID); err != nil {
			return nil, nil, err
		}
		items[revID] = append(items[revID], it)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	drows, err := db.read.QueryContext(ctx,
		`SELECT d.revision_id, d.to_item, i.name
		   FROM feature_plan_deps d
		   JOIN feature_plan_items i ON i.id = d.from_item
		  WHERE d.revision_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("reading plan deps: %w", err)
	}
	defer drows.Close()

	// after is stored on the dependent: from_item is the prerequisite.
	after := map[string][]string{}
	for drows.Next() {
		var revID, toID, fromName string
		if err := drows.Scan(&revID, &toID, &fromName); err != nil {
			return nil, nil, err
		}
		after[toID] = append(after[toID], fromName)
	}
	return items, after, drows.Err()
}

func (db *DB) planEstimate(ctx context.Context, projectID string, n int) (int64, float64, error) {
	var avgTokens sql.NullFloat64
	var avgCost sql.NullFloat64
	err := db.read.QueryRowContext(ctx,
		`SELECT AVG(tokens), AVG(cost) FROM (
		     SELECT SUM(input_tokens + cache_read_tokens + cache_write_tokens + output_tokens) AS tokens,
		            SUM(cost_usd) AS cost
		       FROM usage_turns u
		       JOIN tasks t ON t.id = u.task_id
		      WHERE u.project_id = ? AND t.kind = ? AND t.state = ?
		      GROUP BY u.task_id
		 )`, projectID, TaskKindWork, TaskDone).Scan(&avgTokens, &avgCost)
	if err != nil {
		return 0, 0, fmt.Errorf("estimating the plan: %w", err)
	}
	if !avgTokens.Valid {
		return 0, 0, nil
	}
	return int64(avgTokens.Float64 * float64(n)), avgCost.Float64 * float64(n), nil
}

func validatePlan(drafts []PlanDraft) ([]PlanItem, map[string][]string, error) {
	items := make([]PlanItem, 0, len(drafts))
	seen := map[string]bool{}
	edges := map[string][]string{} // prerequisite name -> dependents

	for i, d := range drafts {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			return nil, nil, invalid("every subtask needs a name")
		}
		if seen[name] {
			return nil, nil, invalid("two subtasks are both named %q", name)
		}
		seen[name] = true
		priority := d.Priority
		if priority == 0 {
			priority = 50
		}
		items = append(items, PlanItem{
			Name: name, Body: d.Body, Priority: priority, Position: i + 1, After: d.After,
		})
	}
	for _, it := range items {
		for _, after := range it.After {
			after = strings.TrimSpace(after)
			if after == "" {
				continue
			}
			if after == it.Name {
				return nil, nil, invalid("%q cannot depend on itself", it.Name)
			}
			if !seen[after] {
				return nil, nil, invalid("%q depends on %q, which is not in this plan", it.Name, after)
			}
			edges[after] = append(edges[after], it.Name)
		}
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
	}
	if planCycle(names, edges) {
		return nil, nil, invalid("this plan has a dependency cycle; nothing in it could start")
	}
	return items, edges, nil
}

func planCycle(names []string, edges map[string][]string) bool {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = grey
		for _, m := range edges[n] {
			if color[m] == grey {
				return true
			}
			if color[m] == white && visit(m) {
				return true
			}
		}
		color[n] = black
		return false
	}
	for _, n := range names {
		if color[n] == white && visit(n) {
			return true
		}
	}
	return false
}

// planDigest is the binding between the rows and the prose. Items by position,
// deps sorted, so the same plan always hashes the same.
func planDigest(items []PlanItem, edges map[string][]string) string {
	type dItem struct {
		Name     string `json:"name"`
		Body     string `json:"body"`
		Priority int    `json:"priority"`
		Position int    `json:"position"`
	}
	type dDep struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	body := struct {
		Items []dItem `json:"items"`
		Deps  []dDep  `json:"deps"`
	}{}
	for _, it := range items {
		body.Items = append(body.Items, dItem{it.Name, it.Body, it.Priority, it.Position})
	}
	for from, tos := range edges {
		for _, to := range tos {
			body.Deps = append(body.Deps, dDep{from, to})
		}
	}
	sort.Slice(body.Deps, func(i, j int) bool {
		if body.Deps[i].From != body.Deps[j].From {
			return body.Deps[i].From < body.Deps[j].From
		}
		return body.Deps[i].To < body.Deps[j].To
	})
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// GetPlan loads one revision and its items.
func (db *DB) GetPlan(ctx context.Context, id string) (*PlanRevision, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT r.id, r.feature_id, t.name, t.body, r.n, r.digest, r.prose_sha, r.state,
		        r.item_count, r.estimate_tokens, r.estimate_cost_usd, r.note, r.created_at
		   FROM feature_plan_revisions r
		   JOIN tasks t ON t.id = r.feature_id
		  WHERE r.id = ?`, id)
	var r PlanRevision
	var created string
	if err := row.Scan(&r.ID, &r.FeatureID, &r.FeatureName, &r.FeatureBody, &r.N,
		&r.Digest, &r.ProseSHA, &r.State, &r.ItemCount, &r.EstimateTokens, &r.EstimateCostUSD,
		&r.Note, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("plan %s: %w", id, ErrNotFound)
		}
		return nil, err
	}
	var err error
	if r.CreatedAt, err = parseStored(created); err != nil {
		return nil, fmt.Errorf("plan %s has an unreadable created_at: %w", r.ID, err)
	}
	items, deps, err := db.planParts(ctx, []string{r.ID})
	if err != nil {
		return nil, err
	}
	list := items[r.ID]
	for i := range list {
		list[i].After = deps[list[i].ID]
	}
	r.Items = list
	return &r, nil
}

// GetFeatureRun returns the live integration row, or nil if none.
func (db *DB) GetFeatureRun(ctx context.Context, featureID string) (*FeatureRun, error) {
	var r FeatureRun
	err := db.read.QueryRowContext(ctx,
		`SELECT feature_id, branch, base_sha, head_sha, state FROM feature_runs WHERE feature_id = ?`,
		featureID).Scan(&r.FeatureID, &r.Branch, &r.BaseSHA, &r.HeadSHA, &r.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading feature run %s: %w", featureID, err)
	}
	return &r, nil
}

// HasLiveChildren reports whether a feature still has cards queued or being worked.
func (db *DB) HasLiveChildren(ctx context.Context, featureID string) (bool, error) {
	var n int
	err := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE parent_id = ? AND state IN (?, ?)`,
		featureID, TaskQueued, TaskWorking).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("counting children of %s: %w", featureID, err)
	}
	return n > 0, nil
}

// NextFeatureToReview is a running feature whose children are all done and
// whose current head has not been reviewed. A rejected child stalls it.
func (db *DB) NextFeatureToReview(ctx context.Context, projectID string) (*Task, string, error) {
	var id, head string
	err := db.read.QueryRowContext(ctx,
		`SELECT t.id, r.head_sha FROM tasks t
		   JOIN feature_runs r ON r.feature_id = t.id
		  WHERE t.project_id = ? AND t.kind = ? AND r.state = ?
		    AND EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = t.id)
		    AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = t.id AND c.state IN (?, ?))
		    AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = t.id AND c.state = ?)
		    AND NOT EXISTS (
		        SELECT 1 FROM feature_reviews v
		         WHERE v.feature_id = t.id AND v.head_sha = r.head_sha
		    )
		  ORDER BY t.created_at LIMIT 1`,
		projectID, TaskKindFeature, FeatureRunning, TaskQueued, TaskWorking, TaskRejected).Scan(&id, &head)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("looking for a feature to review: %w", err)
	}
	t, err := db.GetTask(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return t, head, nil
}

// SubmitReview records the architect's verdict about the current feature head.
func (db *DB) SubmitReview(ctx context.Context, featureID, verdict, note, evidence string) (*FeatureReview, error) {
	if verdict != ReviewOK && verdict != ReviewReject {
		return nil, invalid("a review is ok or reject")
	}
	if verdict == ReviewReject && strings.TrimSpace(note) == "" {
		return nil, invalid("rejecting a feature needs a note: what to change")
	}
	run, err := db.GetFeatureRun(ctx, featureID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.State != FeatureRunning {
		return nil, invalid("that feature is not running")
	}
	live, err := db.HasLiveChildren(ctx, featureID)
	if err != nil {
		return nil, err
	}
	if live {
		return nil, invalid("this feature still has cards being worked")
	}
	var rejected int
	if err := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE parent_id = ? AND state = ?`,
		featureID, TaskRejected).Scan(&rejected); err != nil {
		return nil, err
	}
	if rejected > 0 {
		return nil, invalid("this feature has a failed subtask; cancel it, or retry that card")
	}
	var n int
	if err := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feature_reviews WHERE feature_id = ? AND head_sha = ?`,
		featureID, run.HeadSHA).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, invalid("this head has already been reviewed")
	}
	feat, err := db.GetTask(ctx, featureID)
	if err != nil {
		return nil, err
	}
	rev := &FeatureReview{
		ID: NewID(), FeatureID: featureID, FeatureName: feat.Name, FeatureBody: feat.Body,
		HeadSHA: run.HeadSHA, Verdict: verdict, Note: note, EvidenceSHA: evidence,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO feature_reviews (id, feature_id, head_sha, verdict, note, evidence_sha, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		rev.ID, rev.FeatureID, rev.HeadSHA, rev.Verdict, rev.Note, rev.EvidenceSHA,
		rev.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("recording the review: %w", err)
	}
	return rev, nil
}

// ListFeatureGates is what Attention shows as a land: the architect looked at
// the head that is there now and recommended it. A rejection is not a gate —
// nothing is waiting to be clicked — so it reaches the operator through
// ListFeatureStalls with the actions that answer it.
func (db *DB) ListFeatureGates(ctx context.Context, projectID string) ([]FeatureReview, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT v.id, t.id, t.name, t.body, v.head_sha, v.verdict, v.note, v.evidence_sha, v.created_at
		   FROM feature_runs r
		   JOIN tasks t ON t.id = r.feature_id
		   JOIN feature_reviews v ON v.feature_id = t.id AND v.head_sha = r.head_sha
		  WHERE t.project_id = ? AND r.state = ? AND v.verdict = ?
		    AND v.created_at = (
		        SELECT MAX(v2.created_at) FROM feature_reviews v2
		         WHERE v2.feature_id = t.id AND v2.head_sha = r.head_sha
		    )
		    AND NOT EXISTS (
		        SELECT 1 FROM tasks c WHERE c.parent_id = t.id AND c.state IN (?, ?)
		    )
		  ORDER BY v.created_at`,
		projectID, FeatureRunning, ReviewOK, TaskQueued, TaskWorking)
	if err != nil {
		return nil, fmt.Errorf("listing feature gates: %w", err)
	}
	defer rows.Close()
	var out []FeatureReview
	for rows.Next() {
		var v FeatureReview
		var created string
		if err := rows.Scan(&v.ID, &v.FeatureID, &v.FeatureName, &v.FeatureBody,
			&v.HeadSHA, &v.Verdict, &v.Note, &v.EvidenceSHA, &created); err != nil {
			return nil, err
		}
		if v.CreatedAt, err = parseStored(created); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CurrentReview is the latest review of the live head, or nil.
func (db *DB) CurrentReview(ctx context.Context, featureID string) (*FeatureReview, error) {
	run, err := db.GetFeatureRun(ctx, featureID)
	if err != nil || run == nil {
		return nil, err
	}
	row := db.read.QueryRowContext(ctx,
		`SELECT id, feature_id, head_sha, verdict, note, evidence_sha, created_at
		   FROM feature_reviews WHERE feature_id = ? AND head_sha = ?
		   ORDER BY created_at DESC LIMIT 1`, featureID, run.HeadSHA)
	var v FeatureReview
	var created string
	if err := row.Scan(&v.ID, &v.FeatureID, &v.HeadSHA, &v.Verdict, &v.Note, &v.EvidenceSHA, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if v.CreatedAt, err = parseStored(created); err != nil {
		return nil, err
	}
	return &v, nil
}

// LiveFeatureRuns are the runs whose work has not landed, named, for the checks
// that must refuse to carry one somewhere it does not belong.
func (db *DB) LiveFeatureRuns(ctx context.Context, projectID string) ([]FeatureRun, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT r.feature_id, r.branch, r.base_sha, r.head_sha, r.state, t.name
		   FROM feature_runs r JOIN tasks t ON t.id = r.feature_id
		  WHERE t.project_id = ? AND r.state IN (?, ?)`,
		projectID, FeatureRunning, FeatureConflict)
	if err != nil {
		return nil, fmt.Errorf("listing feature runs: %w", err)
	}
	defer rows.Close()
	var out []FeatureRun
	for rows.Next() {
		var r FeatureRun
		if err := rows.Scan(&r.FeatureID, &r.Branch, &r.BaseSHA, &r.HeadSHA, &r.State, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RejectPendingPlan sends back whatever split is waiting on this feature, if
// any. Used when the feature itself goes away, so Attention stops asking about
// a decision that no longer means anything.
func (db *DB) RejectPendingPlan(ctx context.Context, featureID, note string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE feature_plan_revisions SET state = ?, note = ?, decided_at = ?, decided_by = ?
		  WHERE feature_id = ? AND state = ?`,
		PlanRejected, note, time.Now().UTC().Format(time.RFC3339Nano), OperatorRole,
		featureID, PlanPending)
	if err != nil {
		return fmt.Errorf("closing the plan for %s: %w", featureID, err)
	}
	return nil
}

// FeatureAccepts refuses a card being grouped under a feature that has nowhere
// to put it. A landed or cancelled run is finished, and a card joining one
// could never complete: its last handoff would be an integration into a branch
// that is closed.
func (db *DB) FeatureAccepts(ctx context.Context, featureID string) error {
	run, err := db.GetFeatureRun(ctx, featureID)
	if err != nil || run == nil {
		return err
	}
	switch run.State {
	case FeatureDone:
		return invalid("that feature has already landed; a card cannot join it now")
	case FeatureCancelled:
		return invalid("that feature was cancelled; a card cannot join it now")
	}
	return nil
}

// ListFeatureStalls is what Attention shows for a feature nothing will move on
// its own: an integration conflict, a failed subtask, a plan whose remaining
// cards all wait on work that is not coming, or an architect's rejection.
//
// A feature in any of these states appeared nowhere before: the review gates
// need a review of the current head, and the architect is not offered a feature
// with a failed child. The operator's only visible action was deleting the
// feature, which ungroups its cards and takes the plan with it.
func (db *DB) ListFeatureStalls(ctx context.Context, projectID string) ([]FeatureStall, error) {
	runs, err := db.LiveFeatureRuns(ctx, projectID)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	var out []FeatureStall
	for _, run := range runs {
		stall, err := db.stallOf(ctx, run)
		if err != nil {
			return nil, err
		}
		if stall != nil {
			out = append(out, *stall)
		}
	}
	return out, nil
}

func (db *DB) stallOf(ctx context.Context, run FeatureRun) (*FeatureStall, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id, name, state, blocked FROM tasks WHERE parent_id = ? ORDER BY created_at`,
		run.FeatureID)
	if err != nil {
		return nil, fmt.Errorf("reading the cards of %s: %w", run.FeatureID, err)
	}
	defer rows.Close()

	var failed, blocked []StallCard
	var moving int
	for rows.Next() {
		var c StallCard
		var state string
		var isBlocked int
		if err := rows.Scan(&c.ID, &c.Name, &state, &isBlocked); err != nil {
			return nil, err
		}
		switch {
		case state == TaskRejected:
			c.Action = ActionRetry
			failed = append(failed, c)
		case isBlocked != 0:
			c.Action = ActionWaive
			blocked = append(blocked, c)
		case state == TaskQueued || state == TaskWorking:
			moving++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	stall := &FeatureStall{
		FeatureID: run.FeatureID, Name: run.Name, HeadSHA: run.HeadSHA,
		Cards: append(append([]StallCard{}, failed...), blocked...),
	}
	switch {
	case run.State == FeatureConflict:
		stall.Reason = StallConflict
		stall.Note = "A subtask's work did not merge into the feature. " +
			"The card that hit it resolves the merge in its own worktree and sends again."
	case len(failed) > 0:
		stall.Reason = StallFailed
	case moving == 0 && len(blocked) > 0:
		// Every card left is waiting for work that nothing is going to do.
		stall.Reason = StallBlocked
	default:
		// Still moving, or finished and waiting on a review. Neither is a stall.
		review, err := db.CurrentReview(ctx, run.FeatureID)
		if err != nil || review == nil || review.Verdict != ReviewReject {
			return nil, err
		}
		stall.Reason = StallRejected
		stall.Note = review.Note
	}
	return stall, nil
}

func (db *DB) SetFeatureRunState(ctx context.Context, featureID, state string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE feature_runs SET state = ? WHERE feature_id = ?`, state, featureID)
	if err != nil {
		return fmt.Errorf("updating feature run %s: %w", featureID, err)
	}
	return mustAffect(res, fmt.Sprintf("feature %s", featureID))
}

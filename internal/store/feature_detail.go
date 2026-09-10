package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FeatureDetail is the durable unit a person reviews, including rejected plans
// and verdicts. Attention only lists what needs a decision right now.
type FeatureDetail struct {
	Task        *Task           `json:"task"`
	Run         *FeatureRun     `json:"run"`
	Plans       []PlanRevision  `json:"plans"`
	Reviews     []FeatureReview `json:"reviews"`
	Children    []Task          `json:"children"`
	History     []TrailStep     `json:"history"`
	Usage       UsageTotal      `json:"usage"`
	Integration string          `json:"integration"`
	BaseBranch  string          `json:"baseBranch"`
}

func (db *DB) FeatureDetail(ctx context.Context, id string) (*FeatureDetail, error) {
	task, err := db.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if task.Kind != TaskKindFeature {
		return nil, invalid("that is not a feature")
	}
	d := &FeatureDetail{Task: task, Plans: []PlanRevision{}, Reviews: []FeatureReview{}, Children: []Task{}}
	project, err := db.GetProject(ctx, task.ProjectID)
	if err != nil {
		return nil, err
	}
	d.Integration, d.BaseBranch = project.Integration, project.BaseBranch
	if d.Run, err = db.GetFeatureRun(ctx, id); err != nil {
		return nil, err
	}
	rows, err := db.read.QueryContext(ctx, `SELECT id FROM feature_plan_revisions WHERE feature_id = ? ORDER BY n`, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var revision string
		if err := rows.Scan(&revision); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, revision)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, revision := range ids {
		plan, err := db.GetPlan(ctx, revision)
		if err != nil {
			return nil, err
		}
		d.Plans = append(d.Plans, *plan)
	}
	rows, err = db.read.QueryContext(ctx, `SELECT id, feature_id, head_sha, verdict, note, evidence_sha, created_at, decided_by FROM feature_reviews WHERE feature_id = ? ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v FeatureReview
		var at string
		if err := rows.Scan(&v.ID, &v.FeatureID, &v.HeadSHA, &v.Verdict, &v.Note, &v.EvidenceSHA, &at, &v.DecidedBy); err != nil {
			rows.Close()
			return nil, err
		}
		if v.CreatedAt, err = parseStored(at); err != nil {
			rows.Close()
			return nil, err
		}
		d.Reviews = append(d.Reviews, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = db.read.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE parent_id = ? ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		child, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		d.Children = append(d.Children, *child)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if d.History, err = db.TaskTrail(ctx, id); err != nil {
		return nil, err
	}
	if d.Usage, err = db.UsageForTask(ctx, id); err != nil {
		return nil, err
	}
	return d, nil
}

func (db *DB) AcceptedPlan(ctx context.Context, featureID string) (*PlanRevision, error) {
	var id string
	if err := db.read.QueryRowContext(ctx, `SELECT id FROM feature_plan_revisions WHERE feature_id = ? AND state = 'approved' ORDER BY n DESC LIMIT 1`, featureID).Scan(&id); err != nil {
		return nil, err
	}
	return db.GetPlan(ctx, id)
}

// RecordFeatureEvent uses the existing trail without inventing a second event
// log. No route: a lifecycle entry must never turn a feature into claimable work.
func RecordFeatureEvent(ctx context.Context, tx *sql.Tx, feature *Task, by, body, commit string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, task_id, from_role, kind, body, commit_sha, created_at)
   VALUES (?,?,?,?,?,?,?,?)`, NewID(), feature.ProjectID, feature.ID, by, KindNote, body, commit, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("recording feature history: %w", err)
	}
	return nil
}

// TrackSidecarWork records ownership without giving a sidecar a pipeline lease.
// Repeated next calls on the same item preserve the original window.
func (db *DB) TrackSidecarWork(ctx context.Context, projectID, role, taskID string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM sidecar_work WHERE project_id = ? AND role = ? AND finished_at IS NULL`, projectID, role).Scan(&current)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if current == taskID {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE sidecar_work SET finished_at = ? WHERE project_id = ? AND role = ? AND finished_at IS NULL`, now, projectID, role); err != nil {
		return err
	}
	if taskID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sidecar_work (id, project_id, role, task_id, started_at) VALUES (?,?,?,?,?)`, NewID(), projectID, role, taskID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) sidecarTaskForAt(ctx context.Context, projectID, role string, at time.Time) (*string, error) {
	var id, start string
	var end sql.NullString
	query := `SELECT task_id, started_at, finished_at FROM sidecar_work WHERE project_id = ? AND role = ?`
	args := []any{projectID, role}
	if at.IsZero() {
		query += ` AND finished_at IS NULL`
	} else {
		query += ` AND started_at <= ?`
		args = append(args, at.UTC().Format(time.RFC3339Nano))
	}
	err := db.read.QueryRowContext(ctx, query+` ORDER BY started_at DESC LIMIT 1`, args...).Scan(&id, &start, &end)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !at.IsZero() && end.Valid {
		finished, err := parseStored(end.String)
		if err != nil {
			return nil, err
		}
		if at.After(finished.Add(leaseTrailingGrace)) {
			return nil, nil
		}
	}
	return &id, nil
}

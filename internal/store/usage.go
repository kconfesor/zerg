package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UsageTurn is one harness turn's tokens and cost.
type UsageTurn struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	TaskID    *string   `json:"taskId,omitempty"`
	Role      string    `json:"role"`
	At        time.Time `json:"at"`

	Harness  string `json:"harness"`
	Provider string `json:"provider"`
	Model    string `json:"model"`

	InputTokens      int `json:"inputTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	OutputTokens     int `json:"outputTokens"`

	CostUSD    float64 `json:"costUsd"`
	CostSource string  `json:"costSource"`
	Billing    string  `json:"billing"` // metered | subscription
}

// Where a cost figure came from.
//
// CostComputed is defined but never written: deriving cost requires a price
// table, and §9's model_prices does not exist yet. It is here so the value has
// one spelling when it does, not because anything produces it — a cost labelled
// as computed when nothing computed it is worse than no label, because a stored
// 0.0 then reads as "this turn was free".
const (
	CostFromHarness = "harness"
	CostUnknown     = "unknown"
	CostComputed    = "computed"
)

// RecordUsage stores one turn.
//
// Deliberately not part of any other transaction. Usage is a record of what
// already happened and has no bearing on whether work proceeds, so a failure
// to store it must never roll back or block the turn it describes.
func (db *DB) RecordUsage(ctx context.Context, u UsageTurn) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.At.IsZero() {
		u.At = time.Now().UTC()
	}
	if u.CostSource == "" {
		// Unknown, not harness. A missing label must not assert that the
		// harness stated a figure it never sent.
		u.CostSource = CostUnknown
	}
	return insertUsage(ctx, db.sql, u)
}

// execer is whatever the caller has: the pool, or a transaction it is already
// inside. Usage is written both ways — on its own, and beside the event that
// carried it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertUsage(ctx context.Context, x execer, u UsageTurn) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.At.IsZero() {
		u.At = time.Now().UTC()
	}
	if u.CostSource == "" {
		u.CostSource = CostUnknown
	}
	_, err := x.ExecContext(ctx,
		`INSERT INTO usage_turns
		   (id, project_id, task_id, role, ts, harness, provider, model,
		    input_tokens, cache_write_tokens, cache_read_tokens, output_tokens,
		    cost_usd, cost_source, billing)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.ProjectID, u.TaskID, u.Role, u.At.Format(time.RFC3339Nano),
		u.Harness, u.Provider, u.Model,
		u.InputTokens, u.CacheWriteTokens, u.CacheReadTokens, u.OutputTokens,
		u.CostUSD, u.CostSource, u.Billing)
	if err != nil {
		return fmt.Errorf("recording usage: %w", err)
	}
	return nil
}

// UsageTotal is spend grouped by whatever the caller asked to group by.
type UsageTotal struct {
	Key string `json:"key"` // role, provider or model, per the query

	Turns            int     `json:"turns"`
	InputTokens      int     `json:"inputTokens"`
	CacheWriteTokens int     `json:"cacheWriteTokens"`
	CacheReadTokens  int     `json:"cacheReadTokens"`
	OutputTokens     int     `json:"outputTokens"`
	CostUSD          float64 `json:"costUsd"`

	// SubscriptionTurns are turns with no marginal dollar cost. Reporting one
	// total across both billing modes would describe neither: a subscription
	// run looks free, and a metered run looks like it used no tokens.
	SubscriptionTurns int `json:"subscriptionTurns"`

	// UnpricedTurns are turns whose harness reported no cost. Their tokens are
	// real and counted; their contribution to CostUSD is zero because nothing
	// knows what they cost. Without this the total reads as complete when it is
	// merely the part that happened to be priced.
	UnpricedTurns int `json:"unpricedTurns"`
}

// usageGroupable is the set of columns callers may group by, as an allowlist.
// The column name is interpolated into SQL, so it can never come from a
// caller's string directly.
var usageGroupable = map[string]string{
	"role":     "role",
	"provider": "provider",
	"model":    "model",
}

// ValidUsageGrouping reports whether usage can be grouped by this name. Callers
// validate with it rather than interpreting an error, so a bad grouping is a
// 400 at the edge instead of a 500 from the query layer.
func ValidUsageGrouping(name string) bool {
	_, ok := usageGroupable[name]
	return ok
}

// UsageByGroup totals a project's spend, grouped by role, provider or model.
func (db *DB) UsageByGroup(ctx context.Context, projectID, groupBy string, since time.Time) ([]UsageTotal, error) {
	col, ok := usageGroupable[groupBy]
	if !ok {
		return nil, fmt.Errorf("cannot group usage by %q", groupBy)
	}

	args := []any{projectID}
	where := "project_id = ?"
	if !since.IsZero() {
		where += " AND ts >= ?"
		args = append(args, since.Format(time.RFC3339Nano))
	}

	rows, err := db.read.QueryContext(ctx,
		`SELECT `+col+` AS k, COUNT(*),
		        COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		        COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(cost_usd),0),
		        COALESCE(SUM(CASE WHEN billing = 'subscription' THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN cost_source = 'harness' THEN 0 ELSE 1 END),0)
		 FROM usage_turns WHERE `+where+`
		 GROUP BY k ORDER BY SUM(cost_usd) DESC, k ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("totalling usage: %w", err)
	}
	defer rows.Close()

	out := []UsageTotal{}
	for rows.Next() {
		var t UsageTotal
		if err := rows.Scan(&t.Key, &t.Turns, &t.InputTokens, &t.CacheWriteTokens,
			&t.CacheReadTokens, &t.OutputTokens, &t.CostUSD,
			&t.SubscriptionTurns, &t.UnpricedTurns); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UsageForTask totals one card, across every role that touched it and every
// lap it made through the pipeline. This is what rework actually costs.
func (db *DB) UsageForTask(ctx context.Context, taskID string) (UsageTotal, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		        COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(cost_usd),0),
		        COALESCE(SUM(CASE WHEN billing = 'subscription' THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN cost_source = 'harness' THEN 0 ELSE 1 END),0)
		 FROM usage_turns WHERE task_id = ?`, taskID)

	var t UsageTotal
	t.Key = taskID
	err := row.Scan(&t.Turns, &t.InputTokens, &t.CacheWriteTokens,
		&t.CacheReadTokens, &t.OutputTokens, &t.CostUSD,
		&t.SubscriptionTurns, &t.UnpricedTurns)
	if err != nil {
		return t, fmt.Errorf("totalling task usage: %w", err)
	}
	return t, nil
}

// CurrentTaskFor is the task a role is working, or most recently worked.
//
// Usage events say which role produced them but not which card they were spent
// on; the role's lease is what connects the two. Without it a turn's cost can
// be attributed to a project but never to the work that incurred it.
//
// An acked lease still counts, and that is the point. A turn's final usage
// event routinely arrives just after the agent has run `zerg done` — the
// tokens were spent on that card, and requiring the lease to still be open
// would drop the most expensive event of every turn on a timing race.
func (db *DB) CurrentTaskFor(ctx context.Context, projectID, role string) (*string, error) {
	return db.TaskForAt(ctx, projectID, role, time.Time{})
}

// TaskForAt is the task a role held at a given moment.
//
// "Now" is the wrong question to ask about an event that happened earlier. The
// recorder writes behind a queue, so an event produced while a role held task A
// can reach the writer after that role has claimed task B — and asking which
// lease is newest at write time attributes A's tokens, and A's transcript, to
// B. Under a burst that silently moves the expensive part of one card's cost
// onto another card, with nothing anywhere reporting a problem.
//
// The event's own timestamp is immutable, so the answer is too: the lease this
// role held when the event was produced. Zero means now, which is what the
// live callers want.
func (db *DB) TaskForAt(ctx context.Context, projectID, role string, at time.Time) (*string, error) {
	where, args := "", []any{projectID, role}
	if !at.IsZero() {
		// granted_at only. A lease that was acked before the event still owns
		// it: a turn's final usage event routinely lands just after `zerg
		// done`, and the tokens were spent on that card.
		where = " AND l.granted_at <= ?"
		args = append(args, at.UTC().Format(time.RFC3339Nano))
	}
	var taskID sql.NullString
	err := db.read.QueryRowContext(ctx,
		`SELECT m.task_id
		   FROM leases l
		   JOIN lease_items li ON li.lease_id = l.id
		   JOIN messages m ON m.id = li.message_id
		  WHERE l.project_id = ? AND l.role = ? AND m.task_id IS NOT NULL`+where+`
		  ORDER BY l.granted_at DESC LIMIT 1`, args...).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) || !taskID.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &taskID.String, nil
}

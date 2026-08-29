package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
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

// A spend window, and the ranges the cockpit can ask for.
//
// The set is an allowlist rather than a free-form duration because these become
// a row of chips: an interface that can express "the last 37 hours" invites
// someone to wonder whether they should.
const (
	RangeSession = "session" // since this project's most recent session began
	RangeDay     = "24h"
	RangeWeek    = "7d"
	RangeMonth   = "30d"
	RangeAll     = "all"
)

// ValidSpendRange reports whether a range is one the store understands, so a
// bad one is a 400 at the edge rather than an empty window that reads as
// "nothing was spent".
func ValidSpendRange(name string) bool {
	switch name {
	case RangeSession, RangeDay, RangeWeek, RangeMonth, RangeAll:
		return true
	}
	return false
}

// ResolveSpendRange turns a range name into the moment it starts.
//
// A zero time means all of history and is the honest answer for two different
// questions — "everything" and "this session, of which there has never been
// one" — so the caller is told which it got rather than inferring it from a
// timestamp.
func (db *DB) ResolveSpendRange(ctx context.Context, projectID, name string) (time.Time, error) {
	now := time.Now().UTC()
	switch name {
	case RangeAll:
		return time.Time{}, nil
	case RangeDay:
		return now.Add(-24 * time.Hour), nil
	case RangeWeek:
		return now.AddDate(0, 0, -7), nil
	case RangeMonth:
		return now.AddDate(0, 0, -30), nil
	case RangeSession:
		return db.LatestSessionStart(ctx, projectID)
	}
	return time.Time{}, invalid("unknown range %q", name)
}

// LatestSessionStart is when this project's most recent session began, running
// or finished. Zero when the project has never been started.
func (db *DB) LatestSessionStart(ctx context.Context, projectID string) (time.Time, error) {
	var started string
	err := db.read.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE project_id = ?
		  ORDER BY started_at DESC LIMIT 1`, projectID).Scan(&started)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the last session: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return time.Time{}, fmt.Errorf("session %s has an unreadable started_at: %w", projectID, err)
	}
	return at, nil
}

// RoleUsage is one role's spend, shaped for the row it becomes.
//
// UsageByGroup answers "grouped by one column", which is the wrong shape for
// the spend view's centrepiece: a row there is a role *and* the models it ran
// on *and* the four token classes *and* what that cost. Asking three times and
// joining in the browser would give three answers that can disagree about the
// same window.
type RoleUsage struct {
	Role string `json:"role"`

	Turns            int     `json:"turns"`
	InputTokens      int     `json:"inputTokens"`
	CacheWriteTokens int     `json:"cacheWriteTokens"`
	CacheReadTokens  int     `json:"cacheReadTokens"`
	OutputTokens     int     `json:"outputTokens"`
	CostUSD          float64 `json:"costUsd"`

	// SubscriptionTurns and UnpricedTurns carry the same meaning they do on
	// UsageTotal: how much of this figure is an estimate rather than a bill,
	// and how much of it nothing knows the price of.
	SubscriptionTurns int `json:"subscriptionTurns"`
	UnpricedTurns     int `json:"unpricedTurns"`

	// Models and Providers are every distinct one this role ran on in the
	// window, busiest first. Plural because a role's model can change mid
	// window, and reporting the newest as though it were the only one would
	// attribute opus prices to sonnet tokens.
	Models    []string `json:"models"`
	Providers []string `json:"providers"`

	// Tasks is how many distinct cards this role's spend is attributed to.
	// Zero is ordinary: chat runs outside the pipeline and belongs to none.
	Tasks int `json:"tasks"`

	// LastAt is the most recent turn, so a role that has stopped spending is
	// distinguishable from one that never started.
	LastAt time.Time `json:"lastAt"`
}

// UsageByRole totals each role's spend over a window, with the models it ran on.
//
// Grouped finely and folded in Go rather than grouped by role in SQL: the
// per-role sums and the list of models it used are two different groupings of
// the same rows, and one pass over (role, model, provider) gives both without
// asking twice. The result set is roles x models, which is single digits.
func (db *DB) UsageByRole(ctx context.Context, projectID string, since time.Time) ([]RoleUsage, error) {
	args := []any{projectID}
	where := "project_id = ?"
	if !since.IsZero() {
		where += " AND ts >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}

	rows, err := db.read.QueryContext(ctx,
		`SELECT role, model, provider,
		        COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		        COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(cost_usd),0),
		        COALESCE(SUM(CASE WHEN billing = 'subscription' THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN cost_source = 'harness' THEN 0 ELSE 1 END),0),
		        MAX(ts)
		   FROM usage_turns
		  WHERE `+where+`
		  GROUP BY role, model, provider
		  ORDER BY role, COUNT(*) DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("totalling usage by role: %w", err)
	}
	defer rows.Close()

	byRole := map[string]*RoleUsage{}
	var order []string
	// Distinct tasks have to be counted across a role's whole window, not
	// summed per model: one card worked on two models is one card.
	tasks := map[string]map[string]bool{}

	for rows.Next() {
		var (
			role, model, provider string
			r                     RoleUsage
			last                  sql.NullString
		)
		if err := rows.Scan(&role, &model, &provider,
			&r.Turns, &r.InputTokens, &r.CacheWriteTokens, &r.CacheReadTokens,
			&r.OutputTokens, &r.CostUSD, &r.SubscriptionTurns, &r.UnpricedTurns,
			&last); err != nil {
			return nil, err
		}

		cur, ok := byRole[role]
		if !ok {
			cur = &RoleUsage{Role: role}
			byRole[role] = cur
			order = append(order, role)
			tasks[role] = map[string]bool{}
		}
		cur.Turns += r.Turns
		cur.InputTokens += r.InputTokens
		cur.CacheWriteTokens += r.CacheWriteTokens
		cur.CacheReadTokens += r.CacheReadTokens
		cur.OutputTokens += r.OutputTokens
		cur.CostUSD += r.CostUSD
		cur.SubscriptionTurns += r.SubscriptionTurns
		cur.UnpricedTurns += r.UnpricedTurns
		if model != "" && !slices.Contains(cur.Models, model) {
			cur.Models = append(cur.Models, model)
		}
		if provider != "" && !slices.Contains(cur.Providers, provider) {
			cur.Providers = append(cur.Providers, provider)
		}
		if last.Valid {
			if at, err := time.Parse(time.RFC3339Nano, last.String); err == nil && at.After(cur.LastAt) {
				cur.LastAt = at
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := db.countRoleTasks(ctx, projectID, since, tasks); err != nil {
		return nil, err
	}

	out := make([]RoleUsage, 0, len(order))
	for _, role := range order {
		r := byRole[role]
		r.Tasks = len(tasks[role])
		out = append(out, *r)
	}
	// Most expensive first: the question this view answers is "what is the
	// money going on", and the answer should be the first row.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out, nil
}

// countRoleTasks fills in how many distinct cards each role spent on.
func (db *DB) countRoleTasks(ctx context.Context, projectID string, since time.Time, into map[string]map[string]bool) error {
	args := []any{projectID}
	where := "project_id = ? AND task_id IS NOT NULL"
	if !since.IsZero() {
		where += " AND ts >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	rows, err := db.read.QueryContext(ctx,
		`SELECT DISTINCT role, task_id FROM usage_turns WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("counting the cards a role spent on: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var role, task string
		if err := rows.Scan(&role, &task); err != nil {
			return err
		}
		if set, ok := into[role]; ok {
			set[task] = true
		}
	}
	return rows.Err()
}

// CacheFlag is a role whose cache hit rate has fallen against its own past.
//
// This is the regression nothing else reports. Prompt caching is a prefix match
// over tools -> system -> messages, so one changed byte in the composed system
// prompt invalidates everything after it — and the failure is silent. No error,
// no warning, just cache_read_input_tokens going to zero while the same work
// costs roughly ten times more on input. §11.4 calls the hit rate a headline
// metric for exactly this reason: it is the only number that moves.
type CacheFlag struct {
	Role string `json:"role"`

	// Recent and Trailing are hit rates, 0..1, over the newest turns and
	// everything before them in the window.
	Recent   float64 `json:"recent"`
	Trailing float64 `json:"trailing"`

	RecentTurns   int `json:"recentTurns"`
	TrailingTurns int `json:"trailingTurns"`

	// EditedAt is when this role's library entry last changed, and is present
	// only when that is recent enough to be a plausible cause. Deliberately
	// "edited" and not "its prompt was edited": the timestamp moves for any
	// change to the template, and claiming the prompt specifically would be
	// asserting something this does not know.
	EditedAt *time.Time `json:"editedAt,omitempty"`
}

const (
	// cacheFloor is the trailing rate below which a fall is not news. A role
	// that has never cached well has nothing to regress from, and flagging it
	// every window would train the reader to ignore the flag.
	cacheFloor = 0.4

	// cacheDrop is how far the rate has to fall to be worth saying. Twenty
	// points is roughly a doubling of the input bill, which is the point at
	// which someone would want to know.
	cacheDrop = 0.2

	// cacheMinTurns per side, so one unusual turn is never a regression.
	cacheMinTurns = 3

	// cacheWindow caps how many of a role's turns are read. A trailing average
	// over the last two hundred turns is as good as one over ten thousand, and
	// this runs on a page load.
	cacheWindow = 200

	// cacheEditRecent is how long after a template edit the edit is still
	// offered as the likely cause.
	cacheEditRecent = 6 * time.Hour
)

// CacheRegressions finds roles whose cache hit rate has fallen against their
// own trailing average, newest fall first.
func (db *DB) CacheRegressions(ctx context.Context, projectID string, since time.Time) ([]CacheFlag, error) {
	args := []any{projectID}
	where := "project_id = ?"
	if !since.IsZero() {
		where += " AND ts >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}

	// Ordered newest first and capped, so the split below is "the recent ones
	// against everything before them" without reading a year of history.
	rows, err := db.read.QueryContext(ctx,
		`SELECT role, cache_read_tokens, input_tokens, cache_write_tokens
		   FROM usage_turns
		  WHERE `+where+`
		  ORDER BY ts DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading cache rates: %w", err)
	}
	defer rows.Close()

	type turn struct{ read, in, write int }
	byRole := map[string][]turn{}
	var order []string
	for rows.Next() {
		var role string
		var t turn
		if err := rows.Scan(&role, &t.read, &t.in, &t.write); err != nil {
			return nil, err
		}
		if _, seen := byRole[role]; !seen {
			order = append(order, role)
		}
		if len(byRole[role]) < cacheWindow {
			byRole[role] = append(byRole[role], t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	edits, err := db.roleEdits(ctx)
	if err != nil {
		return nil, err
	}

	// A hit rate over a set of turns, summed rather than averaged per turn: a
	// turn with ten tokens and a turn with a million are not equal evidence.
	rate := func(ts []turn) (float64, bool) {
		var read, total int
		for _, t := range ts {
			read += t.read
			total += t.read + t.in + t.write
		}
		if total == 0 {
			return 0, false
		}
		return float64(read) / float64(total), true
	}

	var out []CacheFlag
	for _, role := range order {
		ts := byRole[role]
		if len(ts) < cacheMinTurns*2 {
			continue
		}
		// A third recent, two thirds trailing, with a floor on each side.
		n := len(ts) / 3
		if n < cacheMinTurns {
			n = cacheMinTurns
		}
		if len(ts)-n < cacheMinTurns {
			continue
		}
		recent, okR := rate(ts[:n])
		trailing, okT := rate(ts[n:])
		if !okR || !okT {
			continue
		}
		if trailing < cacheFloor || recent > trailing-cacheDrop {
			continue
		}

		flag := CacheFlag{
			Role: role, Recent: recent, Trailing: trailing,
			RecentTurns: n, TrailingTurns: len(ts) - n,
		}
		if at, ok := edits[role]; ok && time.Since(at) < cacheEditRecent {
			edited := at
			flag.EditedAt = &edited
		}
		out = append(out, flag)
	}

	// Worst fall first: if there is more than one, the biggest is the one to
	// look at.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Trailing-out[i].Recent > out[j].Trailing-out[j].Recent
	})
	return out, nil
}

// roleEdits is when each library role was last changed, keyed by the name usage
// rows are recorded under.
//
// Every role in the library, not this project's team. A role taken off the team
// keeps the spend it already incurred, and that spend is exactly when an edit
// is worth knowing about — joining through project_roles would drop the
// explanation for every row it can still be asked about. Names are unique in
// the library, which is what makes the lookup by name sound.
func (db *DB) roleEdits(ctx context.Context) (map[string]time.Time, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT name, updated_at FROM role_templates`)
	if err != nil {
		return nil, fmt.Errorf("reading role edits: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var name, updated string
		if err := rows.Scan(&name, &updated); err != nil {
			return nil, err
		}
		if at, err := time.Parse(time.RFC3339Nano, updated); err == nil {
			out[name] = at
		}
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
	// "Now" and "at this moment in the past" are different questions and are
	// answered differently on purpose. Asking what a role holds now must not
	// compare a lease against the wall clock: leases are written with whatever
	// clock made them, and a comparison across two clocks is a coin flip.
	if at.IsZero() {
		return db.openLeaseTask(ctx, projectID, role)
	}

	var (
		taskID    sql.NullString
		acked     sql.NullString
		expired   sql.NullString
		expiresAt string
	)
	err := db.read.QueryRowContext(ctx,
		`SELECT m.task_id, l.acked_at, l.expired_at, l.expires_at
		   FROM leases l
		   JOIN lease_items li ON li.lease_id = l.id
		   JOIN messages m ON m.id = li.message_id
		  WHERE l.project_id = ? AND l.role = ? AND m.task_id IS NOT NULL
		    AND l.granted_at <= ?
		  ORDER BY l.granted_at DESC LIMIT 1`,
		projectID, role, at.UTC().Format(time.RFC3339Nano)).Scan(&taskID, &acked, &expired, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) || !taskID.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !leaseOwns(at, acked, expired, expiresAt) {
		return nil, nil
	}
	return &taskID.String, nil
}

// openLeaseTask is the card a role is holding right now: the newest lease, and
// only if it is still open.
//
// A role that has acked its lease is holding nothing, and saying otherwise is
// what put three days of events onto cards that had already finished. Nothing
// here reads a clock, so it answers the same under a test's clock as under the
// daemon's.
func (db *DB) openLeaseTask(ctx context.Context, projectID, role string) (*string, error) {
	var taskID sql.NullString
	err := db.read.QueryRowContext(ctx,
		`SELECT m.task_id
		   FROM leases l
		   JOIN lease_items li ON li.lease_id = l.id
		   JOIN messages m ON m.id = li.message_id
		  WHERE l.project_id = ? AND l.role = ? AND m.task_id IS NOT NULL
		    AND l.acked_at IS NULL AND l.expired_at IS NULL
		  ORDER BY l.granted_at DESC LIMIT 1`, projectID, role).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) || !taskID.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &taskID.String, nil
}

// leaseTrailingGrace is how long after a lease closes its card still owns what
// the role emitted.
//
// Not zero: a turn's final usage event routinely arrives just after the agent
// has run `zerg done`, and those tokens were spent on that card. Not unbounded,
// which is what it was: with no end at all, every event a role produced for the
// rest of the daemon's life attributed to the last card it happened to hold.
// Three days of "ready" events landed on cards that had finished hours earlier,
// so a card's activity showed another card's work.
const leaseTrailingGrace = 2 * time.Minute

// leaseOwns reports whether a lease still owned what was emitted at a moment.
//
// A lease ends when the role acks it, when the daemon expires it, or when it
// runs out; the earliest of those is the end, and anything past it plus the
// grace belongs to no card rather than to the wrong one.
func leaseOwns(at time.Time, acked, expired sql.NullString, expiresAt string) bool {
	end := parseLeaseTime(expiresAt)
	for _, closed := range []sql.NullString{acked, expired} {
		if !closed.Valid || closed.String == "" {
			continue
		}
		if t := parseLeaseTime(closed.String); !t.IsZero() && (end.IsZero() || t.Before(end)) {
			end = t
		}
	}
	// An unreadable timestamp is not a reason to lose the attribution: this
	// package writes the column, so a parse failure means something stranger
	// than a stale lease.
	if end.IsZero() {
		return true
	}
	return !at.After(end.Add(leaseTrailingGrace))
}

func parseLeaseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Task states. lane says which role holds a card; state says whether that role
// is actually working it. With only the lane, a card reads as "in cleaner's
// lane" the instant delivery happens, whether or not cleaner has looked at it.
const (
	TaskQueued  = "queued"
	TaskWorking = "working"
	TaskDone    = "done"
	// TaskRejected is a role's verdict: the work was looked at and refused.
	TaskRejected = "rejected"
	// A card a person parks is also TaskRejected, with StoppedAt set. The
	// states live in a CHECK constraint and changing one means rebuilding the
	// table, which for `tasks` means an implicit cascade through every message,
	// event and usage row — see schema_014.sql. StoppedAt is what tells the two
	// apart, and it answers "when" as well.
)

// What happened to the work when a task finished. Empty on a card that has not
// ended, and on one that ended before schema 19 recorded this.
const (
	// OutcomeMerged: the commit is in the base branch.
	OutcomeMerged = "merged"
	// OutcomePR: a pull request was opened, and OutcomeRef is its URL.
	OutcomePR = "pr"
	// OutcomeBranch: the work is committed on the role's branch and landing it
	// is someone else's decision.
	OutcomeBranch = "branch"
)

// LaneDone is the well a finished card lands in.
const LaneDone = "done"

// Message kinds. A handoff points at a commit and carries work; a note is
// out-of-band and never occupies a lease.
const (
	KindHandoff = "handoff"
	KindNote    = "note"
)

// Route states.
const (
	RouteHeld     = "held" // waiting on a human approval
	RouteQueued   = "queued"
	RouteClaimed  = "claimed"
	RouteDone     = "done"
	RouteRejected = "rejected"
)

// Approval states.
const (
	ApprovalPending = "pending"
	// ApprovalIntegrating is claimed by whichever decision got there first,
	// held across the merge or PR, and replaced by the outcome.
	ApprovalIntegrating = "integrating"
	ApprovalApproved    = "approved"
	ApprovalRejected    = "rejected"
)

type Task struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"projectId"`
	SessionID      *string    `json:"sessionId,omitempty"`
	Name           string     `json:"name"`
	Body           string     `json:"body"`
	Lane           string     `json:"lane"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"createdAt"`
	FirstClaimedAt *time.Time `json:"firstClaimedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	ActiveMS       int64      `json:"activeMs"`

	// ReworkCount is how many times this card has gone backward through the
	// pipeline. Rework is legitimate; an unbounded amount of it is a loop
	// nobody is paying attention to.
	ReworkCount int `json:"reworkCount"`

	// Hidden is a card the person has put away. Finished work that is still
	// finished — the board stops showing it, nothing else changes.
	Hidden bool `json:"hidden"`

	// StoppedAt is set when a person parked this card, which is a different
	// event from a role rejecting it even though both leave State "rejected".
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`

	// Outcome is what happened to the work when the last role finished, and
	// OutcomeRef is where it went: the commit that was merged, or the pull
	// request's URL. Recorded at the moment it happens, because the project's
	// integration setting is what it is *now* and a task ended under whatever
	// it was then.
	Outcome    string `json:"outcome,omitempty"`
	OutcomeRef string `json:"outcomeRef,omitempty"`

	// Tokens and CostUSD are what this card has cost across every role and
	// every lap. A board that shows only a lane says nothing about the price
	// of what it is showing.
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"costUsd"`

	// Doing is the most recent thing an agent did on this card, for cards
	// being worked. "working" for four minutes is indistinguishable from
	// stuck; "running cargo test" is not.
	Doing string `json:"doing,omitempty"`
}

type Message struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	TaskID    *string   `json:"taskId,omitempty"`
	FromRole  string    `json:"fromRole"`
	Kind      string    `json:"kind"`
	Priority  int       `json:"priority"`
	CommitSHA *string   `json:"commitSha,omitempty"`
	Body      string    `json:"body"`
	Terminal  bool      `json:"terminal"`
	CreatedAt time.Time `json:"createdAt"`
}

// Lease is a claim with a deadline. Expiry returns its work to the queue,
// which is what makes a missed hand-off recoverable rather than permanent.
type Lease struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	Role      string    `json:"role"`
	GrantedAt time.Time `json:"grantedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Items     []Message `json:"items"`

	// Merged records, per message id, whether the orchestrator actually got
	// that message's commit into the claiming role's worktree. It is derived
	// from the merge attempt, never from the presence of a commit — a boolean
	// named after an action has to be set by that action's result. Transient:
	// it describes this hand-off, not a stored fact.
	Merged map[string]bool `json:"-"`
}

type Approval struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	MessageID string    `json:"messageId"`
	State     string    `json:"state"`
	Note      *string   `json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	TaskName  string    `json:"taskName,omitempty"`
	TaskID    string    `json:"taskId,omitempty"`
	FromRole  string    `json:"fromRole,omitempty"`

	// Body is what the sending role wrote when it handed the work on, and
	// Commit is what it points at. Without them the approval card names a task
	// and a role and asks you to decide — which is asking someone to approve
	// something they cannot read.
	Body   string `json:"body,omitempty"`
	Commit string `json:"commit,omitempty"`

	// Terminal marks the approval that lands the work. It changes what the
	// reader needs to see: for a hand-off between roles, the question is what
	// that role just wrote; for this one, it is everything about to reach the
	// base branch, which is usually more than one commit.
	Terminal bool `json:"terminal"`
}

type Session struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"projectId"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	EndReason *string    `json:"endReason,omitempty"`
}

// ── tasks ─────────────────────────────────────────────────────────────────

// CreateTask opens a card in the given lane.
func (db *DB) CreateTask(ctx context.Context, projectID, name, body, lane string) (*Task, error) {
	if name == "" {
		return nil, invalid("a task needs a name; the name follows the card through the whole pipeline")
	}
	t := &Task{
		ID:        NewID(),
		ProjectID: projectID,
		Name:      name,
		Body:      body,
		Lane:      lane,
		State:     TaskQueued,
		CreatedAt: time.Now().UTC(),
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, name, body, lane, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		t.ID, t.ProjectID, t.Name, t.Body, t.Lane, t.State, t.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("creating task %q: %w", name, err)
	}
	return t, nil
}

const taskCols = `id, project_id, session_id, name, body, lane, state,
	created_at, first_claimed_at, completed_at, active_ms, rework_count, hidden, stopped_at,
	outcome, outcome_ref`

// taskColsT is the same list qualified to the tasks table, for the queries that
// join. Unqualified names resolve today and would become ambiguous the moment a
// joined table gained a column of the same name.
const taskColsT = `t.id, t.project_id, t.session_id, t.name, t.body, t.lane, t.state,
	t.created_at, t.first_claimed_at, t.completed_at, t.active_ms, t.rework_count, t.hidden,
	t.stopped_at, t.outcome, t.outcome_ref`

// GetTaskIn resolves a task that must belong to this project.
//
// GetTask is global, which is right for the cockpit — it addresses a task by
// id and already knows the project — and wrong for anything an agent supplies.
// An agent names a task from inside one project, so the project is part of the
// identity, not context to be assumed.
func (db *DB) GetTaskIn(ctx context.Context, projectID, id string) (*Task, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE id = ? AND project_id = ?`, id, projectID)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading task %s: %w", id, err)
	}
	return t, nil
}

func (db *DB) GetTask(ctx context.Context, id string) (*Task, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	return t, err
}

// ListTasks returns a project's board, newest first.
func (db *DB) ListTasks(ctx context.Context, projectID string) ([]Task, error) {
	// Totals and the latest activity come back with the tasks rather than in a
	// request per card: a board with twenty cards would otherwise make
	// twenty-one round trips every two seconds.
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+taskColsT+`,
		        COALESCE(u.tokens, 0), COALESCE(u.cost, 0),
		        COALESCE(e.doing, '')
		 FROM tasks t
		 LEFT JOIN (
		     SELECT task_id,
		            SUM(input_tokens + cache_read_tokens + cache_write_tokens + output_tokens) AS tokens,
		            SUM(cost_usd) AS cost
		       FROM usage_turns GROUP BY task_id
		 ) u ON u.task_id = t.id
		 LEFT JOIN (
		     SELECT task_id,
		            -- The newest event that says something a person can read.
		            -- Ids are monotonic, so MAX(id) is the latest.
		            (SELECT CASE WHEN kind = 'tool_call' THEN COALESCE(NULLIF(tool, ''), 'working')
		                         ELSE text END
		               FROM events e2
		              WHERE e2.task_id = e1.task_id
		                AND kind IN ('tool_call', 'message')
		              ORDER BY id DESC LIMIT 1) AS doing
		       FROM events e1 GROUP BY task_id
		 ) e ON e.task_id = t.id
		 WHERE t.project_id = ? ORDER BY t.created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTaskWithSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// scanTaskWithSummary reads a task plus the totals and activity the board shows.
func scanTaskWithSummary(s scanner) (*Task, error) {
	var (
		t           Task
		sessionID   sql.NullString
		created     string
		firstClaim  sql.NullString
		completedAt sql.NullString
		stoppedAt   sql.NullString
	)
	if err := s.Scan(&t.ID, &t.ProjectID, &sessionID, &t.Name, &t.Body, &t.Lane, &t.State,
		&created, &firstClaim, &completedAt, &t.ActiveMS, &t.ReworkCount, &t.Hidden, &stoppedAt,
		&t.Outcome, &t.OutcomeRef,
		&t.Tokens, &t.CostUSD, &t.Doing); err != nil {
		return nil, err
	}
	if err := fillTaskTimes(&t, sessionID, created, firstClaim, completedAt, stoppedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

func scanTask(s scanner) (*Task, error) {
	var (
		t           Task
		sessionID   sql.NullString
		created     string
		firstClaim  sql.NullString
		completedAt sql.NullString
		stoppedAt   sql.NullString
	)
	if err := s.Scan(&t.ID, &t.ProjectID, &sessionID, &t.Name, &t.Body, &t.Lane, &t.State,
		&created, &firstClaim, &completedAt, &t.ActiveMS, &t.ReworkCount, &t.Hidden,
		&stoppedAt, &t.Outcome, &t.OutcomeRef); err != nil {
		return nil, err
	}
	if err := fillTaskTimes(&t, sessionID, created, firstClaim, completedAt, stoppedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// fillTaskTimes parses the stored timestamps onto a task. Shared by both
// scanners so the two cannot come to disagree about how a time is read.
func fillTaskTimes(t *Task, sessionID sql.NullString, created string, firstClaim, completedAt, stoppedAt sql.NullString) error {
	if sessionID.Valid {
		t.SessionID = &sessionID.String
	}
	var err error
	if t.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return fmt.Errorf("task %s has an unreadable created_at: %w", t.ID, err)
	}
	if t.FirstClaimedAt, err = nullTime(firstClaim); err != nil {
		return err
	}
	if t.CompletedAt, err = nullTime(completedAt); err != nil {
		return err
	}
	if t.StoppedAt, err = nullTime(stoppedAt); err != nil {
		return err
	}
	return nil
}

func nullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		return nil, fmt.Errorf("unreadable timestamp %q: %w", ns.String, err)
	}
	return &t, nil
}

// ── sessions ──────────────────────────────────────────────────────────────

func (db *DB) StartSession(ctx context.Context, projectID string) (*Session, error) {
	s := &Session{ID: NewID(), ProjectID: projectID, StartedAt: time.Now().UTC()}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, started_at) VALUES (?,?,?)`,
		s.ID, s.ProjectID, s.StartedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("starting session: %w", err)
	}
	return s, nil
}

func (db *DB) EndSession(ctx context.Context, id, reason string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), reason, id)
	if err != nil {
		return fmt.Errorf("ending session %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("open session %s", id))
}

func (db *DB) ListSessions(ctx context.Context, projectID string) ([]Session, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id, project_id, started_at, ended_at, end_reason
		 FROM sessions WHERE project_id = ? ORDER BY started_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			s       Session
			started string
			ended   sql.NullString
			reason  sql.NullString
		)
		if err := rows.Scan(&s.ID, &s.ProjectID, &started, &ended, &reason); err != nil {
			return nil, err
		}
		var err error
		if s.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, err
		}
		if s.EndedAt, err = nullTime(ended); err != nil {
			return nil, err
		}
		if reason.Valid {
			s.EndReason = &reason.String
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ── messages ──────────────────────────────────────────────────────────────

func scanMessage(s scanner) (*Message, error) {
	var (
		m         Message
		taskID    sql.NullString
		commitSHA sql.NullString
		created   string
		terminal  int
	)
	if err := s.Scan(&m.ID, &m.ProjectID, &taskID, &m.FromRole, &m.Kind, &m.Priority,
		&commitSHA, &m.Body, &terminal, &created); err != nil {
		return nil, err
	}
	if taskID.Valid {
		m.TaskID = &taskID.String
	}
	if commitSHA.Valid {
		m.CommitSHA = &commitSHA.String
	}
	m.Terminal = terminal != 0
	var err error
	if m.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("message %s has an unreadable created_at: %w", m.ID, err)
	}
	return &m, nil
}

const messageCols = `id, project_id, task_id, from_role, kind, priority,
	commit_sha, body, terminal, created_at`

// ── approvals ─────────────────────────────────────────────────────────────

// ListPendingApprovals returns what Attention must show, joined to the task so
// a human sees which card they are deciding about rather than a message id.
func (db *DB) ListPendingApprovals(ctx context.Context, projectID string) ([]Approval, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT a.id, a.project_id, a.message_id, a.state, a.note, a.created_at,
		        COALESCE(t.name, ''), COALESCE(m.task_id, ''), m.from_role,
		        m.body, COALESCE(m.commit_sha, ''), m.terminal
		 FROM approvals a
		 JOIN messages m ON m.id = a.message_id
		 LEFT JOIN tasks t ON t.id = m.task_id
		 WHERE a.project_id = ? AND a.state = ?
		 ORDER BY a.created_at`, projectID, ApprovalPending)
	if err != nil {
		return nil, fmt.Errorf("listing approvals: %w", err)
	}
	defer rows.Close()

	var out []Approval
	for rows.Next() {
		var (
			a        Approval
			note     sql.NullString
			created  string
			terminal int
		)
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.MessageID, &a.State, &note,
			&created, &a.TaskName, &a.TaskID, &a.FromRole, &a.Body, &a.Commit,
			&terminal); err != nil {
			return nil, err
		}
		a.Terminal = terminal != 0
		if note.Valid {
			a.Note = &note.String
		}
		var err error
		if a.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── clarifications ────────────────────────────────────────────────────────

// Clarification states.
const (
	ClarificationOpen      = "open"
	ClarificationAnswered  = "answered"
	ClarificationCancelled = "cancelled"
)

// Clarification is an agent's question waiting on a human.
type Clarification struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"projectId"`
	TaskID     *string    `json:"taskId,omitempty"`
	Role       string     `json:"role"`
	Question   string     `json:"question"`
	Answer     *string    `json:"answer,omitempty"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"createdAt"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
}

// AskClarification records a question and returns it.
func (db *DB) AskClarification(ctx context.Context, projectID, role, question string, taskID *string) (*Clarification, error) {
	if question == "" {
		return nil, invalid("a clarification needs a question")
	}
	c := &Clarification{
		ID: NewID(), ProjectID: projectID, TaskID: taskID, Role: role,
		Question: question, State: ClarificationOpen, CreatedAt: time.Now().UTC(),
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO clarifications (id, project_id, task_id, role, question, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		c.ID, c.ProjectID, c.TaskID, c.Role, c.Question, c.State,
		c.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("recording clarification: %w", err)
	}
	return c, nil
}

// AnswerClarification records a human's answer.
func (db *DB) AnswerClarification(ctx context.Context, id, answer string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE clarifications SET answer = ?, state = ?, answered_at = ?
		 WHERE id = ? AND state = ?`,
		answer, ClarificationAnswered, time.Now().UTC().Format(time.RFC3339Nano),
		id, ClarificationOpen)
	if err != nil {
		return fmt.Errorf("answering clarification %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("open clarification %s", id))
}

// GetClarification reads one question and whatever answer it has.
func (db *DB) GetClarification(ctx context.Context, id string) (*Clarification, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT id, project_id, task_id, role, question, answer, state, created_at, answered_at
		 FROM clarifications WHERE id = ?`, id)
	c, err := scanClarification(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("clarification %s: %w", id, ErrNotFound)
	}
	return c, err
}

// ListOpenClarifications returns what Attention must show.
func (db *DB) ListOpenClarifications(ctx context.Context, projectID string) ([]Clarification, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id, project_id, task_id, role, question, answer, state, created_at, answered_at
		 FROM clarifications WHERE project_id = ? AND state = ? ORDER BY created_at`,
		projectID, ClarificationOpen)
	if err != nil {
		return nil, fmt.Errorf("listing clarifications: %w", err)
	}
	defer rows.Close()

	var out []Clarification
	for rows.Next() {
		c, err := scanClarification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func scanClarification(s scanner) (*Clarification, error) {
	var (
		c        Clarification
		taskID   sql.NullString
		answer   sql.NullString
		created  string
		answered sql.NullString
	)
	if err := s.Scan(&c.ID, &c.ProjectID, &taskID, &c.Role, &c.Question,
		&answer, &c.State, &created, &answered); err != nil {
		return nil, err
	}
	if taskID.Valid {
		c.TaskID = &taskID.String
	}
	if answer.Valid {
		c.Answer = &answer.String
	}
	var err error
	if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, err
	}
	if c.AnsweredAt, err = nullTime(answered); err != nil {
		return nil, err
	}
	return &c, nil
}

// QueuedCount reports how much work is waiting for a role.
//
// The overmind uses this to decide whether an idle agent is worth nudging.
// Held routes are excluded: work behind an approval gate is not the agent's to
// see yet, and nudging over it would have the agent claim nothing and stop.
func (db *DB) QueuedCount(ctx context.Context, projectID, role string) (int, error) {
	var n int
	err := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM routes r JOIN messages m ON m.id = r.message_id
		 WHERE r.to_role = ? AND r.state = ? AND m.project_id = ?`,
		role, RouteQueued, projectID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting queued work for %s: %w", role, err)
	}
	return n, nil
}

// SettingReworkThreshold is how many backward handoffs a card may take before
// it is raised for a human. Stored rather than hardcoded, because how much
// rework is normal is a judgement about a project, not a fact about zerg.
const SettingReworkThreshold = "rework_threshold"

// DefaultReworkThreshold is deliberately low. Three laps between the same two
// roles usually means they disagree about something a human should settle,
// not that the work is nearly done.
const DefaultReworkThreshold = 3

// ReworkThreshold reads the configured threshold, falling back to the default.
func (db *DB) ReworkThreshold(ctx context.Context) int {
	raw, err := db.GetSetting(ctx, SettingReworkThreshold)
	if err != nil {
		return DefaultReworkThreshold
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return DefaultReworkThreshold
	}
	return n
}

// ListReworkedTasks returns open cards that have bounced at least threshold
// times, for Attention to raise.
//
// Finished cards are excluded: a card that took four laps and then shipped is
// history worth keeping, not a decision anyone still needs to make.
func (db *DB) ListReworkedTasks(ctx context.Context, projectID string, threshold int) ([]Task, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+taskCols+` FROM tasks
		 WHERE project_id = ? AND rework_count >= ? AND state NOT IN (?, ?)
		 ORDER BY rework_count DESC, created_at`,
		projectID, threshold, TaskDone, TaskRejected)
	if err != nil {
		return nil, fmt.Errorf("listing reworked tasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetTaskByName resolves a card by the name that follows it through the
// pipeline. Agents think in names — the name is what every handoff carries —
// so the name has to be a usable handle, not just a label.
func (db *DB) GetTaskByName(ctx context.Context, projectID, name string) (*Task, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE project_id = ? AND name = ?`, projectID, name)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("task %q: %w", name, ErrNotFound)
	}
	return t, err
}

// Handoff is one step of a task's history, as the detail view shows it.
type Handoff struct {
	From   string    `json:"from"`
	To     string    `json:"to,omitempty"` // empty means this one finished the task
	Kind   string    `json:"kind"`
	Commit string    `json:"commit,omitempty"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
	Final  bool      `json:"final"`
}

// TaskHistory is every step a task took, oldest first.
//
// The bodies are the point. Each is what a role wrote when it handed the work
// on — the verdict, the rework list, what was left out — and together they are
// the account of what happened that a state of "done" cannot give.
func (db *DB) TaskHistory(ctx context.Context, taskID string) ([]Handoff, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT m.from_role, COALESCE(r.to_role, ''), m.kind,
		        COALESCE(m.commit_sha, ''), m.body, m.created_at, m.terminal
		   FROM messages m
		   LEFT JOIN routes r ON r.message_id = m.id
		  WHERE m.task_id = ?
		  ORDER BY m.created_at ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("reading task history: %w", err)
	}
	defer rows.Close()

	out := []Handoff{}
	for rows.Next() {
		var (
			h     Handoff
			at    string
			final int
		)
		if err := rows.Scan(&h.From, &h.To, &h.Kind, &h.Commit, &h.Body, &at, &final); err != nil {
			return nil, err
		}
		h.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("handoff has an unparseable timestamp %q: %w", at, err)
		}
		h.Final = final != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetApproval reads one approval, with the message detail the UI needs to show
// what is being decided about.
func (db *DB) GetApproval(ctx context.Context, id string) (*Approval, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT a.id, a.project_id, a.message_id, a.state, a.note, a.created_at,
		        COALESCE(t.name, ''), COALESCE(m.task_id, ''), m.from_role,
		        m.body, COALESCE(m.commit_sha, ''), m.terminal
		   FROM approvals a
		   JOIN messages m ON m.id = a.message_id
		   LEFT JOIN tasks t ON t.id = m.task_id
		  WHERE a.id = ?`, id)

	var (
		a        Approval
		note     sql.NullString
		created  string
		terminal int
	)
	err := row.Scan(&a.ID, &a.ProjectID, &a.MessageID, &a.State, &note, &created,
		&a.TaskName, &a.TaskID, &a.FromRole, &a.Body, &a.Commit, &terminal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("approval %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading approval: %w", err)
	}
	a.Terminal = terminal != 0
	if note.Valid {
		a.Note = &note.String
	}
	if a.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, err
	}
	return &a, nil
}

// SetTaskHidden puts a card away, or brings it back.
//
// No state or lane change: a hidden card is finished work that is still
// finished, and unhiding must return it exactly as it was. Only tasks that
// have actually finished can be hidden — putting away something still moving
// through the pipeline would hide the work, not the record of it.
func (db *DB) SetTaskHidden(ctx context.Context, id string, hidden bool) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE tasks SET hidden = ? WHERE id = ? AND state = 'done'`, hidden, id)
	if err != nil {
		return fmt.Errorf("setting hidden on %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("task %s is not a finished card", id)
	}
	return nil
}

// StopTask parks a card: no agent will pick it up again, and its history stays.
//
// The lease is expired and every route that has not been delivered is closed,
// because a card whose state says stopped while its route still says queued is
// a card that starts again two seconds later. Commits already made stay in the
// role's branch — they are git's, not zerg's to remove.
func (db *DB) StopTask(ctx context.Context, projectID, taskID string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning stop: %w", err)
	}
	defer tx.Rollback()

	stoppedAt := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET state = ?, stopped_at = ?
		  WHERE id = ? AND project_id = ? AND state IN (?, ?)`,
		TaskRejected, stoppedAt, taskID, projectID, TaskQueued, TaskWorking)
	if err != nil {
		return fmt.Errorf("stopping task: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return invalid("that card is not queued or being worked on")
	}

	// Close its undelivered routes so nothing claims it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE routes SET state = ?
		  WHERE state IN (?, ?)
		    AND message_id IN (SELECT id FROM messages WHERE task_id = ?)`,
		RouteDone, RouteQueued, RouteHeld, taskID); err != nil {
		return fmt.Errorf("closing routes: %w", err)
	}

	// Cancel anything it was still asking. The role that asked has nothing left
	// to do with the answer, and an open question outlives the card otherwise —
	// the same ghost a delete used to leave, arriving by a different route.
	if _, err := tx.ExecContext(ctx,
		`UPDATE clarifications SET state = 'cancelled'
		  WHERE task_id = ? AND project_id = ? AND state = 'open'`,
		taskID, projectID); err != nil {
		return fmt.Errorf("cancelling its questions: %w", err)
	}

	// And release any lease covering them, or the holder keeps its claim until
	// the deadline and the board shows work in flight that has been stopped.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`UPDATE leases SET expired_at = ?
		  WHERE acked_at IS NULL AND expired_at IS NULL AND project_id = ?
		    AND id IN (SELECT li.lease_id FROM lease_items li
		                 JOIN messages m ON m.id = li.message_id
		                WHERE m.task_id = ?)`,
		now, projectID, taskID); err != nil {
		return fmt.Errorf("releasing the lease: %w", err)
	}
	return tx.Commit()
}

// DeleteTask removes a card and the record that only makes sense beside it.
//
// Messages, routes and approvals go by foreign key. Events are deleted
// explicitly: they are reachable only through their task, so leaving them
// behind is keeping bytes nobody can read.
//
// Usage rows are kept, orphaned. The money was spent, and a project total that
// dropped when a card was tidied away would be a cost report that quietly
// disagrees with the bill.
func (db *DB) DeleteTask(ctx context.Context, projectID, taskID string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning delete: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE task_id = ? AND project_id = ?`, taskID, projectID); err != nil {
		return fmt.Errorf("deleting the transcript: %w", err)
	}
	// Questions go with the card, like its transcript.
	//
	// clarifications.task_id is ON DELETE SET NULL, so without this the delete
	// detached the question instead of removing it: an item in Attention about
	// a card that no longer exists, unanswerable and impossible to clear, since
	// answering is the only thing a person can do to one. Not filtered out on
	// read, because a null task_id is also a legitimate question asked about no
	// card at all — `zerg ask` does not require one.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM clarifications WHERE task_id = ? AND project_id = ?`,
		taskID, projectID); err != nil {
		return fmt.Errorf("deleting its questions: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM tasks WHERE id = ? AND project_id = ?`, taskID, projectID)
	if err != nil {
		return fmt.Errorf("deleting task: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ── history ───────────────────────────────────────────────────────────────

// HistoryEntry is one worked task as the history screen reads it: the card,
// what it cost, and which roles touched it.
type HistoryEntry struct {
	Task
	// Roles that sent at least one handoff on this task, in the order they
	// first did. Which agents worked on it is the question a list of names
	// answers and a lane does not.
	Roles []string `json:"roles"`
}

// HistoryFilter narrows the list. Zero values mean no narrowing.
type HistoryFilter struct {
	Outcome string // merged, pr, branch, or "none" for a card that ended without one
	Role    string // touched by this role
	Query   string // matches the task name
	// Before is the cursor from a previous page: everything strictly older
	// than this position. Empty starts at the newest.
	Before string
	Limit  int
}

// ListHistory returns worked tasks newest first, a page at a time.
//
// Not ListTasks with a filter. That query carries a per-card subquery for what
// each agent is doing right now, which the board polls every two seconds and
// history has no use for, and it returns every card a project has ever had in
// one answer. This one is ordered by when work ended, pages on a cursor, and
// includes the cards a person has put away, which are exactly the ones history
// is for.
func (db *DB) ListHistory(ctx context.Context, projectID string, f HistoryFilter) ([]HistoryEntry, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	where := []string{"t.project_id = ?"}
	args := []any{projectID}
	switch {
	case f.Outcome == "none":
		where = append(where, "t.outcome = ''")
	case f.Outcome != "":
		where = append(where, "t.outcome = ?")
		args = append(args, f.Outcome)
	}
	if f.Role != "" {
		where = append(where, `EXISTS (SELECT 1 FROM messages r WHERE r.task_id = t.id AND r.from_role = ?)`)
		args = append(args, f.Role)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "t.name LIKE ? ESCAPE '\\'")
		args = append(args, "%"+likeEscape(q)+"%")
	}
	// The cursor is a position, not an offset: a page taken while a task
	// finishes would otherwise repeat a row or skip one. Row values compare
	// left to right, so the id breaks ties between tasks that ended in the
	// same nanosecond.
	if at, id, ok := splitCursor(f.Before); ok {
		where = append(where, "(COALESCE(t.completed_at, t.created_at), t.id) < (?, ?)")
		args = append(args, at, id)
	}

	args = append(args, limit+1) // one extra says whether there is another page
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+taskColsT+`,
		        COALESCE(u.tokens, 0), COALESCE(u.cost, 0),
		        COALESCE((SELECT group_concat(role, char(10)) FROM (
		            SELECT m.from_role AS role, MIN(m.created_at) AS first
		              FROM messages m WHERE m.task_id = t.id AND m.from_role <> ''
		             GROUP BY m.from_role ORDER BY first)), '')
		   FROM tasks t
		   LEFT JOIN (
		       SELECT task_id,
		              SUM(input_tokens + cache_read_tokens + cache_write_tokens + output_tokens) AS tokens,
		              SUM(cost_usd) AS cost
		         FROM usage_turns GROUP BY task_id
		   ) u ON u.task_id = t.id
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY COALESCE(t.completed_at, t.created_at) DESC, t.id DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("reading history: %w", err)
	}
	defer rows.Close()

	out := []HistoryEntry{}
	for rows.Next() {
		var (
			e           HistoryEntry
			sessionID   sql.NullString
			created     string
			firstClaim  sql.NullString
			completedAt sql.NullString
			stoppedAt   sql.NullString
			roles       string
		)
		if err := rows.Scan(&e.ID, &e.ProjectID, &sessionID, &e.Name, &e.Body, &e.Lane, &e.State,
			&created, &firstClaim, &completedAt, &e.ActiveMS, &e.ReworkCount, &e.Hidden,
			&stoppedAt, &e.Outcome, &e.OutcomeRef, &e.Tokens, &e.CostUSD, &roles); err != nil {
			return nil, "", err
		}
		if err := fillTaskTimes(&e.Task, sessionID, created, firstClaim, completedAt, stoppedAt); err != nil {
			return nil, "", err
		}
		e.Roles = []string{}
		if roles != "" {
			e.Roles = strings.Split(roles, "\n")
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		at := last.CreatedAt
		if last.CompletedAt != nil {
			at = *last.CompletedAt
		}
		next = at.Format(time.RFC3339Nano) + " " + last.ID
	}
	return out, next, nil
}

// splitCursor reads a cursor back into the position it names.
func splitCursor(cursor string) (at, id string, ok bool) {
	at, id, ok = strings.Cut(cursor, " ")
	if !ok || at == "" || id == "" {
		return "", "", false
	}
	return at, id, true
}

// likeEscape keeps a search for "100%" from matching everything.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

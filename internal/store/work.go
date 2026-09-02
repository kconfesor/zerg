package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

// OperatorRole is the sender of the message that opens a card. It is a person,
// not an agent, and it is on every task, so the history lists the roles that
// worked on a card without it.
const OperatorRole = "operator"

// ChatRole is the agent a person talks to about the project, outside the
// pipeline. Named here rather than imported from internal/chat, because the
// store cannot depend on it and retention has to know which events are a
// conversation.
const ChatRole = "chat"

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

	// Pinned keeps this card's transcript past the retention window. Events are
	// swept because they are the expensive tier (ARCHITECTURE §12.1); the card
	// worth reading in six months is usually the one that went wrong, and this
	// is how it is kept.
	Pinned bool `json:"pinned"`

	// Tokens and CostUSD are what this card has cost across every role and
	// every lap. A board that shows only a lane says nothing about the price
	// of what it is showing.
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"costUsd"`

	// Doing is the most recent thing an agent did on this card, for cards
	// being worked. "working" for four minutes is indistinguishable from
	// stuck; "running cargo test" is not.
	Doing string `json:"doing,omitempty"`

	// Deploy is where this card's work should be put when it lands, decided by
	// whoever wrote the card. Empty for most of them: a preview costs an agent
	// turn, and only some work is worth looking at.
	Deploy string `json:"deploy,omitempty"`

	// Skip is the roles this one card does not visit, as role template ids.
	// The pipeline belongs to the project and most cards want all of it; a
	// one-line fix does not want a plan, and editing the team to get that
	// changes every card that follows. Empty for almost every card.
	//
	// It governs automatic forward routing only -- the opening lane, the
	// `next` an agent is handed, and which role is terminal. An explicit
	// recipient still reaches a skipped role, because a reviewer that finds a
	// problem on a card whose coder was skipped still has to be able to send
	// the work back to it.
	Skip []string `json:"skip,omitempty"`

	// Models are the models that have actually spent tokens on this card, in
	// the order they first did. What a role is configured with is a live value
	// and answers a different question: this says what produced the work in
	// front of you, which is the one worth asking when a card came out well or
	// badly.
	Models []string `json:"models,omitempty"`
}

// Where a finished card gets deployed. Empty is the default and means nowhere.
const (
	DeployNone  = ""
	DeployLocal = "local"
)

// ValidDeploy reports whether a target is one this build knows how to reach.
//
// Named targets rather than a boolean because dev and staging are the same
// decision with a different destination, and an unknown one has to be refused
// at the edge: stored, it would be a card that silently never deploys.
func ValidDeploy(target string) bool {
	switch target {
	case DeployNone, DeployLocal:
		return true
	}
	return false
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
	outcome, outcome_ref, pinned, deploy, skip`

// taskColsT is the same list qualified to the tasks table, for the queries that
// join. Unqualified names resolve today and would become ambiguous the moment a
// joined table gained a column of the same name.
const taskColsT = `t.id, t.project_id, t.session_id, t.name, t.body, t.lane, t.state,
	t.created_at, t.first_claimed_at, t.completed_at, t.active_ms, t.rework_count, t.hidden,
	t.stopped_at, t.outcome, t.outcome_ref, t.pinned, t.deploy, t.skip`

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
		        COALESCE(u.tokens, 0), COALESCE(u.cost, 0), COALESCE(u.models, ''),
		        COALESCE(e.doing, '')
		 FROM tasks t
		 LEFT JOIN (
		     SELECT task_id,
		            SUM(input_tokens + cache_read_tokens + cache_write_tokens + output_tokens) AS tokens,
		            SUM(cost_usd) AS cost,
		            -- Which models did the work, first use first. A subselect
		            -- rather than group_concat(DISTINCT ...), which SQLite
		            -- returns in no defined order: a card whose models
		            -- reshuffled between polls would flicker on the board.
		            (SELECT group_concat(model, char(10)) FROM (
		                SELECT model, MIN(ts) AS first
		                  FROM usage_turns m
		                 WHERE m.task_id = usage_turns.task_id AND model <> ''
		                 GROUP BY model ORDER BY first
		            )) AS models
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
	var models, skip string
	if err := s.Scan(&t.ID, &t.ProjectID, &sessionID, &t.Name, &t.Body, &t.Lane, &t.State,
		&created, &firstClaim, &completedAt, &t.ActiveMS, &t.ReworkCount, &t.Hidden, &stoppedAt,
		&t.Outcome, &t.OutcomeRef, &t.Pinned, &t.Deploy, &skip,
		&t.Tokens, &t.CostUSD, &models, &t.Doing); err != nil {
		return nil, err
	}
	var err error
	if t.Skip, err = unmarshalArgs(skip); err != nil {
		return nil, fmt.Errorf("task %s has an unreadable skip list: %w", t.ID, err)
	}
	if models != "" {
		t.Models = strings.Split(models, "\n")
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
	var skip string
	if err := s.Scan(&t.ID, &t.ProjectID, &sessionID, &t.Name, &t.Body, &t.Lane, &t.State,
		&created, &firstClaim, &completedAt, &t.ActiveMS, &t.ReworkCount, &t.Hidden,
		&stoppedAt, &t.Outcome, &t.OutcomeRef, &t.Pinned, &t.Deploy, &skip); err != nil {
		return nil, err
	}
	var err error
	if t.Skip, err = unmarshalArgs(skip); err != nil {
		return nil, fmt.Errorf("task %s has an unreadable skip list: %w", t.ID, err)
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
	if t.CreatedAt, err = parseStored(created); err != nil {
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
	t, err := parseStored(ns.String)
	if err != nil {
		return nil, err
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

// CloseOpenSessions ends every session still marked open and reports how many
// there were.
//
// For the start of a run. A session is closed by the shutdown that ends it, so
// one still open at boot belongs to a daemon that was killed rather than asked,
// and leaving it that way makes "how many sessions, how long" answer with a
// period that never ended. Every agent it counted is gone regardless.
func (db *DB) CloseOpenSessions(ctx context.Context, reason string) (int, error) {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE ended_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), reason)
	if err != nil {
		return 0, fmt.Errorf("closing sessions from the previous run: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
		if s.StartedAt, err = parseStored(started); err != nil {
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
	if m.CreatedAt, err = parseStored(created); err != nil {
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
		if a.CreatedAt, err = parseStored(created); err != nil {
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
	ID        string  `json:"id"`
	ProjectID string  `json:"projectId"`
	TaskID    *string `json:"taskId,omitempty"`
	Role      string  `json:"role"`
	Question  string  `json:"question"`
	// Options the agent worked out itself, for a question that is a choice.
	// Empty is the free-text question this started as, and is what every row
	// written before 034 reads back as.
	Options []string `json:"options,omitempty"`
	Answer  *string  `json:"answer,omitempty"`
	State   string   `json:"state"`
	// DeliveredAt is when the answer was handed to an agent waiting on it.
	// Answered but never delivered is the answer that arrived a moment after
	// its asker gave up waiting, and it is what a repeat of the question is
	// given instead of a second card.
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	AnsweredAt  *time.Time `json:"answeredAt,omitempty"`
}

// maxClarificationOptions is where a choice stops being one. Ten radio
// buttons is already a lot to read on a phone, which is where approvals and
// questions actually get answered, and an agent offering more than that has
// not narrowed the decision down for anybody.
const maxClarificationOptions = 10

// AskClarification records a question, with the options it offers, and
// returns it. No options is a question answered in prose, which is every
// question asked before this was possible.
func (db *DB) AskClarification(ctx context.Context, projectID, role, question string, options []string, taskID *string) (*Clarification, error) {
	if question == "" {
		return nil, invalid("a clarification needs a question")
	}
	options, err := checkOptions(options)
	if err != nil {
		return nil, err
	}
	// A repeat of a question already waiting is that same question, not a
	// second one. `zerg ask` gives up after its wait and the agent asks again,
	// which filed two identical cards the operator could not tell apart, and
	// let two different answers be given to what was one decision. A repeat of
	// a question whose answer was never handed over gets that answer, so an
	// answer typed a moment after the asker stopped listening is late rather
	// than lost.
	prior, err := db.priorAsk(ctx, projectID, role, question, options, taskID)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		return prior, nil
	}
	c := &Clarification{
		ID: NewID(), ProjectID: projectID, TaskID: taskID, Role: role,
		Question: question, Options: options, State: ClarificationOpen,
		CreatedAt: time.Now().UTC(),
	}
	stored, err := encodeOptions(options)
	if err != nil {
		return nil, err
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO clarifications (id, project_id, task_id, role, question, options, state, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		c.ID, c.ProjectID, c.TaskID, c.Role, c.Question, stored, c.State,
		c.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("recording clarification: %w", err)
	}
	return c, nil
}

// priorAsk returns the question this one repeats, if there is one: the same
// role asking the same thing about the same card, offering the same answers.
//
// Open, or answered without the answer ever reaching an agent. A question that
// was answered and read is finished, so asking it again is a new question --
// an agent that comes back to a decision later deserves a fresh answer rather
// than the one it already acted on.
func (db *DB) priorAsk(ctx context.Context, projectID, role, question string, options []string, taskID *string) (*Clarification, error) {
	stored, err := encodeOptions(options)
	if err != nil {
		return nil, err
	}
	row := db.read.QueryRowContext(ctx,
		`SELECT id, project_id, task_id, role, question, options, answer, state, delivered_at, created_at, answered_at
		   FROM clarifications
		  WHERE project_id = ? AND role = ? AND question = ?
		    AND task_id IS ? AND options IS ?
		    AND (state = ? OR (state = ? AND delivered_at IS NULL))
		  ORDER BY created_at DESC LIMIT 1`,
		projectID, role, question, taskID, stored,
		ClarificationOpen, ClarificationAnswered)
	c, err := scanClarification(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// MarkClarificationDelivered records that an agent read the answer. Only the
// first reader sets it: the question is finished at that point, and a later
// reader is not who the answer was waiting for.
func (db *DB) MarkClarificationDelivered(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE clarifications SET delivered_at = ? WHERE id = ? AND delivered_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("recording that clarification %s was read: %w", id, err)
	}
	return nil
}

// encodeOptions is how the column holds an offer. NULL, not '[]', for a
// question with nothing to choose from: the column is what tells the cockpit
// which of the two shapes to draw, and what makes two asks comparable.
func encodeOptions(options []string) (any, error) {
	if len(options) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return nil, fmt.Errorf("encoding a question's options: %w", err)
	}
	return string(encoded), nil
}

// checkOptions is the agent's own mistakes, caught where they can still be
// reported to it: these are 400s naming what to fix, not faults. An option
// that is blank, or the same as another, is a radio button the operator
// cannot tell apart from its neighbour once it reaches the panel.
func checkOptions(options []string) ([]string, error) {
	if len(options) == 0 {
		return nil, nil
	}
	if len(options) > maxClarificationOptions {
		return nil, invalid("a question can offer at most %d options; this one offers %d",
			maxClarificationOptions, len(options))
	}
	out := make([]string, 0, len(options))
	seen := make(map[string]bool, len(options))
	for _, o := range options {
		o = strings.TrimSpace(o)
		if o == "" {
			return nil, invalid("an option cannot be empty")
		}
		if seen[o] {
			return nil, invalid("option %q is offered twice", o)
		}
		seen[o] = true
		out = append(out, o)
	}
	return out, nil
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
		`SELECT id, project_id, task_id, role, question, options, answer, state, delivered_at, created_at, answered_at
		 FROM clarifications WHERE id = ?`, id)
	c, err := scanClarification(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("clarification %s: %w", id, ErrNotFound)
	}
	return c, err
}

// ClarificationsForTask returns every question raised on a task, answered or
// not, oldest first. The trail reads these; Attention reads the open ones.
func (db *DB) ClarificationsForTask(ctx context.Context, taskID string) ([]Clarification, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id, project_id, task_id, role, question, options, answer, state, delivered_at, created_at, answered_at
		 FROM clarifications WHERE task_id = ? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, fmt.Errorf("listing a task's clarifications: %w", err)
	}
	defer rows.Close()

	out := []Clarification{}
	for rows.Next() {
		c, err := scanClarification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListOpenClarifications returns what Attention must show.
func (db *DB) ListOpenClarifications(ctx context.Context, projectID string) ([]Clarification, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id, project_id, task_id, role, question, options, answer, state, delivered_at, created_at, answered_at
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
		c         Clarification
		taskID    sql.NullString
		options   sql.NullString
		answer    sql.NullString
		delivered sql.NullString
		created   string
		answered  sql.NullString
	)
	if err := s.Scan(&c.ID, &c.ProjectID, &taskID, &c.Role, &c.Question,
		&options, &answer, &c.State, &delivered, &created, &answered); err != nil {
		return nil, err
	}
	if taskID.Valid {
		c.TaskID = &taskID.String
	}
	if options.Valid && options.String != "" {
		if err := json.Unmarshal([]byte(options.String), &c.Options); err != nil {
			return nil, fmt.Errorf("clarification %s has unreadable options %q: %w",
				c.ID, options.String, err)
		}
	}
	if answer.Valid {
		c.Answer = &answer.String
	}
	var err error
	if c.CreatedAt, err = parseStored(created); err != nil {
		return nil, err
	}
	if c.AnsweredAt, err = nullTime(answered); err != nil {
		return nil, err
	}
	if c.DeliveredAt, err = nullTime(delivered); err != nil {
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
		h.At, err = parseStored(at)
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
	if a.CreatedAt, err = parseStored(created); err != nil {
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
// SetTaskPinned keeps a task's transcript, or lets the sweep have it.
//
// Any task, not only a finished one: a card being worked on is exactly when
// somebody decides it is the one they will want to read later.
func (db *DB) SetTaskPinned(ctx context.Context, id string, pinned bool) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE tasks SET pinned = ? WHERE id = ?`, pinned, id)
	if err != nil {
		return fmt.Errorf("pinning %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("task %s", id))
}

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

	// HasTranscript is whether this card's events are still here, asked of the
	// table rather than worked out from the retention window: a sweep that has
	// not run yet and a window that was lengthened afterwards both make that
	// arithmetic wrong, and the answer decides whether a step can be opened.
	HasTranscript bool `json:"hasTranscript"`
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
	// The operator is the sender of the message that opens every card, so it is
	// on every row and says nothing about who worked on it. The trail still
	// shows that first message; this is the list of agents.
	args := []any{OperatorRole, projectID}
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
	// left to right, so the id breaks ties between two cards that ended in the
	// same nanosecond.
	//
	// Unfinished cards sort first as a group, and the rest by when they ended,
	// which is a key that never changes once written. Ordering everything by
	// "completed, or else created" made the key of a running card move the
	// moment it finished: a card below the cursor jumped above it and no later
	// page could return it. What is left of that is a card still running while
	// you page past the first fifty, which is a queue nobody is watching.
	if at, id, ok := splitCursor(f.Before); ok {
		where = append(where, "(t.completed_at IS NOT NULL) AND (COALESCE(t.completed_at, t.created_at), t.id) < (?, ?)")
		args = append(args, at, id)
	}

	args = append(args, limit+1) // one extra says whether there is another page
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+taskColsT+`,
		        COALESCE(u.tokens, 0), COALESCE(u.cost, 0),
		        EXISTS (SELECT 1 FROM events e WHERE e.task_id = t.id),
		        COALESCE((SELECT group_concat(role, char(10)) FROM (
		            SELECT m.from_role AS role, MIN(m.created_at) AS first
		              FROM messages m
		             WHERE m.task_id = t.id AND m.from_role <> '' AND m.from_role <> ?
		             GROUP BY m.from_role ORDER BY first)), '')
		   FROM tasks t
		   LEFT JOIN (
		       SELECT task_id,
		              SUM(input_tokens + cache_read_tokens + cache_write_tokens + output_tokens) AS tokens,
		              SUM(cost_usd) AS cost
		         FROM usage_turns GROUP BY task_id
		   ) u ON u.task_id = t.id
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY (t.completed_at IS NULL) DESC,
		           COALESCE(t.completed_at, t.created_at) DESC, t.id DESC
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
			skip        string
		)
		if err := rows.Scan(&e.ID, &e.ProjectID, &sessionID, &e.Name, &e.Body, &e.Lane, &e.State,
			&created, &firstClaim, &completedAt, &e.ActiveMS, &e.ReworkCount, &e.Hidden,
			&stoppedAt, &e.Outcome, &e.OutcomeRef, &e.Pinned, &e.Deploy, &skip,
			&e.Tokens, &e.CostUSD, &e.HasTranscript, &roles); err != nil {
			return nil, "", err
		}
		var err error
		if e.Skip, err = unmarshalArgs(skip); err != nil {
			return nil, "", fmt.Errorf("task %s has an unreadable skip list: %w", e.ID, err)
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

// ── the trail ─────────────────────────────────────────────────────────────

// TrailStep is one hop with everything that happened around it: how long the
// role held the work, what those turns cost, and whatever stopped it there.
type TrailStep struct {
	Handoff

	// StartedAt is when the role took the work, from the lease the handoff was
	// produced under. Absent for a step with no lease behind it, which is what
	// the operator's own first message is.
	StartedAt *time.Time `json:"startedAt,omitempty"`

	// DurationMS is how long that role held it: the handoff, less the lease.
	// Zero when there is no lease to measure from, which is not the same as a
	// step that took no time and is why the field is separate from StartedAt.
	DurationMS int64 `json:"durationMs"`

	// WindowStart and WindowEnd are what this step's turns were summed over,
	// handed out so that anything else reading the step reads the same span.
	// The transcript did not: it stopped at the handoff plus a guessed half
	// minute, so a closing turn that ran longer was missing from what a step
	// "did" while its cost was counted, and a quick second lap by the same role
	// leaked into the step before it. End is exclusive and absent on a role's
	// last step, which runs to the end of the card.
	WindowStart *time.Time `json:"windowStart,omitempty"`
	WindowEnd   *time.Time `json:"windowEnd,omitempty"`

	// What the turns inside that window cost. Per step rather than per task,
	// because "this pipeline cost four dollars" does not say which role spent
	// it or on which lap.
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"costUsd"`

	// Gate is the approval this handoff waited on, if it had one.
	Gate *TrailGate `json:"gate,omitempty"`

	// Clarifications the role raised while it held the work. A question asked
	// mid-step is most of the gap between a step's duration and its turns.
	Clarifications []Clarification `json:"clarifications,omitempty"`
}

// TrailGate is an approval as the trail reads it: what was decided, and how
// long the work sat waiting for a person to decide it.
type TrailGate struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	// WaitedMS is how long it was pending. This is where wall time goes when
	// active time is short, and it is invisible in any per-task total.
	WaitedMS int64 `json:"waitedMs"`
}

// TaskTrail is every step a task took, with the numbers that belong to each.
//
// Three queries rather than one per step: a task with twenty hops would
// otherwise make sixty round trips to answer one screen, which is the pattern
// ResolveTeam was rewritten to stop doing.
func (db *DB) TaskTrail(ctx context.Context, taskID string) ([]TrailStep, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT m.from_role, COALESCE(r.to_role, ''), m.kind,
		        COALESCE(m.commit_sha, ''), m.body, m.created_at, m.terminal,
		        l.granted_at,
		        a.id, a.state, a.note, a.created_at, a.decided_at
		   FROM messages m
		   LEFT JOIN routes r ON r.message_id = m.id
		   LEFT JOIN leases l ON l.id = m.source_lease_id
		   LEFT JOIN approvals a ON a.message_id = m.id
		  WHERE m.task_id = ?
		  ORDER BY m.created_at ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("reading the task trail: %w", err)
	}
	defer rows.Close()

	steps := []TrailStep{}
	for rows.Next() {
		var (
			s                           TrailStep
			granted                     sql.NullString
			final                       int
			gateID, gateState, gateNote sql.NullString
			gateCreated, gateDecided    sql.NullString
			createdAt                   string
		)
		if err := rows.Scan(&s.From, &s.To, &s.Kind, &s.Commit, &s.Body, &createdAt, &final,
			&granted, &gateID, &gateState, &gateNote, &gateCreated, &gateDecided); err != nil {
			return nil, err
		}
		if s.At, err = parseStored(createdAt); err != nil {
			return nil, fmt.Errorf("handoff has an unreadable timestamp %q: %w", createdAt, err)
		}
		s.Final = final != 0
		if s.StartedAt, err = nullTime(granted); err != nil {
			return nil, err
		}
		if s.StartedAt != nil {
			s.DurationMS = s.At.Sub(*s.StartedAt).Milliseconds()
			if s.DurationMS < 0 {
				s.DurationMS = 0
			}
		}
		if gateID.Valid {
			gate := TrailGate{ID: gateID.String, State: gateState.String, Note: gateNote.String}
			if gate.CreatedAt, err = parseStored(gateCreated.String); err != nil {
				return nil, fmt.Errorf("approval has an unreadable timestamp: %w", err)
			}
			if gate.DecidedAt, err = nullTime(gateDecided); err != nil {
				return nil, err
			}
			// Still pending: the wait is running, and reporting zero would say
			// a gate nobody has answered cost nothing.
			end := time.Now().UTC()
			if gate.DecidedAt != nil {
				end = *gate.DecidedAt
			}
			gate.WaitedMS = end.Sub(gate.CreatedAt).Milliseconds()
			s.Gate = &gate
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return steps, nil
	}

	if err := db.attachTurns(ctx, taskID, steps); err != nil {
		return nil, err
	}
	// The same windows the turns were bucketed into, so a reader of one step
	// asks for exactly what that step was charged for.
	for _, w := range windowsFor(steps) {
		from := w.from
		steps[w.step].WindowStart = &from
		if !w.until.IsZero() {
			until := w.until
			steps[w.step].WindowEnd = &until
		}
	}
	return steps, db.attachClarifications(ctx, taskID, steps)
}

// attachTurns puts each turn's cost on the step it was spent during.
//
// A turn belongs to the step whose role held the work when it happened, and
// that window runs from one of the role's leases to its next, not to the
// handoff in between. The turn that ends a step is recorded *after* the handoff
// it produced: the agent calls `zerg send`, which writes the message, and the
// turn carrying that call only finishes afterwards. Closing the window at the
// handoff put the largest turn of every step outside it, which showed up
// against real data as a card totalling $1.61 whose steps totalled $0.23.
//
// Turns before a role's first lease belong to no step and are left where they
// are rather than folded into a neighbour, which would move money between
// roles.
func (db *DB) attachTurns(ctx context.Context, taskID string, steps []TrailStep) error {
	rows, err := db.read.QueryContext(ctx,
		`SELECT role, ts, input_tokens + cache_read_tokens + cache_write_tokens + output_tokens, cost_usd
		   FROM usage_turns WHERE task_id = ? ORDER BY ts`, taskID)
	if err != nil {
		return fmt.Errorf("reading the trail's turns: %w", err)
	}
	defer rows.Close()

	windows := windowsFor(steps)
	for rows.Next() {
		var (
			role, ts string
			tokens   int64
			cost     float64
		)
		if err := rows.Scan(&role, &ts, &tokens, &cost); err != nil {
			return err
		}
		at, err := parseStored(ts)
		if err != nil {
			return fmt.Errorf("a turn has an unreadable timestamp %q: %w", ts, err)
		}
		if i, ok := stepAt(windows, role, at); ok {
			steps[i].Tokens += tokens
			steps[i].CostUSD += cost
		}
	}
	return rows.Err()
}

// stepWindow is when one role held the work, so a turn or a question can be
// placed on the step it happened during.
type stepWindow struct {
	role string
	from time.Time
	// until is zero for a role's last step, which runs on: the turn that ends
	// a step lands after the handoff that closed it.
	until time.Time
	step  int
}

func windowsFor(steps []TrailStep) []stepWindow {
	out := []stepWindow{}
	for i, s := range steps {
		from := s.StartedAt
		if from == nil {
			// No lease to measure from. Messages carried none before schema 11,
			// and those cards are most of what a history screen has to show, so
			// the work is placed between the handoff that gave it to this role
			// and whatever this role did next. Weaker than a lease, and the
			// alternative is a card whose steps all read $0 while the card
			// itself reads $2.74.
			if i == 0 {
				continue // nothing preceded the first message
			}
			from = &steps[i-1].At
		}
		out = append(out, stepWindow{role: s.From, from: *from, step: i})
	}
	// A role's window ends where that role's next one begins: a second lap
	// closes the first, and nothing else does.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].role == out[i].role {
				out[i].until = out[j].from
				break
			}
		}
	}
	return out
}

// stepAt is the step a role was on at that moment, if it was on one.
func stepAt(windows []stepWindow, role string, at time.Time) (int, bool) {
	for _, window := range windows {
		if window.role != role || at.Before(window.from) {
			continue
		}
		if !window.until.IsZero() && !at.Before(window.until) {
			continue
		}
		return window.step, true
	}
	return 0, false
}

// attachClarifications hangs each question on the step it was asked during,
// by the same windows the turns use.
func (db *DB) attachClarifications(ctx context.Context, taskID string, steps []TrailStep) error {
	asked, err := db.ClarificationsForTask(ctx, taskID)
	if err != nil {
		return err
	}
	windows := windowsFor(steps)
	for _, c := range asked {
		if i, ok := stepAt(windows, c.Role, c.CreatedAt); ok {
			steps[i].Clarifications = append(steps[i].Clarifications, c)
		}
	}
	return nil
}

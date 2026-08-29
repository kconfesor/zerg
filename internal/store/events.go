package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Event is one thing an agent did, as the cockpit displays it.
type Event struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	TaskID    *string   `json:"taskId,omitempty"`
	Role      string    `json:"role"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`

	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`

	// Data is whatever this kind carries: a tool's arguments, a turn's token
	// split and cost. Raw JSON so the store neither parses nor validates a
	// shape that only the view cares about.
	Data  json.RawMessage `json:"data,omitempty"`
	Fatal bool            `json:"fatal,omitempty"`

	// ChatID is the conversation this belongs to, for the two roles that are
	// one. Empty for everything else, which is almost every event: the
	// pipeline's work belongs to a card, not to a thread.
	ChatID string `json:"chatId,omitempty"`
}

// RecordEvent appends one event.
//
// Not part of any other transaction, for the same reason usage is not: this is
// a record of something that already happened, and failing to store it must
// never roll back or block the work it describes.
func (db *DB) RecordEvent(ctx context.Context, e *Event) error {
	return db.RecordTurn(ctx, e, nil)
}

// RecordTurn writes an event and, when the event carried one, its usage row —
// in one transaction.
//
// The two used to be two independent inserts. A usage event is one thing that
// happened, and writing half of it leaves the record and the ledger disagreeing
// about the same turn: a transcript entry for a turn with no cost, or a cost
// with no turn behind it. Neither is recoverable afterwards, because a harness
// reports a turn once.
func (db *DB) RecordTurn(ctx context.Context, e *Event, u *UsageTurn) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	var data any
	if len(e.Data) > 0 {
		data = string(e.Data)
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recording event: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (id, project_id, task_id, role, kind, ts, text, tool, data, fatal, chat_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.ProjectID, e.TaskID, e.Role, e.Kind, e.At.Format(time.RFC3339Nano),
		e.Text, e.Tool, data, e.Fatal, nullable(e.ChatID)); err != nil {
		return fmt.Errorf("recording event: %w", err)
	}

	if u != nil {
		if err := insertUsage(ctx, tx, *u); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording event: %w", err)
	}
	return nil
}

// eventCols is the row as the cockpit reads it.
const eventCols = `id, project_id, task_id, role, kind, ts, text, tool, data, fatal, chat_id`

// EventQuery selects a slice of the record.
type EventQuery struct {
	ProjectID string

	// Chat narrows to one conversation. Empty means every event the other
	// filters allow, conversations included.
	Chat string

	// After is an event id. Because ids are monotonic ULIDs, "after this id"
	// and "after this moment" are the same question, which is what lets an SSE
	// client resume from the Last-Event-ID it was holding.
	After string

	// Role and Task narrow the stream. Empty means every role, every card.
	Role string
	Task string

	// From and Until bound the window, which is how one step of a task's trail
	// is read rather than the whole card. Zero means unbounded. With From set
	// the oldest matching events come back rather than the newest: a step is
	// read from where it started, not from where it stopped.
	From  time.Time
	Until time.Time

	// Limit caps the rows returned. A replay has to be bounded — a project with
	// a week of history would otherwise send megabytes before the first live
	// event arrived.
	Limit int
}

// ListEvents returns events in chronological order.
//
// When After is empty the *last* Limit events are returned, not the first:
// opening the activity view should show what just happened, not the beginning
// of recorded history. They still come back oldest-first, so a caller appends
// without reversing.
func (db *DB) ListEvents(ctx context.Context, q EventQuery) ([]Event, error) {
	if q.Limit <= 0 || q.Limit > 2000 {
		q.Limit = 500
	}

	where := "project_id = ?"
	args := []any{q.ProjectID}
	if q.After != "" {
		where += " AND id > ?"
		args = append(args, q.After)
	}
	if q.Role != "" {
		where += " AND role = ?"
		args = append(args, q.Role)
	}
	if q.Task != "" {
		where += " AND task_id = ?"
		args = append(args, q.Task)
	}
	// One conversation, not the project's chatter. Asked for explicitly rather
	// than inferred from the role, because "every chat event in this project"
	// is no longer a thing anybody wants: it is several conversations laid on
	// top of each other.
	if q.Chat != "" {
		where += " AND chat_id = ?"
		args = append(args, q.Chat)
	}
	if !q.From.IsZero() {
		where += " AND ts >= ?"
		args = append(args, q.From.Format(time.RFC3339Nano))
	}
	if !q.Until.IsZero() {
		where += " AND ts <= ?"
		args = append(args, q.Until.Format(time.RFC3339Nano))
	}

	// Without a cursor, take the newest rows and put them back in order. The
	// inner query is what bounds the scan; ordering the outer one is free.
	query := `SELECT ` + eventCols + `
	          FROM events WHERE ` + where + ` ORDER BY id ASC LIMIT ?`
	if q.After == "" && q.From.IsZero() {
		query = `SELECT * FROM (
		             SELECT ` + eventCols + `
		             FROM events WHERE ` + where + ` ORDER BY id DESC LIMIT ?
		         ) ORDER BY id ASC`
	}
	args = append(args, q.Limit)

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading events: %w", err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		var (
			e      Event
			task   sql.NullString
			data   sql.NullString
			chatID sql.NullString
			at     string
			fatal  int
		)
		if err := rows.Scan(&e.ID, &e.ProjectID, &task, &e.Role, &e.Kind, &at,
			&e.Text, &e.Tool, &data, &fatal, &chatID); err != nil {
			return nil, err
		}
		e.ChatID = chatID.String
		if task.Valid {
			t := task.String
			e.TaskID = &t
		}
		if data.Valid && data.String != "" {
			e.Data = json.RawMessage(data.String)
		}
		e.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("event %s has an unparseable timestamp %q: %w", e.ID, at, err)
		}
		e.Fatal = fatal != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than the cutoff and reports how many went.
//
// Events are the expensive tier and the least valuable in the long run: roughly
// 40 MB a day at five active roles, against a few hundred kilobytes of usage
// rows for the same period. They exist to replay recent work. Metrics, costs
// and outcomes live in usage_turns and tasks, and survive this.
//
// The count is returned rather than swallowed so the caller can say what was
// dropped. Silent truncation reads exactly like complete history.
//
// A pinned task is exempt, however old. That is what pinning is for: the card
// worth reading in six months is usually the one that went wrong, and the
// window that is right for a project's ordinary work is wrong for that one.
//
// A conversation is exempt too, and for a different reason. These events are a
// role's working notes, kept for as long as they are worth replaying; a chat is
// the thing itself, and there is no other copy. A conversation that emptied
// itself after a fortnight, halfway through, with nothing said and nothing to
// undo, is not a retention policy anybody chose. It ends when the person ends
// it, which deletes it outright.
func (db *DB) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM events
		  WHERE ts < ?
		    AND role NOT IN (?, ?)
		    AND (task_id IS NULL
		         OR NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = events.task_id AND t.pinned = 1))`,
		before.UTC().Format(time.RFC3339Nano), ChatRole, OperatorRole)
	if err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	return res.RowsAffected()
}

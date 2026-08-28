package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A review is a conversation about the code, anchored to it.
//
// The gate used to take one note for a whole diff and end there. What a person
// actually does is remark on a line, get an answer, and decide whether the
// answer settles it, which is three things the old shape could not hold: where
// the remark points, who has replied, and whether it is finished.

const (
	// ThreadOpen is a remark nobody has settled yet. Work does not merge with
	// one of these on it, which is the point of writing it down.
	ThreadOpen = "open"
	// ThreadResolved is one the reviewer has closed. Only a person closes a
	// thread: an agent answering its own question would be marking its own
	// homework.
	ThreadResolved = "resolved"

	// ThreadRemark is the reviewer's own remark about the code, and holds the
	// gate until it is settled.
	ThreadRemark = "remark"
	// ThreadQuestion is a question the reviewer asked an agent while reading.
	// It holds nothing: making it an obligation would mean asking anything
	// costs a click to dismiss, and a reviewer who learns that stops asking.
	// Raising one turns it into a remark, which is the person deciding that
	// what they learned matters.
	ThreadQuestion = "question"
)

// ReviewThread is one remark and everything said about it.
type ReviewThread struct {
	ID         string  `json:"id"`
	ProjectID  string  `json:"projectId"`
	TaskID     string  `json:"taskId"`
	ApprovalID *string `json:"approvalId,omitempty"`

	// Where it points. Line 0 means the file as a whole.
	CommitSHA string `json:"commitSha,omitempty"`
	File      string `json:"file,omitempty"`
	Line      int    `json:"line"`

	// Kind is whether this holds the gate: a remark does, a question does not.
	Kind       string          `json:"kind"`
	State      string          `json:"state"`
	CreatedAt  time.Time       `json:"createdAt"`
	ResolvedAt *time.Time      `json:"resolvedAt,omitempty"`
	Comments   []ReviewComment `json:"comments"`
}

// ReviewComment is one turn in that conversation.
type ReviewComment struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"threadId"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// OpenReviewThread starts a thread with the remark that opened it.
//
// Both in one transaction: a thread with no comment is a row that renders as an
// empty box and blocks a merge, which is the worst of both.
func (db *DB) OpenReviewThread(ctx context.Context, t *ReviewThread, author, body string) (*ReviewThread, error) {
	if strings.TrimSpace(body) == "" {
		return nil, invalid("a review comment needs something in it")
	}
	if author == "" {
		return nil, invalid("a review comment needs an author")
	}
	if _, err := db.GetTask(ctx, t.TaskID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	t.ID, t.State, t.CreatedAt = NewID(), ThreadOpen, now
	if t.Kind == "" {
		t.Kind = ThreadRemark
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("opening a review thread: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_threads (id,project_id,task_id,approval_id,commit_sha,file,line,kind,state,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.ProjectID, t.TaskID, t.ApprovalID, t.CommitSHA, t.File, t.Line, t.Kind, t.State,
		now.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("opening a review thread: %w", err)
	}
	comment := ReviewComment{ID: NewID(), ThreadID: t.ID, Author: author, Body: body, CreatedAt: now}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_comments (id,thread_id,author,body,created_at) VALUES (?,?,?,?,?)`,
		comment.ID, comment.ThreadID, comment.Author, comment.Body,
		now.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("recording the remark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t.Comments = []ReviewComment{comment}
	return t, nil
}

// AddReviewComment adds a turn to a thread.
//
// A resolved thread still takes comments: a reviewer reopening a settled point
// is an ordinary thing to do, and refusing it would send them to a new thread
// that has lost the context of the old one.
func (db *DB) AddReviewComment(ctx context.Context, threadID, author, body string) (*ReviewComment, error) {
	if strings.TrimSpace(body) == "" {
		return nil, invalid("a review comment needs something in it")
	}
	if author == "" {
		return nil, invalid("a review comment needs an author")
	}
	if _, err := db.ReviewThread(ctx, threadID); err != nil {
		return nil, err
	}
	c := ReviewComment{ID: NewID(), ThreadID: threadID, Author: author, Body: body, CreatedAt: time.Now().UTC()}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO review_comments (id,thread_id,author,body,created_at) VALUES (?,?,?,?,?)`,
		c.ID, c.ThreadID, c.Author, c.Body, c.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("adding a review comment: %w", err)
	}
	return &c, nil
}

// SetReviewThreadState resolves a thread, or opens it again.
func (db *DB) SetReviewThreadState(ctx context.Context, id string, resolved bool) error {
	state, at := ThreadOpen, any(nil)
	if resolved {
		state, at = ThreadResolved, time.Now().UTC().Format(time.RFC3339Nano)
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE review_threads SET state = ?, resolved_at = ? WHERE id = ?`, state, at, id)
	if err != nil {
		return fmt.Errorf("resolving review thread %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("review thread %s", id))
}

// ReviewThread reads one thread with everything said on it.
func (db *DB) ReviewThread(ctx context.Context, id string) (*ReviewThread, error) {
	t, err := scanThread(db.read.QueryRowContext(ctx,
		`SELECT id,project_id,task_id,approval_id,commit_sha,file,line,kind,state,created_at,resolved_at
		   FROM review_threads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("review thread %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	t.Comments, err = db.reviewComments(ctx, t.ID)
	return t, err
}

// ReviewThreads is every thread on a card, oldest first, with its comments.
func (db *DB) ReviewThreads(ctx context.Context, taskID string) ([]ReviewThread, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id,project_id,task_id,approval_id,commit_sha,file,line,kind,state,created_at,resolved_at
		   FROM review_threads WHERE task_id = ? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, fmt.Errorf("reading review threads: %w", err)
	}
	out := []ReviewThread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Comments, err = db.reviewComments(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// OpenReviewThreads counts what still holds the gate on a card: remarks the
// reviewer has not settled. Questions are not counted, however many are open,
// because asking one is reading the change rather than objecting to it.
func (db *DB) OpenReviewThreads(ctx context.Context, taskID string) (int, error) {
	var n int
	if err := db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM review_threads WHERE task_id = ? AND kind = ? AND state = ?`,
		taskID, ThreadRemark, ThreadOpen).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting open review threads: %w", err)
	}
	return n, nil
}

// RaiseReviewThread turns a question into a remark: the person deciding that
// what they learned has to be dealt with before this lands.
func (db *DB) RaiseReviewThread(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE review_threads SET kind = ?, state = ?, resolved_at = NULL WHERE id = ?`,
		ThreadRemark, ThreadOpen, id)
	if err != nil {
		return fmt.Errorf("raising review thread %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("review thread %s", id))
}

func (db *DB) reviewComments(ctx context.Context, threadID string) ([]ReviewComment, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id,thread_id,author,body,created_at FROM review_comments
		  WHERE thread_id = ? ORDER BY created_at, id`, threadID)
	if err != nil {
		return nil, fmt.Errorf("reading review comments: %w", err)
	}
	defer rows.Close()

	// Empty, never nil: a thread always has at least the remark that opened it,
	// but `"comments": null` is what a cockpit dereferences and throws on.
	out := []ReviewComment{}
	for rows.Next() {
		var c ReviewComment
		var at string
		if err := rows.Scan(&c.ID, &c.ThreadID, &c.Author, &c.Body, &at); err != nil {
			return nil, err
		}
		if c.CreatedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("review comment %s has an unreadable timestamp: %w", c.ID, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanThread(s scanner) (*ReviewThread, error) {
	var (
		t          ReviewThread
		approval   sql.NullString
		created    string
		resolvedAt sql.NullString
	)
	if err := s.Scan(&t.ID, &t.ProjectID, &t.TaskID, &approval, &t.CommitSHA, &t.File, &t.Line,
		&t.Kind, &t.State, &created, &resolvedAt); err != nil {
		return nil, err
	}
	if approval.Valid {
		v := approval.String
		t.ApprovalID = &v
	}
	var err error
	if t.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("review thread %s has an unreadable timestamp: %w", t.ID, err)
	}
	if t.ResolvedAt, err = nullTime(resolvedAt); err != nil {
		return nil, err
	}
	t.Comments = []ReviewComment{}
	return &t, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Conversations with the project's chat agent.
//
// One per tab. A conversation owns its transcript, its attachments and the
// worktree its agent runs in, and ending one takes all three: that is the whole
// contract, and the reason a tab is worth having rather than a filter over one
// long thread.

// Chat is one conversation.
type Chat struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	// Title is what the tab says. Taken from the first message when nobody has
	// named it, because "Chat 3" tells you nothing about which one it is.
	Title      string    `json:"title"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`

	// Worktree and Branch are where this conversation's agent works. Filled in
	// by the API rather than stored: they are derived from the project's path
	// and this id by rules the daemon owns, and a copy in the database would
	// be a second answer able to disagree with the first.
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

const chatCols = `id, project_id, title, created_at, last_used_at`

// CreateChat opens a conversation.
func (db *DB) CreateChat(ctx context.Context, projectID, title string) (*Chat, error) {
	if projectID == "" {
		return nil, invalid("a conversation belongs to a project")
	}
	now := time.Now().UTC()
	c := &Chat{
		ID: NewID(), ProjectID: projectID, Title: strings.TrimSpace(title),
		CreatedAt: now, LastUsedAt: now,
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO chats (`+chatCols+`) VALUES (?,?,?,?,?)`,
		c.ID, c.ProjectID, c.Title,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("opening a conversation: %w", err)
	}
	return c, nil
}

// ListChats returns a project's conversations, most recently used first.
func (db *DB) ListChats(ctx context.Context, projectID string) ([]Chat, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+chatCols+` FROM chats WHERE project_id = ?
		  ORDER BY last_used_at DESC, id DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("reading conversations: %w", err)
	}
	defer rows.Close()

	out := []Chat{}
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetChat reads one, and refuses one belonging to another project.
//
// The project is part of the identity rather than context to be assumed: a
// conversation id arrives from a browser, and reading somebody else's thread by
// naming its id is not a thing this should make possible.
func (db *DB) GetChat(ctx context.Context, projectID, id string) (*Chat, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT `+chatCols+` FROM chats WHERE id = ? AND project_id = ?`, id, projectID)
	c, err := scanChat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("conversation %s: %w", id, ErrNotFound)
	}
	return c, err
}

// TouchChat records that a conversation is the one being used, which is what
// orders the tabs.
func (db *DB) TouchChat(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE chats SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

// NameChat titles a conversation.
//
// Called with the first thing said in it when nobody has named it by hand, so
// the tabs read as what they are about rather than as a numbered list. A title
// already set is left alone: a person's own name for it outranks a sentence
// they happened to open with.
func (db *DB) NameChat(ctx context.Context, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil
	}
	if len([]rune(title)) > 60 {
		title = strings.TrimSpace(string([]rune(title)[:60]))
	}
	_, err := db.sql.ExecContext(ctx,
		`UPDATE chats SET title = ? WHERE id = ?`, title, id)
	return err
}

// RenameChat sets a title a person typed, whether or not there was one.
func (db *DB) RenameChat(ctx context.Context, projectID, id, title string) error {
	title = strings.TrimSpace(title)
	if len([]rune(title)) > 60 {
		title = strings.TrimSpace(string([]rune(title)[:60]))
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE chats SET title = ? WHERE id = ? AND project_id = ?`, title, id, projectID)
	if err != nil {
		return fmt.Errorf("renaming a conversation: %w", err)
	}
	return mustAffect(res, fmt.Sprintf("conversation %s", id))
}

// DeleteChat removes a conversation, its transcript and its attachments, and
// reports the digests nothing names any more.
//
// One transaction: a half-deleted conversation is a tab that has lost its
// messages, or messages with no tab to reach them from.
func (db *DB) DeleteChat(ctx context.Context, projectID, id string) ([]string, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ending a conversation: %w", err)
	}
	defer tx.Rollback()

	var exists string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM chats WHERE id = ? AND project_id = ?`, id, projectID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("conversation %s: %w", id, ErrNotFound)
		}
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT sha256 FROM artifacts WHERE chat_id = ?`, id)
	if err != nil {
		return nil, err
	}
	var digests []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		if d != "" {
			digests = append(digests, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, stmt := range []string{
		`DELETE FROM artifacts WHERE chat_id = ?`,
		`DELETE FROM events WHERE chat_id = ?`,
		`DELETE FROM chats WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return nil, fmt.Errorf("ending a conversation: %w", err)
		}
	}

	orphans, err := orphanedDigests(ctx, tx, digests)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return orphans, nil
}

// nullable stores an empty string as NULL.
//
// A chat id is absent for almost every event ever recorded, and an index that
// excludes NULL is worth more than one carrying a row per event with "" in it.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanChat(s scanner) (*Chat, error) {
	var (
		c                 Chat
		created, lastUsed string
	)
	if err := s.Scan(&c.ID, &c.ProjectID, &c.Title, &created, &lastUsed); err != nil {
		return nil, err
	}
	var err error
	if c.CreatedAt, err = parseStored(created); err != nil {
		return nil, fmt.Errorf("conversation %s has an unreadable created_at: %w", c.ID, err)
	}
	if c.LastUsedAt, err = parseStored(lastUsed); err != nil {
		return nil, fmt.Errorf("conversation %s has an unreadable last_used_at: %w", c.ID, err)
	}
	return &c, nil
}

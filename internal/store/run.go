package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// What a runner learned about serving a project.
//
// Prose rather than fields, deliberately. An earlier attempt stored a command,
// a working directory and a list of files, which assumes the answer has that
// shape: a compose stack, a monorepo with three apps behind one script, and a
// binary that wants a database first are all different shapes, and the one
// thing they have in common is that somebody had to work it out by reading the
// repository. What is stored is that reading.

// RunNote is a project's note about how it runs.
type RunNote struct {
	ProjectID string    `json:"projectId"`
	Note      string    `json:"note"`
	Author    string    `json:"author"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RunNoteFor reads a project's note, or ErrNotFound when nothing has learned
// anything yet.
func (db *DB) RunNoteFor(ctx context.Context, projectID string) (*RunNote, error) {
	n := &RunNote{ProjectID: projectID}
	var at string
	err := db.read.QueryRowContext(ctx,
		`SELECT note, author, updated_at FROM run_notes WHERE project_id = ?`,
		projectID).Scan(&n.Note, &n.Author, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no run note for %s: %w", projectID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the run note: %w", err)
	}
	n.UpdatedAt, _ = time.Parse(time.RFC3339Nano, at)
	return n, nil
}

// SaveRunNote replaces it. One per project: this is what is currently known,
// not a history of what was tried.
func (db *DB) SaveRunNote(ctx context.Context, projectID, note, author string) error {
	if strings.TrimSpace(note) == "" {
		return invalid("a note needs something in it")
	}
	if author == "" {
		author = "runner"
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT OR REPLACE INTO run_notes (project_id, note, author, updated_at)
		 VALUES (?,?,?,?)`,
		projectID, strings.TrimSpace(note), author,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("saving the run note: %w", err)
	}
	return nil
}

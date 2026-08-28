package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Artifacts: the things an agent made that a person wants to look at.
//
// A pipeline that finishes leaves a commit, which answers the question for a
// library and answers nothing for anything with a screen. This is the row; the
// bytes live on disk under their own digest (internal/artifact), and a service
// has no bytes at all, only a port something is listening on.

const (
	ArtifactFile    = "file"
	ArtifactImage   = "image"
	ArtifactService = "service"
)

// Artifact is one produced thing.
type Artifact struct {
	ID        string  `json:"id"`
	ProjectID string  `json:"projectId"`
	TaskID    *string `json:"taskId,omitempty"`
	Role      string  `json:"role,omitempty"`
	Kind      string  `json:"kind"`
	Label     string  `json:"label,omitempty"`

	// A file's identity on disk, and what it is.
	SHA256 string `json:"sha256,omitempty"`
	MIME   string `json:"mime,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Name   string `json:"name,omitempty"`

	// A service's port, and when it stopped being one.
	Port      int        `json:"port,omitempty"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	Pinned    bool      `json:"pinned"`
}

// Live reports whether a service is still worth linking to.
func (a *Artifact) Live() bool { return a.Kind == ArtifactService && a.StoppedAt == nil }

// AddArtifact records one. The caller has already put the bytes on disk, or
// has a port to register.
func (db *DB) AddArtifact(ctx context.Context, a *Artifact) (*Artifact, error) {
	switch a.Kind {
	case ArtifactFile, ArtifactImage:
		if a.SHA256 == "" {
			return nil, invalid("a stored file needs its digest")
		}
	case ArtifactService:
		// A port outside the range is a typo, and one that would otherwise be
		// stored, listed, and fail only when somebody clicked it.
		if a.Port < 1 || a.Port > 65535 {
			return nil, invalid("a service needs the port it is listening on, 1 to 65535")
		}
	default:
		return nil, invalid("unknown artifact kind %q; it is file, image or service", a.Kind)
	}
	if a.ProjectID == "" {
		return nil, invalid("an artifact belongs to a project")
	}

	a.ID = NewID()
	a.CreatedAt = time.Now().UTC()
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO artifacts (id, project_id, task_id, role, kind, label,
		   sha256, mime, bytes, name, port, created_at, pinned)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ProjectID, a.TaskID, a.Role, a.Kind, a.Label,
		a.SHA256, a.MIME, a.Bytes, a.Name, a.Port,
		a.CreatedAt.Format(time.RFC3339Nano), boolInt(a.Pinned)); err != nil {
		return nil, fmt.Errorf("recording the artifact: %w", err)
	}
	return a, nil
}

// GetArtifact reads one.
func (db *DB) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = ?`, id)
	a, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("artifact %s: %w", id, ErrNotFound)
	}
	return a, err
}

// ArtifactsForTask lists what a card produced, newest last so the list reads
// in the order the work happened.
func (db *DB) ArtifactsForTask(ctx context.Context, taskID string) ([]Artifact, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+artifactCols+` FROM artifacts WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("reading artifacts: %w", err)
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// LiveServices are the running services of a project, for the cockpit to link
// to and for shutdown to mark stopped.
func (db *DB) LiveServices(ctx context.Context, projectID string) ([]Artifact, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+artifactCols+` FROM artifacts
		  WHERE project_id = ? AND kind = ? AND stopped_at IS NULL
		  ORDER BY created_at`, projectID, ArtifactService)
	if err != nil {
		return nil, fmt.Errorf("reading services: %w", err)
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// StopServices marks a project's services stopped.
//
// Called when the swarm goes down, because that is when the processes holding
// those ports die: the row outlives them and would otherwise offer a link to
// whatever binds that port next, which is the worst kind of wrong answer.
// Passing an empty project stops every one of them, which is what a daemon
// shutting down means.
func (db *DB) StopServices(ctx context.Context, projectID string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := `UPDATE artifacts SET stopped_at = ? WHERE kind = ? AND stopped_at IS NULL`
	args := []any{now, ArtifactService}
	if projectID != "" {
		q += ` AND project_id = ?`
		args = append(args, projectID)
	}
	res, err := db.sql.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("stopping services: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// SetArtifactPinned keeps an artifact after its task's transcript ages out.
func (db *DB) SetArtifactPinned(ctx context.Context, id string, pinned bool) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE artifacts SET pinned = ? WHERE id = ?`, boolInt(pinned), id)
	if err != nil {
		return fmt.Errorf("pinning artifact %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("artifact %s", id))
}

// UnreferencedBlobs returns digests no row names any more, so the bytes on
// disk can go with them.
//
// Content addressing is what makes this necessary: two rows can share one
// file, so deleting a row is not permission to delete what it pointed at.
func (db *DB) UnreferencedBlobs(ctx context.Context, digests []string) ([]string, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	holes := strings.TrimSuffix(strings.Repeat("?,", len(digests)), ",")
	args := make([]any, len(digests))
	for i, d := range digests {
		args[i] = d
	}
	rows, err := db.read.QueryContext(ctx,
		`SELECT DISTINCT sha256 FROM artifacts WHERE sha256 IN (`+holes+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("checking which artifacts are still referenced: %w", err)
	}
	defer rows.Close()

	referenced := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		referenced[d] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var orphans []string
	for _, d := range digests {
		if !referenced[d] {
			orphans = append(orphans, d)
		}
	}
	return orphans, nil
}

const artifactCols = `id, project_id, task_id, role, kind, label, sha256, mime, bytes, name,
	port, stopped_at, created_at, pinned`

func scanArtifact(s scanner) (*Artifact, error) {
	var (
		a         Artifact
		taskID    sql.NullString
		stoppedAt sql.NullString
		createdAt string
		pinned    int
	)
	if err := s.Scan(&a.ID, &a.ProjectID, &taskID, &a.Role, &a.Kind, &a.Label,
		&a.SHA256, &a.MIME, &a.Bytes, &a.Name, &a.Port, &stoppedAt, &createdAt, &pinned); err != nil {
		return nil, err
	}
	if taskID.Valid {
		a.TaskID = &taskID.String
	}
	if stoppedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, stoppedAt.String); err == nil {
			a.StoppedAt = &t
		}
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	a.Pinned = pinned != 0
	return &a, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

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

// Who started a service, which decides what stops it. An agent's dev server
// is a child of the swarm; a preview the daemon started outlives it, because
// the reason to run one is to click around after the pipeline finished.
const (
	OwnerAgent  = "agent"
	OwnerDaemon = "daemon"
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
	// Owner is agent or daemon; see the constants above.
	Owner string `json:"owner,omitempty"`

	// ChatID is the conversation a file was attached to. Empty for everything
	// an agent produced, which is what the rest of this table is.
	ChatID string `json:"chatId,omitempty"`

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
	if a.Owner == "" {
		a.Owner = OwnerAgent
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO artifacts (id, project_id, task_id, role, kind, label,
		   sha256, mime, bytes, name, port, created_at, pinned, owner, chat_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ProjectID, a.TaskID, a.Role, a.Kind, a.Label,
		a.SHA256, a.MIME, a.Bytes, a.Name, a.Port,
		a.CreatedAt.Format(time.RFC3339Nano), boolInt(a.Pinned), a.Owner,
		nullable(a.ChatID)); err != nil {
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

// ArtifactsForChat lists what was attached to one conversation, oldest first so
// it reads in the order it was said.
func (db *DB) ArtifactsForChat(ctx context.Context, chatID string) ([]Artifact, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+artifactCols+` FROM artifacts WHERE chat_id = ? ORDER BY created_at, id`, chatID)
	if err != nil {
		return nil, fmt.Errorf("reading a conversation's files: %w", err)
	}
	defer rows.Close()

	out := []Artifact{}
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

// StopServices marks services stopped.
//
// Called when the processes holding those ports die: the row outlives them and
// would otherwise offer a link to whatever binds that port next, which is the
// worst kind of wrong answer. Passing an empty project means every project,
// which is what a daemon shutting down means.
//
// owner selects whose services: the swarm going down takes the agents' dev
// servers with it and must leave a preview the daemon is still running alone.
// An empty owner means both, for shutdown.
func (db *DB) StopServices(ctx context.Context, projectID, owner string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := `UPDATE artifacts SET stopped_at = ? WHERE kind = ? AND stopped_at IS NULL`
	args := []any{now, ArtifactService}
	if projectID != "" {
		q += ` AND project_id = ?`
		args = append(args, projectID)
	}
	if owner != "" {
		q += ` AND owner = ?`
		args = append(args, owner)
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

const artifactCols = `id, project_id, task_id, role, kind, label, sha256, mime, bytes, name,
	port, stopped_at, created_at, pinned, owner, chat_id`

func scanArtifact(s scanner) (*Artifact, error) {
	var (
		a         Artifact
		taskID    sql.NullString
		stoppedAt sql.NullString
		chatID    sql.NullString
		createdAt string
		pinned    int
	)
	if err := s.Scan(&a.ID, &a.ProjectID, &taskID, &a.Role, &a.Kind, &a.Label,
		&a.SHA256, &a.MIME, &a.Bytes, &a.Name, &a.Port, &stoppedAt, &createdAt, &pinned,
		&a.Owner, &chatID); err != nil {
		return nil, err
	}
	a.ChatID = chatID.String
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

// DeleteChatAttachments removes what was attached to a project's conversation,
// and reports the digests nothing names any more.
//
// Called when a chat is ended, which is the one moment a conversation's files
// stop being wanted: they are exempt from the sweep precisely because ending it
// is a decision rather than an age.
func (db *DB) DeleteChatAttachments(ctx context.Context, projectID string) ([]string, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("clearing attachments: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, sha256 FROM artifacts
		  WHERE project_id = ? AND role = ? AND task_id IS NULL`, projectID, OperatorRole)
	if err != nil {
		return nil, fmt.Errorf("clearing attachments: %w", err)
	}
	var ids, digests []string
	for rows.Next() {
		var id, digest string
		if err := rows.Scan(&id, &digest); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if digest != "" {
			digests = append(digests, digest)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id IN (`+holes(len(ids))+`)`,
		anySlice(ids)...); err != nil {
		return nil, fmt.Errorf("clearing attachments: %w", err)
	}

	// The same bytes can be named by another row -- the same screenshot
	// attached to two projects, or produced by an agent -- so only the digests
	// nothing points at are handed back for deletion.
	orphans, err := orphanedDigests(ctx, tx, digests)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return orphans, nil
}

// orphanedDigests is which of these the artifacts table no longer names.
func orphanedDigests(ctx context.Context, tx *sql.Tx, digests []string) ([]string, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT sha256 FROM artifacts WHERE sha256 IN (`+holes(len(digests))+`)`,
		anySlice(digests)...)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		kept[d] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, d := range digests {
		if !kept[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

// PruneArtifacts drops artifacts of tasks that have aged out, and reports the
// digests whose bytes are now unreferenced.
//
// The same window as events (§13.5), and the same exemptions with one more:
// a pinned artifact survives, and so does one whose task is pinned. The card
// worth keeping is usually the one that went wrong, and its screenshot is
// most of why it is worth keeping.
//
// Services are dropped on age like anything else; their rows carry no bytes,
// and one still marked live after the retention window belongs to a daemon
// that stopped without saying so.
//
// The digests come back rather than the files being deleted here, because two
// rows can name the same bytes: what is safe to remove is a question about the
// whole table, and the caller owns the directory.
func (db *DB) PruneArtifacts(ctx context.Context, before time.Time) (int, []string, error) {
	cut := before.UTC().Format(time.RFC3339Nano)

	// Read the digests first: after the delete there is nothing to read them
	// from, and doing it in one transaction keeps a concurrent insert from
	// slipping between the two.
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("pruning artifacts: %w", err)
	}
	defer tx.Rollback()

	// A file somebody attached to a conversation is exempt, the way the
	// conversation itself is. Swept, a chat kept its words and lost its
	// pictures: "what is wrong with this?" above a gap, with the answer
	// underneath discussing something no longer on screen.
	const doomed = `SELECT id, sha256 FROM artifacts
	                 WHERE created_at < ? AND pinned = 0
	                   AND role <> ?
	                   AND (task_id IS NULL
	                        OR NOT EXISTS (SELECT 1 FROM tasks t
	                                        WHERE t.id = artifacts.task_id AND t.pinned = 1))`
	rows, err := tx.QueryContext(ctx, doomed, cut, OperatorRole)
	if err != nil {
		return 0, nil, fmt.Errorf("pruning artifacts: %w", err)
	}
	var ids, digests []string
	for rows.Next() {
		var id, digest string
		if err := rows.Scan(&id, &digest); err != nil {
			rows.Close()
			return 0, nil, err
		}
		ids = append(ids, id)
		if digest != "" {
			digests = append(digests, digest)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(ids) == 0 {
		return 0, nil, nil
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id IN (`+holes(len(ids))+`)`,
		anySlice(ids)...); err != nil {
		return 0, nil, fmt.Errorf("pruning artifacts: %w", err)
	}

	// Which of those digests nothing names any more, asked inside the same
	// transaction so a row added meanwhile is seen.
	kept := map[string]bool{}
	if len(digests) > 0 {
		q, err := tx.QueryContext(ctx,
			`SELECT DISTINCT sha256 FROM artifacts WHERE sha256 IN (`+holes(len(digests))+`)`,
			anySlice(digests)...)
		if err != nil {
			return 0, nil, err
		}
		for q.Next() {
			var d string
			if err := q.Scan(&d); err != nil {
				q.Close()
				return 0, nil, err
			}
			kept[d] = true
		}
		q.Close()
		if err := q.Err(); err != nil {
			return 0, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}

	var orphans []string
	seen := map[string]bool{}
	for _, d := range digests {
		if !kept[d] && !seen[d] {
			seen[d] = true
			orphans = append(orphans, d)
		}
	}
	return len(ids), orphans, nil
}

func holes(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

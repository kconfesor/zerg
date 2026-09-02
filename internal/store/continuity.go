package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What has to be remembered for a restart to be invisible.
//
// Two separate facts, written by different actors and cleared on different
// events, which is why they are not one row. The operator says whether a
// project should be running at all; a harness says which conversation its agent
// is holding. A restart is invisible only when both are answered, and each is
// useful without the other: a swarm that comes back up with cold agents still
// finishes the queue, and a resumable session is worth keeping for the crash
// restarts that happen inside a single run.

// ── the operator's intent ─────────────────────────────────────────────────

// RequestStart records that the operator wants this project running, and keeps
// recording the first time they said so.
//
// The timestamp is not refreshed on a re-Start, because the question it exists
// to answer is "since when has this been wanted", and a daemon that restarts
// nightly would otherwise rewrite that to "since this morning" forever.
func (db *DB) RequestStart(ctx context.Context, projectID string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE projects SET start_requested_at = COALESCE(start_requested_at, ?) WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), projectID)
	if err != nil {
		return fmt.Errorf("recording that %s should be running: %w", projectID, err)
	}
	return mustAffect(res, fmt.Sprintf("project %s", projectID))
}

// WithdrawStart records that the operator no longer wants this project running.
//
// Only an operator calls this. A daemon shutting down deliberately does not:
// the whole point of the column is that it survives the process, so clearing it
// on the way out would leave nothing to come back to.
func (db *DB) WithdrawStart(ctx context.Context, projectID string) error {
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE projects SET start_requested_at = NULL WHERE id = ?`, projectID); err != nil {
		return fmt.Errorf("recording that %s should stop: %w", projectID, err)
	}
	return nil
}

// ProjectsWantingStart lists the projects the operator left running, oldest
// request first so a restart brings them up in the order they were asked for.
func (db *DB) ProjectsWantingStart(ctx context.Context) ([]string, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id FROM projects WHERE start_requested_at IS NOT NULL
		  ORDER BY start_requested_at`)
	if err != nil {
		return nil, fmt.Errorf("listing projects to resume: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ── the agent's own memory ────────────────────────────────────────────────

// RoleSession is the harness conversation one role was last holding.
type RoleSession struct {
	ProjectID string    `json:"projectId"`
	Role      string    `json:"role"`
	Harness   string    `json:"harness"`
	SessionID string    `json:"sessionId"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RoleSessionFor returns the session a role may resume, or ErrNotFound.
//
// harness and fingerprint are part of the lookup rather than checked by the
// caller, because a mismatch is not an error to report: it means this role is
// configured differently from the one that owned that conversation, and the
// only correct answer is a cold session. Returning the row and trusting every
// caller to compare is how a resume across a prompt edit gets shipped.
func (db *DB) RoleSessionFor(ctx context.Context, projectID, role, harness, fingerprint string) (*RoleSession, error) {
	s := &RoleSession{ProjectID: projectID, Role: role, Harness: harness}
	var updated string
	err := db.read.QueryRowContext(ctx,
		`SELECT session_id, updated_at FROM role_sessions
		  WHERE project_id = ? AND role = ? AND harness = ? AND fingerprint = ?`,
		projectID, role, harness, fingerprint).Scan(&s.SessionID, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no resumable session for %s/%s: %w", projectID, role, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the session for %s/%s: %w", projectID, role, err)
	}
	s.UpdatedAt, _ = parseStored(updated)
	return s, nil
}

// SaveRoleSession records what the harness said it is running.
//
// One row per role, replaced rather than appended: this is the conversation to
// resume, not a history of conversations. A harness that forks to a new id
// mid-run overwrites the old one, which is what makes the stored value follow
// what is actually being written to.
func (db *DB) SaveRoleSession(ctx context.Context, projectID, role, harness, sessionID, fingerprint string) error {
	if sessionID == "" {
		return invalid("a session needs an id to be resumable")
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO role_sessions (project_id, role, harness, session_id, fingerprint, updated_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(project_id, role) DO UPDATE SET
		     harness = excluded.harness,
		     session_id = excluded.session_id,
		     fingerprint = excluded.fingerprint,
		     updated_at = excluded.updated_at`,
		projectID, role, harness, sessionID, fingerprint,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("saving the session for %s/%s: %w", projectID, role, err)
	}
	return nil
}

// ListRoleSessions is every conversation on record for a project, newest first.
//
// Unfiltered by fingerprint on purpose, unlike RoleSessionFor: this answers
// "what was this project holding", which is a question about the last run, and
// whether a given session is still resumable is a question about the next one.
func (db *DB) ListRoleSessions(ctx context.Context, projectID string) ([]RoleSession, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT role, harness, session_id, updated_at FROM role_sessions
		  WHERE project_id = ? ORDER BY updated_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing sessions for %s: %w", projectID, err)
	}
	defer rows.Close()

	var out []RoleSession
	for rows.Next() {
		s := RoleSession{ProjectID: projectID}
		var updated string
		if err := rows.Scan(&s.Role, &s.Harness, &s.SessionID, &updated); err != nil {
			return nil, err
		}
		s.UpdatedAt, _ = parseStored(updated)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ForgetRoleSessions drops every stored session for a project, so its next
// start is a cold one.
//
// Called when the operator stops a swarm, and not when the daemon goes down.
// The distinction is the same one start_requested_at draws: a process ending is
// not a decision, and a person pressing Stop is. Resuming a week-old
// conversation about a finished task on the next Start would be continuity of
// the wrong thing.
func (db *DB) ForgetRoleSessions(ctx context.Context, projectID string) (int, error) {
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM role_sessions WHERE project_id = ?`, projectID)
	if err != nil {
		return 0, fmt.Errorf("forgetting sessions for %s: %w", projectID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

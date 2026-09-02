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
//
// It reports whether this call is the one that set it. A caller that has to
// undo a half-finished start needs to know the difference: withdrawing an
// intent it merely found already there would cancel the resume of a project
// that was running before this attempt, which is the case Resume is made of.
func (db *DB) RequestStart(ctx context.Context, projectID string) (recorded bool, err error) {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE projects SET start_requested_at = ? WHERE id = ? AND start_requested_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), projectID)
	if err != nil {
		return false, fmt.Errorf("recording that %s should be running: %w", projectID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("recording that %s should be running: %w", projectID, err)
	}
	if n > 0 {
		return true, nil
	}
	// No row changed for one of two reasons, and only one of them is a
	// problem: the intent was already recorded, or there is no such project.
	var exists int
	if err := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE id = ?`, projectID).Scan(&exists); err != nil {
		return false, fmt.Errorf("recording that %s should be running: %w", projectID, err)
	}
	if exists == 0 {
		return false, fmt.Errorf("project %s: %w", projectID, ErrNotFound)
	}
	return false, nil
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

// ForgetContinuity is the whole of an operator's Stop, in one transaction: the
// project stops wanting to be running, and its roles forget the conversations
// they were holding.
//
// One transaction because half of it is worse than neither. The two writes were
// separate and their failures were logged rather than returned, so a Stop that
// cleared the sessions and failed to clear the intent reported success and then
// started the project again on the next daemon boot; the other order reported
// success and brought back a week-old conversation about a task that had since
// been finished. Either way the operator was told the project was stopped,
// which is the one thing that has to be true.
//
// The agent processes go with them, and only here. A row in agent_processes
// that survived the boot sweep is a worktree nothing could confirm is free, and
// it refuses a start for exactly that reason; an operator pressing Stop is the
// one statement that clears it, because they are the only party in a position
// to look at the machine and say so.
//
// The count is the sessions dropped, for the log line that says what was lost.
func (db *DB) ForgetContinuity(ctx context.Context, projectID string) (sessions int, err error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("forgetting what %s was doing: %w", projectID, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE projects SET start_requested_at = NULL WHERE id = ?`, projectID); err != nil {
		return 0, fmt.Errorf("recording that %s should stop: %w", projectID, err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM role_sessions WHERE project_id = ?`, projectID)
	if err != nil {
		return 0, fmt.Errorf("forgetting sessions for %s: %w", projectID, err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_processes WHERE project_id = ?`, projectID); err != nil {
		return 0, fmt.Errorf("forgetting the agent processes for %s: %w", projectID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("forgetting what %s was doing: %w", projectID, err)
	}
	return int(n), nil
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

// ── the processes the last run left behind ────────────────────────────────

// AgentProcess is one agent process group, as it was when it was spawned.
//
// PID is the harness the daemon started and PGID is the group it leads, which
// is what has to be signalled: every bash tool call an agent makes is a child
// in that group, and killing only the leader leaves them holding the worktree.
// Identity is what makes PID safe to act on at all — see schema_037.
type AgentProcess struct {
	ProjectID string    `json:"projectId"`
	Role      string    `json:"role"`
	PID       int       `json:"pid"`
	PGID      int       `json:"pgid"`
	Identity  string    `json:"identity"`
	Worktree  string    `json:"worktree"`
	StartedAt time.Time `json:"startedAt"`
}

// RecordAgentProcess writes down a process the daemon has just spawned.
//
// One row per role, replaced: a role respawning after a crash is the same role
// in the same worktree, and the row has to name the process that is there now.
func (db *DB) RecordAgentProcess(ctx context.Context, p AgentProcess) error {
	if p.PID <= 0 || p.PGID <= 0 {
		return invalid("an agent process needs a pid and a process group to be stoppable")
	}
	if p.Identity == "" {
		return invalid("an agent process needs an identity, or its pid cannot be checked later")
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO agent_processes (project_id, role, pid, pgid, identity, worktree, started_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(project_id, role) DO UPDATE SET
		     pid = excluded.pid,
		     pgid = excluded.pgid,
		     identity = excluded.identity,
		     worktree = excluded.worktree,
		     started_at = excluded.started_at`,
		p.ProjectID, p.Role, p.PID, p.PGID, p.Identity, p.Worktree,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("recording the agent process for %s/%s: %w", p.ProjectID, p.Role, err)
	}
	return nil
}

// ForgetAgentProcess drops the row for a process that has been seen to exit.
//
// Only the row matching pid is removed. A respawn overwrites the row while the
// goroutine that watched the previous process is still on its way to this call,
// and a delete that did not check would erase the live process's row and leave
// the next daemon with nothing to clean up.
func (db *DB) ForgetAgentProcess(ctx context.Context, projectID, role string, pid int) error {
	if _, err := db.sql.ExecContext(ctx,
		`DELETE FROM agent_processes WHERE project_id = ? AND role = ? AND pid = ?`,
		projectID, role, pid); err != nil {
		return fmt.Errorf("forgetting the agent process for %s/%s: %w", projectID, role, err)
	}
	return nil
}

// ListAgentProcesses is every agent process on record, oldest first.
//
// Read at boot, where every row is by definition a process the previous daemon
// did not stop: a daemon that exits normally removes its own rows.
func (db *DB) ListAgentProcesses(ctx context.Context) ([]AgentProcess, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT project_id, role, pid, pgid, identity, worktree, started_at
		   FROM agent_processes ORDER BY started_at`)
	if err != nil {
		return nil, fmt.Errorf("listing the agent processes from the previous run: %w", err)
	}
	defer rows.Close()

	var out []AgentProcess
	for rows.Next() {
		var p AgentProcess
		var started string
		if err := rows.Scan(&p.ProjectID, &p.Role, &p.PID, &p.PGID,
			&p.Identity, &p.Worktree, &started); err != nil {
			return nil, err
		}
		p.StartedAt, _ = parseStored(started)
		out = append(out, p)
	}
	return out, rows.Err()
}

// AgentProcessesFor is what is still on record for one project.
//
// Read before every start. After the boot sweep, a row here is a worktree that
// nothing could confirm is free, and putting a second agent into it is the
// failure the sweep exists to prevent.
func (db *DB) AgentProcessesFor(ctx context.Context, projectID string) ([]AgentProcess, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT role, pid, pgid, identity, worktree, started_at
		   FROM agent_processes WHERE project_id = ? ORDER BY role`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing the agents on record for %s: %w", projectID, err)
	}
	defer rows.Close()

	var out []AgentProcess
	for rows.Next() {
		p := AgentProcess{ProjectID: projectID}
		var started string
		if err := rows.Scan(&p.Role, &p.PID, &p.PGID, &p.Identity, &p.Worktree, &started); err != nil {
			return nil, err
		}
		p.StartedAt, _ = parseStored(started)
		out = append(out, p)
	}
	return out, rows.Err()
}

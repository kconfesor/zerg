package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Where a project's work can be run or sent.
//
// A target is a name and a command. zerg knows nothing about Docker, Vercel or
// Fly, and adding a provider would mean an SDK, a credential shape and a
// release cadence that are not this project's: the command is the integration
// point, and the operator can read it.

const (
	// TargetLocal runs on this machine and is proxied for a person to click.
	TargetLocal = "local"
	// TargetRemote sends the work somewhere else; issue #9's second half.
	TargetRemote = "remote"
)

// DeployTarget is one place work can go.
type DeployTarget struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Command   string `json:"command"`
	// Cwd is relative to the checkout; empty is its root.
	Cwd string `json:"cwd,omitempty"`
	// StopCommand undoes what Command started, when killing the process group
	// is not enough: `docker compose up` interrupted leaves its containers
	// exited, and only the command knows they exist.
	StopCommand string `json:"stopCommand,omitempty"`
	// CopyFiles are paths git does not track, brought from the operator's
	// checkout into the preview's. One per line; .env is what this is for.
	CopyFiles string `json:"copyFiles,omitempty"`
	// ReadySecs bounds the wait for the port to answer. An image that has to
	// be pulled is minutes and a vite preview is seconds; neither should be
	// the other's timeout.
	ReadySecs int       `json:"readySecs"`
	CreatedAt time.Time `json:"createdAt"`
}

// SaveDeployTarget creates or updates one.
func (db *DB) SaveDeployTarget(ctx context.Context, t *DeployTarget) (*DeployTarget, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.Command = strings.TrimSpace(t.Command)
	if t.Name == "" {
		return nil, invalid("a target needs a name")
	}
	if t.Command == "" {
		return nil, invalid("a target needs a command to run")
	}
	if t.Kind != TargetLocal && t.Kind != TargetRemote {
		return nil, invalid("a target is local or remote, not %q", t.Kind)
	}
	if t.ReadySecs <= 0 {
		t.ReadySecs = 120
	}

	if t.ID == "" {
		t.ID = NewID()
		t.CreatedAt = time.Now().UTC()
		_, err := db.sql.ExecContext(ctx,
			`INSERT INTO deploy_targets (id, project_id, name, kind, command, cwd, stop_command,
			   copy_files, ready_secs, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.ProjectID, t.Name, t.Kind, t.Command, t.Cwd, t.StopCommand, t.CopyFiles,
			t.ReadySecs, t.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil, invalid("this project already has a target called %q", t.Name)
			}
			return nil, fmt.Errorf("saving the target: %w", err)
		}
		return t, nil
	}

	res, err := db.sql.ExecContext(ctx,
		`UPDATE deploy_targets SET name = ?, kind = ?, command = ?, cwd = ?, stop_command = ?,
		   copy_files = ?, ready_secs = ?
		  WHERE id = ?`,
		t.Name, t.Kind, t.Command, t.Cwd, t.StopCommand, t.CopyFiles, t.ReadySecs, t.ID)
	if err != nil {
		return nil, fmt.Errorf("saving the target: %w", err)
	}
	if err := mustAffect(res, fmt.Sprintf("target %s", t.ID)); err != nil {
		return nil, err
	}
	return db.GetDeployTarget(ctx, t.ID)
}

// GetDeployTarget reads one.
func (db *DB) GetDeployTarget(ctx context.Context, id string) (*DeployTarget, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT id, project_id, name, kind, command, cwd, stop_command, copy_files, ready_secs, created_at
		   FROM deploy_targets WHERE id = ?`, id)
	t, err := scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("target %s: %w", id, ErrNotFound)
	}
	return t, err
}

// DeployTargets lists a project's, oldest first.
func (db *DB) DeployTargets(ctx context.Context, projectID string) ([]DeployTarget, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id, project_id, name, kind, command, cwd, stop_command, copy_files, ready_secs, created_at
		   FROM deploy_targets WHERE project_id = ? ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("reading targets: %w", err)
	}
	defer rows.Close()

	var out []DeployTarget
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// DeleteDeployTarget removes one.
func (db *DB) DeleteDeployTarget(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM deploy_targets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting the target: %w", err)
	}
	return mustAffect(res, fmt.Sprintf("target %s", id))
}

func scanTarget(s scanner) (*DeployTarget, error) {
	var (
		t         DeployTarget
		createdAt string
	)
	if err := s.Scan(&t.ID, &t.ProjectID, &t.Name, &t.Kind, &t.Command, &t.Cwd,
		&t.StopCommand, &t.CopyFiles, &t.ReadySecs, &createdAt); err != nil {
		return nil, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	return &t, nil
}

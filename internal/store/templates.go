package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a lookup by id finds nothing.
var ErrNotFound = errors.New("not found")

const templateCols = `id, name, harness, model, args, receive, batch_max_items,
	batch_max_age_sec, prompt, gate, builtin, created_at, updated_at`

// CreateTemplate adds a role to the library.
func (db *DB) CreateTemplate(ctx context.Context, t *RoleTemplate) (*RoleTemplate, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	t.ID = NewID()
	t.CreatedAt, t.UpdatedAt = now, now

	args, err := marshalArgs(t.Args)
	if err != nil {
		return nil, err
	}

	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO role_templates (`+templateCols+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Harness, t.Model, args, t.Receive, t.BatchMaxItems,
		t.BatchMaxAgeSec, t.Prompt, t.Gate, t.Builtin,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("creating role %q: %w", t.Name, err)
	}
	return t, nil
}

// ListTemplates returns the library in name order.
func (db *DB) ListTemplates(ctx context.Context) ([]RoleTemplate, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+templateCols+` FROM role_templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	defer rows.Close()

	var out []RoleTemplate
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetTemplate looks a library entry up by id.
func (db *DB) GetTemplate(ctx context.Context, id string) (*RoleTemplate, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+templateCols+` FROM role_templates WHERE id = ?`, id)
	t, err := scanTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("role %s: %w", id, ErrNotFound)
	}
	return t, err
}

// GetTemplateByName looks a library entry up by its unique name.
func (db *DB) GetTemplateByName(ctx context.Context, name string) (*RoleTemplate, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+templateCols+` FROM role_templates WHERE name = ?`, name)
	t, err := scanTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("role %q: %w", name, ErrNotFound)
	}
	return t, err
}

// UpdateTemplate rewrites a library entry. Editing a template changes that
// role in every project that selected it — which is the point of a library,
// and why the UI distinguishes this from setting a project override.
func (db *DB) UpdateTemplate(ctx context.Context, t *RoleTemplate) error {
	if err := t.Validate(); err != nil {
		return err
	}
	args, err := marshalArgs(t.Args)
	if err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()

	res, err := db.sql.ExecContext(ctx,
		`UPDATE role_templates SET name=?, harness=?, model=?, args=?, receive=?,
		   batch_max_items=?, batch_max_age_sec=?, prompt=?, gate=?, updated_at=?
		 WHERE id=?`,
		t.Name, t.Harness, t.Model, args, t.Receive, t.BatchMaxItems,
		t.BatchMaxAgeSec, t.Prompt, t.Gate, t.UpdatedAt.Format(time.RFC3339Nano), t.ID)
	if err != nil {
		return fmt.Errorf("updating role %q: %w", t.Name, err)
	}
	return mustAffect(res, fmt.Sprintf("role %s", t.ID))
}

// DeleteTemplate removes a library entry, and with it that role's membership in
// every project (ON DELETE CASCADE on project_roles).
func (db *DB) DeleteTemplate(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM role_templates WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting role %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("role %s", id))
}

// ── helpers ───────────────────────────────────────────────────────────────

type scanner interface{ Scan(dest ...any) error }

func scanTemplate(s scanner) (*RoleTemplate, error) {
	var (
		t          RoleTemplate
		args       string
		created    string
		updated    string
		builtinInt int
	)
	if err := s.Scan(&t.ID, &t.Name, &t.Harness, &t.Model, &args, &t.Receive,
		&t.BatchMaxItems, &t.BatchMaxAgeSec, &t.Prompt, &t.Gate, &builtinInt,
		&created, &updated); err != nil {
		return nil, err
	}
	t.Builtin = builtinInt != 0
	var err error
	if t.Args, err = unmarshalArgs(args); err != nil {
		return nil, err
	}
	if t.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("role %s has an unreadable created_at: %w", t.ID, err)
	}
	if t.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return nil, fmt.Errorf("role %s has an unreadable updated_at: %w", t.ID, err)
	}
	return &t, nil
}

// Args are stored as a JSON array rather than a joined string: a flag can
// legitimately contain a space, and re-splitting one is how quoting bugs start.
func marshalArgs(args []string) (string, error) {
	if args == nil {
		args = []string{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encoding args: %w", err)
	}
	return string(b), nil
}

func unmarshalArgs(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decoding args %q: %w", s, err)
	}
	return out, nil
}

func mustAffect(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

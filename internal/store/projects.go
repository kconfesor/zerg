package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CreateProject registers a repository. path is stored absolute so the same
// repo reached by two relative paths is one project, not two.
func (db *DB) CreateProject(ctx context.Context, path, name, baseBranch string) (*Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", path, err)
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	p := &Project{
		ID:         NewID(),
		Path:       abs,
		Name:       name,
		BaseBranch: baseBranch,
		CreatedAt:  time.Now().UTC(),
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO projects (id, path, name, base_branch, created_at) VALUES (?,?,?,?,?)`,
		p.ID, p.Path, p.Name, p.BaseBranch, p.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("creating project %s: %w", abs, err)
	}
	return p, nil
}

// ListProjects returns projects most-recently-opened first, so the picker
// surfaces what you were last working on.
func (db *DB) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, path, name, base_branch, created_at, last_opened_at
		 FROM projects ORDER BY COALESCE(last_opened_at, created_at) DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetProject looks a project up by id.
func (db *DB) GetProject(ctx context.Context, id string) (*Project, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT id, path, name, base_branch, created_at, last_opened_at FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project %s: %w", id, ErrNotFound)
	}
	return p, err
}

// TouchProject records that a project was opened.
func (db *DB) TouchProject(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE projects SET last_opened_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("touching project %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("project %s", id))
}

// DeleteProject forgets a project. Its team membership goes with it via
// ON DELETE CASCADE; files on disk are never touched.
func (db *DB) DeleteProject(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting project %s: %w", id, err)
	}
	return mustAffect(res, fmt.Sprintf("project %s", id))
}

// ── team ──────────────────────────────────────────────────────────────────

// SetTeam replaces a project's pipeline in one transaction. Callers send the
// whole desired team rather than diffing it, so a reorder and a selection
// change are the same operation and cannot half-apply.
func (db *DB) SetTeam(ctx context.Context, projectID string, roles []ProjectRole) error {
	if _, err := db.GetProject(ctx, projectID); err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, r := range roles {
		if seen[r.TemplateID] {
			return invalid("role %s appears twice in the team; each role joins a pipeline once", r.TemplateID)
		}
		seen[r.TemplateID] = true
		if _, err := db.GetTemplate(ctx, r.TemplateID); err != nil {
			return err
		}
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning team update: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM project_roles WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("clearing team: %w", err)
	}

	// Positions are normalised to 0..n-1 in the order given, so the caller can
	// send whatever the drag produced without worrying about gaps or ties.
	for i, r := range roles {
		args, err := marshalOverrideArgs(r.ArgsOverride)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO project_roles
			   (project_id, template_id, position, enabled, model_override, args_override)
			 VALUES (?,?,?,?,?,?)`,
			projectID, r.TemplateID, i, r.Enabled, r.ModelOverride, args); err != nil {
			return fmt.Errorf("adding role to team: %w", err)
		}
	}
	return tx.Commit()
}

// SelectDefaultTeam gives a new project the starting pipeline: coder then
// reviewer, enough to be useful without a configuration session.
func (db *DB) SelectDefaultTeam(ctx context.Context, projectID string) error {
	var team []ProjectRole
	for _, name := range DefaultProjectRoles {
		tpl, err := db.GetTemplateByName(ctx, name)
		if err != nil {
			return fmt.Errorf("default role %q is missing from the library: %w", name, err)
		}
		team = append(team, ProjectRole{TemplateID: tpl.ID, Enabled: true})
	}
	return db.SetTeam(ctx, projectID, team)
}

// ResolveTeam returns the project's pipeline in order with overrides applied —
// what a cerebrate is actually asked to run.
//
// Terminal is computed here rather than stored: it is the last *enabled* role,
// so disabling the final role promotes the one before it without an edit
// anywhere else. The predecessor decided terminality from config-file line
// order, which meant reordering a file silently relocated the end of the
// pipeline.
func (db *DB) ResolveTeam(ctx context.Context, projectID string) ([]ResolvedRole, error) {
	if _, err := db.GetProject(ctx, projectID); err != nil {
		return nil, err
	}

	rows, err := db.sql.QueryContext(ctx,
		`SELECT pr.position, pr.enabled, pr.model_override, pr.args_override, `+
			prefixed(templateCols, "t")+`
		 FROM project_roles pr
		 JOIN role_templates t ON t.id = pr.template_id
		 WHERE pr.project_id = ?
		 ORDER BY pr.position`, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolving team: %w", err)
	}
	defer rows.Close()

	var out []ResolvedRole
	for rows.Next() {
		var (
			position      int
			enabledInt    int
			modelOverride sql.NullString
			argsOverride  sql.NullString
			tplArgs       string
			created       string
			updated       string
			builtinInt    int
			r             ResolvedRole
		)
		if err := rows.Scan(&position, &enabledInt, &modelOverride, &argsOverride,
			&r.ID, &r.Name, &r.Harness, &r.Model, &tplArgs, &r.Receive,
			&r.BatchMaxItems, &r.BatchMaxAgeSec, &r.Prompt, &r.Gate, &builtinInt,
			&created, &updated); err != nil {
			return nil, fmt.Errorf("scanning team row: %w", err)
		}

		r.Position = position
		r.Enabled = enabledInt != 0
		r.Builtin = builtinInt != 0
		if r.Args, err = unmarshalArgs(tplArgs); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("role %s has an unreadable created_at: %w", r.ID, err)
		}
		if r.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, fmt.Errorf("role %s has an unreadable updated_at: %w", r.ID, err)
		}

		if modelOverride.Valid && modelOverride.String != r.Model {
			r.Model = modelOverride.String
			r.Overridden = true
		}
		if argsOverride.Valid {
			args, err := unmarshalArgs(argsOverride.String)
			if err != nil {
				return nil, err
			}
			if !slices.Equal(args, r.Args) {
				r.Args = args
				r.Overridden = true
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Enabled {
			out[i].Terminal = true
			break
		}
	}
	return out, nil
}

// ── helpers ───────────────────────────────────────────────────────────────

func scanProject(s scanner) (*Project, error) {
	var (
		p          Project
		created    string
		lastOpened sql.NullString
	)
	if err := s.Scan(&p.ID, &p.Path, &p.Name, &p.BaseBranch, &created, &lastOpened); err != nil {
		return nil, err
	}
	var err error
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("project %s has an unreadable created_at: %w", p.ID, err)
	}
	if lastOpened.Valid {
		t, err := time.Parse(time.RFC3339Nano, lastOpened.String)
		if err != nil {
			return nil, fmt.Errorf("project %s has an unreadable last_opened_at: %w", p.ID, err)
		}
		p.LastOpenedAt = &t
	}
	return &p, nil
}

// marshalOverrideArgs keeps nil distinct from empty: nil means "no override",
// while an empty slice means "this project runs this role with no args at all".
func marshalOverrideArgs(args []string) (any, error) {
	if args == nil {
		return nil, nil
	}
	s, err := marshalArgs(args)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// prefixed qualifies a column list with a table alias, so templateCols can be
// reused in a join without spelling every column out a second time.
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

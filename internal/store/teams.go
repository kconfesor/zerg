package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const DefaultTeamPresetID = "builtin-default-team"

const roleOverrideCols = `harness_override, model_override, args_override, receive_override,
	batch_max_items_override, batch_max_age_sec_override, prompt_override, gate_override`

// settingDefaultTeamSeeded records that the built-in team has been filled once.
// Its absence, not the preset's emptiness, is what asks for seeding.
const settingDefaultTeamSeeded = "default_team_seeded"

// settingExists reports whether a settings key has been written.
func (db *DB) settingExists(ctx context.Context, key string) (bool, error) {
	var n int
	if err := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, key).Scan(&n); err != nil {
		return false, fmt.Errorf("reading setting %s: %w", key, err)
	}
	return n > 0, nil
}

func (db *DB) EnsureDefaultTeamPreset(ctx context.Context) error {
	// Keyed on whether the preset has ever been filled, not on whether it is
	// empty right now.
	//
	// Emptiness is a state a person can choose. Seeding on it meant a Default
	// team someone had deliberately cleared came back on the next restart,
	// which is the exact thing cmd/zerg/main.go promises does not happen:
	// "Seeding is idempotent and never clobbers an edited role ...
	// configuration lives in the database precisely so that a restart does not
	// overwrite what the user changed." The role seeding gets this right by
	// keying on presence-by-name; this now keys on a mark that survives the
	// roles being removed.
	seeded, err := db.settingExists(ctx, settingDefaultTeamSeeded)
	if err != nil {
		return err
	}
	if seeded {
		return nil
	}
	roles := make([]TeamPresetRole, 0, len(DefaultProjectRoles))
	for _, name := range DefaultProjectRoles {
		t, err := db.GetTemplateByName(ctx, name)
		if err != nil {
			return fmt.Errorf("default role %q is missing from the library: %w", name, err)
		}
		roles = append(roles, TeamPresetRole{TemplateID: t.ID, Enabled: true})
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertPresetRoles(ctx, tx, DefaultTeamPresetID, roles); err != nil {
		return err
	}
	// In the same transaction as the roles, so a failure part-way does not
	// leave the mark claiming work that did not happen.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingDefaultTeamSeeded, "1"); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) CreateTeamPreset(ctx context.Context, p *TeamPreset) (*TeamPreset, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return nil, invalid("a team preset needs a name")
	}
	if err := db.validatePresetRoles(ctx, p.Roles); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	p.ID, p.CreatedAt, p.UpdatedAt = NewID(), now, now
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO team_presets (id,name,builtin,created_at,updated_at) VALUES (?,?,?,?,?)`,
		p.ID, p.Name, p.Builtin, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("creating team preset %q: %w", p.Name, err)
	}
	if err := insertPresetRoles(ctx, tx, p.ID, p.Roles); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetTeamPreset(ctx, p.ID)
}

func (db *DB) UpdateTeamPreset(ctx context.Context, p *TeamPreset) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return invalid("a team preset needs a name")
	}
	if _, err := db.GetTeamPreset(ctx, p.ID); err != nil {
		return err
	}
	if err := db.validatePresetRoles(ctx, p.Roles); err != nil {
		return err
	}
	if err := db.validatePresetProjectOverrides(ctx, p); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE team_presets SET name=?, updated_at=? WHERE id=?`,
		p.Name, p.UpdatedAt.Format(time.RFC3339Nano), p.ID); err != nil {
		return fmt.Errorf("updating team preset %q: %w", p.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM team_preset_roles WHERE preset_id=?`, p.ID); err != nil {
		return err
	}
	if err := insertPresetRoles(ctx, tx, p.ID, p.Roles); err != nil {
		return err
	}
	return tx.Commit()
}

// ListTeamPresets returns the built-in team first, then the rest by name.
//
// Ordering by name alone put Default wherever the alphabet happened to place
// it, so a clone called "Calc pipeline" sat above the team every project starts
// on. It also decided the editor's fallback selection, which is the team you
// look at before choosing one — that should be the one a new project runs, not
// whichever name sorts first.
func (db *DB) ListTeamPresets(ctx context.Context) ([]TeamPreset, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id,name,builtin,created_at,updated_at FROM team_presets ORDER BY builtin DESC, name`)
	if err != nil {
		return nil, err
	}
	var out []TeamPreset
	for rows.Next() {
		p, err := scanPreset(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		roles, err := db.listPresetRoles(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Roles = roles
	}
	return out, nil
}

func (db *DB) GetTeamPreset(ctx context.Context, id string) (*TeamPreset, error) {
	p, err := scanPreset(db.read.QueryRowContext(ctx,
		`SELECT id,name,builtin,created_at,updated_at FROM team_presets WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("team preset %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	p.Roles, err = db.listPresetRoles(ctx, id)
	return p, err
}

func (db *DB) DeleteTeamPreset(ctx context.Context, id string) error {
	p, err := db.GetTeamPreset(ctx, id)
	if err != nil {
		return err
	}
	if p.Builtin {
		return invalid("built-in team presets cannot be deleted")
	}
	var used int
	if err := db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE team_preset_id=?`, id).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return invalid("team preset is used by %d project(s); choose another team there first", used)
	}
	res, err := db.sql.ExecContext(ctx, `DELETE FROM team_presets WHERE id=?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res, fmt.Sprintf("team preset %s", id))
}

func scanPreset(s scanner) (*TeamPreset, error) {
	var p TeamPreset
	var builtin int
	var created, updated string
	if err := s.Scan(&p.ID, &p.Name, &builtin, &created, &updated); err != nil {
		return nil, err
	}
	p.Builtin = builtin != 0
	var err error
	p.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, err
	}
	p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return &p, err
}

func (db *DB) listPresetRoles(ctx context.Context, id string) ([]TeamPresetRole, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT template_id,position,enabled,`+roleOverrideCols+
		` FROM team_preset_roles WHERE preset_id=? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Empty, never nil. A preset with no roles is an ordinary state — it is
	// what unchecking the last role produces — and a nil slice marshals as
	// `"roles": null`, which every consumer in the cockpit dereferenced. One
	// empty preset anywhere in the list took the whole Team page down with a
	// TypeError, for every project, until the row was deleted by hand.
	out := []TeamPresetRole{}
	for rows.Next() {
		var r TeamPresetRole
		var enabled int
		var h, m, a, recv, prompt, gate sql.NullString
		var items, age sql.NullInt64
		if err := rows.Scan(&r.TemplateID, &r.Position, &enabled,
			&h, &m, &a, &recv, &items, &age, &prompt, &gate); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		if err := decodeOverrides(&r.RoleOverrides, h, m, a, recv, items, age, prompt, gate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) validatePresetRoles(ctx context.Context, roles []TeamPresetRole) error {
	seen := map[string]bool{}
	for _, r := range roles {
		if seen[r.TemplateID] {
			return invalid("role %s appears twice in the team", r.TemplateID)
		}
		seen[r.TemplateID] = true
		t, err := db.GetTemplate(ctx, r.TemplateID)
		if err != nil {
			return err
		}
		applyOverrides(t, r.RoleOverrides)
		if err := t.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) validatePresetProjectOverrides(ctx context.Context, p *TeamPreset) error {
	byTemplate := map[string]TeamPresetRole{}
	for _, r := range p.Roles {
		byTemplate[r.TemplateID] = r
	}
	rows, err := db.read.QueryContext(ctx, `SELECT o.template_id,`+roleOverrideCols+`
		FROM project_role_overrides o JOIN projects p ON p.id=o.project_id WHERE p.team_preset_id=?`, p.ID)
	if err != nil {
		return err
	}
	type entry struct {
		id string
		o  RoleOverrides
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var h, m, a, recv, prompt, gate sql.NullString
		var items, age sql.NullInt64
		if err := rows.Scan(&e.id, &h, &m, &a, &recv, &items, &age, &prompt, &gate); err != nil {
			rows.Close()
			return err
		}
		if err := decodeOverrides(&e.o, h, m, a, recv, items, age, prompt, gate); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, e := range entries {
		pr, ok := byTemplate[e.id]
		if !ok {
			continue
		}
		t, err := db.GetTemplate(ctx, e.id)
		if err != nil {
			return err
		}
		applyOverrides(t, pr.RoleOverrides)
		applyOverrides(t, e.o)
		if err := t.Validate(); err != nil {
			return fmt.Errorf("team preset would invalidate a project override: %w", err)
		}
	}
	return nil
}

func insertPresetRoles(ctx context.Context, tx *sql.Tx, id string, roles []TeamPresetRole) error {
	for i, r := range roles {
		args, err := marshalOverrideArgs(r.ArgsOverride)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_preset_roles
			(preset_id,template_id,position,enabled,`+roleOverrideCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, r.TemplateID, i, r.Enabled, r.HarnessOverride, r.ModelOverride, args,
			r.ReceiveOverride, r.BatchMaxItemsOverride, r.BatchMaxAgeSecOverride,
			r.PromptOverride, r.GateOverride); err != nil {
			return fmt.Errorf("adding role to team preset: %w", err)
		}
	}
	return nil
}

func applyOverrides(t *RoleTemplate, o RoleOverrides) {
	if o.HarnessOverride != nil {
		t.Harness = *o.HarnessOverride
	}
	if o.ModelOverride != nil {
		t.Model = *o.ModelOverride
	}
	if o.ArgsOverride != nil {
		t.Args = slices.Clone(o.ArgsOverride)
	}
	if o.ReceiveOverride != nil {
		t.Receive = *o.ReceiveOverride
	}
	if o.BatchMaxItemsOverride != nil {
		t.BatchMaxItems = *o.BatchMaxItemsOverride
	}
	if o.BatchMaxAgeSecOverride != nil {
		t.BatchMaxAgeSec = *o.BatchMaxAgeSecOverride
	}
	if o.PromptOverride != nil {
		t.Prompt = *o.PromptOverride
	}
	if o.GateOverride != nil {
		t.Gate = *o.GateOverride
	}
}

func decodeOverrides(o *RoleOverrides, h, m, a, recv sql.NullString, items, age sql.NullInt64, prompt, gate sql.NullString) error {
	if h.Valid {
		v := h.String
		o.HarnessOverride = &v
	}
	if m.Valid {
		v := m.String
		o.ModelOverride = &v
	}
	if a.Valid {
		var err error
		o.ArgsOverride, err = unmarshalArgs(a.String)
		if err != nil {
			return err
		}
	}
	if recv.Valid {
		v := recv.String
		o.ReceiveOverride = &v
	}
	if items.Valid {
		v := int(items.Int64)
		o.BatchMaxItemsOverride = &v
	}
	if age.Valid {
		v := int(age.Int64)
		o.BatchMaxAgeSecOverride = &v
	}
	if prompt.Valid {
		v := prompt.String
		o.PromptOverride = &v
	}
	if gate.Valid {
		v := gate.String
		o.GateOverride = &v
	}
	return nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CreateProject registers a repository with no team. path is stored absolute so
// the same repo reached by two relative paths is one project, not two.
func (db *DB) CreateProject(ctx context.Context, path, name, baseBranch string) (*Project, error) {
	return db.createProject(ctx, path, name, baseBranch, false)
}

// CreateProjectWithDefaultTeam registers a repository and gives it the starting
// pipeline, in one transaction.
//
// The two used to be separate calls from the handler: insert the project, then
// select the default team. When a built-in role was missing from the library
// the second failed, the request reported an error, and the project row — with
// the unique path that goes with it — stayed behind. Adding the same repository
// again then failed on the uniqueness of a project the operator had never
// successfully created and could not see anywhere to remove.
func (db *DB) CreateProjectWithDefaultTeam(ctx context.Context, path, name, baseBranch string) (*Project, error) {
	return db.createProject(ctx, path, name, baseBranch, true)
}

func (db *DB) createProject(ctx context.Context, path, name, baseBranch string, withTeam bool) (*Project, error) {
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

	// A path is checked before it is stored. Without this any string was
	// accepted — "/totally/made/up/nonsense" and "/etc/hosts" both became
	// projects — and the mistake surfaced much later as a worktree that could
	// not be created, by which point it looked like a git problem rather than a
	// typo in a dialog.
	info, err := os.Stat(abs)
	switch {
	case os.IsNotExist(err):
		return nil, invalid("there is nothing at %s", abs)
	case err != nil:
		return nil, invalid("cannot read %s: %v", abs, err)
	case !info.IsDir():
		return nil, invalid("%s is a file; a project is a directory", abs)
	}

	var presetID any
	if withTeam {
		if _, err := db.GetTeamPreset(ctx, DefaultTeamPresetID); err != nil {
			return nil, fmt.Errorf("default team preset is missing: %w", err)
		}
		presetID = DefaultTeamPresetID
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("creating project %s: %w", abs, err)
	}
	defer tx.Rollback()

	id := NewID()
	created := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO projects (id, path, name, base_branch, created_at, team_preset_id) VALUES (?,?,?,?,?,?)`,
		id, abs, name, baseBranch, created.Format(time.RFC3339Nano), presetID); err != nil {
		return nil, fmt.Errorf("creating project %s: %w", abs, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("creating project %s: %w", abs, err)
	}

	// Read it back rather than returning the struct that was written. Columns
	// with defaults — integration is one — are set by the database, and a
	// hand-built return value reported an empty string for it while the row
	// said "merge". Returning what was stored is the only way the two cannot
	// disagree.
	return db.GetProject(ctx, id)
}

// ListProjects returns projects most-recently-opened first, so the picker
// surfaces what you were last working on.
func (db *DB) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT id, path, name, base_branch, integration, pr_draft, created_at, last_opened_at,
		        chat_harness, chat_model, icon, team_preset_id, team_topology_override
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
	row := db.read.QueryRowContext(ctx,
		`SELECT id, path, name, base_branch, integration, pr_draft, created_at, last_opened_at,
		        chat_harness, chat_model, icon, team_preset_id, team_topology_override FROM projects WHERE id = ?`, id)
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
	p, err := db.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	return db.SetProjectTeam(ctx, projectID, p.TeamPresetID, true, roles)
}

// SetProjectTeam selects a reusable preset and atomically replaces the
// project's optional topology and field-override layers. When topologyOverride
// is false, roles carries only field overrides for roles in the selected preset.
func (db *DB) SetProjectTeam(ctx context.Context, projectID string, presetID *string, topologyOverride bool, roles []ProjectRole) error {
	if _, err := db.GetProject(ctx, projectID); err != nil {
		return err
	}
	var preset *TeamPreset
	var err error
	if presetID != nil {
		preset, err = db.GetTeamPreset(ctx, *presetID)
		if err != nil {
			return err
		}
		// A team belonging to another project is not on offer here. Without
		// this the owner is only a filter in the picker, and anything that
		// posts an id straight at the daemon walks around it.
		if preset.ProjectID != nil && *preset.ProjectID != projectID {
			return invalid("team %s belongs to another project", preset.Name)
		}
	}
	if !topologyOverride && preset == nil {
		return invalid("a project without a preset needs its own topology")
	}
	allowed := map[string]bool{}
	if preset != nil {
		for _, r := range preset.Roles {
			allowed[r.TemplateID] = true
		}
	}
	seen := map[string]bool{}
	for _, r := range roles {
		if seen[r.TemplateID] {
			return invalid("role %s appears twice in the team; each role joins a pipeline once", r.TemplateID)
		}
		seen[r.TemplateID] = true
		if !topologyOverride && !allowed[r.TemplateID] {
			return invalid("role %s is not in the selected preset", r.TemplateID)
		}
		t, err := db.GetTemplate(ctx, r.TemplateID)
		if err != nil {
			return err
		}
		if preset != nil {
			for _, pr := range preset.Roles {
				if pr.TemplateID == r.TemplateID {
					applyOverrides(t, pr.RoleOverrides)
					break
				}
			}
		}
		applyOverrides(t, r.RoleOverrides)
		if err := t.Validate(); err != nil {
			return err
		}
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning team update: %w", err)
	}
	defer tx.Rollback()
	var presetValue any
	if presetID != nil {
		presetValue = *presetID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET team_preset_id=?, team_topology_override=? WHERE id=?`, presetValue, topologyOverride, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_roles WHERE project_id=?`, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_role_overrides WHERE project_id=?`, projectID); err != nil {
		return err
	}
	if topologyOverride {
		if err := insertTeam(ctx, tx, projectID, roles); err != nil {
			return err
		}
	}
	if err := insertProjectOverrides(ctx, tx, projectID, roles); err != nil {
		return err
	}
	return tx.Commit()
}

// insertTeam writes a pipeline inside a caller's transaction.
//
// Positions are normalised to 0..n-1 in the order given, so the caller can send
// whatever the drag produced without worrying about gaps or ties.
func insertTeam(ctx context.Context, tx *sql.Tx, projectID string, roles []ProjectRole) error {
	for i, r := range roles {
		args, err := marshalOverrideArgs(r.ArgsOverride)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO project_roles (project_id,template_id,position,enabled,model_override,args_override) VALUES (?,?,?,?,?,?)`,
			projectID, r.TemplateID, i, r.Enabled, r.ModelOverride, args); err != nil {
			return fmt.Errorf("adding role to team: %w", err)
		}
	}
	return nil
}

func insertProjectOverrides(ctx context.Context, tx *sql.Tx, projectID string, roles []ProjectRole) error {
	for _, r := range roles {
		if !hasOverrides(r.RoleOverrides) {
			continue
		}
		args, err := marshalOverrideArgs(r.ArgsOverride)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_role_overrides
			(project_id,template_id,`+roleOverrideCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			projectID, r.TemplateID, r.HarnessOverride, r.ModelOverride, args, r.ReceiveOverride,
			r.BatchMaxItemsOverride, r.BatchMaxAgeSecOverride, r.PromptOverride, r.GateOverride); err != nil {
			return fmt.Errorf("adding project role override: %w", err)
		}
	}
	return nil
}

func hasOverrides(o RoleOverrides) bool {
	return o.HarnessOverride != nil || o.ModelOverride != nil || o.ArgsOverride != nil ||
		o.ReceiveOverride != nil || o.BatchMaxItemsOverride != nil || o.BatchMaxAgeSecOverride != nil ||
		o.PromptOverride != nil || o.GateOverride != nil
}

// SelectDefaultTeam gives an existing project the starting pipeline. New
// projects get it as part of their own creation.
func (db *DB) SelectDefaultTeam(ctx context.Context, projectID string) error {
	id := DefaultTeamPresetID
	return db.SetProjectTeam(ctx, projectID, &id, false, nil)
}

// ResolveTeam returns the project's pipeline in order with overrides applied —
// what a cerebrate is actually asked to run.
//
// Terminal is computed here rather than stored: it is the last *enabled* role,
// so disabling the final role promotes the one before it without an edit
// anywhere else. Deciding terminality from config-file line order means
// reordering a file silently relocates the end of the pipeline.
func (db *DB) ResolveTeam(ctx context.Context, projectID string) ([]ResolvedRole, error) {
	p, err := db.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return db.resolveLayeredTeam(ctx, p)
}

func (db *DB) GetProjectTeam(ctx context.Context, projectID string) (*ProjectTeam, error) {
	p, err := db.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// The project is already in hand; re-reading it here was the cheapest of
	// the round trips and the easiest to lose.
	roles, err := db.resolveLayeredTeam(ctx, p)
	if err != nil {
		return nil, err
	}
	return &ProjectTeam{PresetID: p.TeamPresetID, TopologyOverride: p.TeamTopologyOverride, Roles: roles}, nil
}

// resolveLayeredTeam merges the three layers — the library's template, the
// team's overrides, the project's own — into what each role actually runs.
//
// It takes the project rather than its id because every caller already has it,
// and this is on the board poll.
func (db *DB) resolveLayeredTeam(ctx context.Context, p *Project) ([]ResolvedRole, error) {
	projectID := p.ID
	presetByTemplate := map[string]TeamPresetRole{}
	var topology []ProjectRole
	if p.TeamPresetID != nil {
		preset, err := db.GetTeamPreset(ctx, *p.TeamPresetID)
		if err != nil {
			return nil, err
		}
		for _, r := range preset.Roles {
			presetByTemplate[r.TemplateID] = r
			if !p.TeamTopologyOverride {
				topology = append(topology, ProjectRole{TemplateID: r.TemplateID, Position: r.Position, Enabled: r.Enabled})
			}
		}
	}
	if p.TeamTopologyOverride || p.TeamPresetID == nil {
		rows, err := db.read.QueryContext(ctx, `SELECT template_id,position,enabled FROM project_roles WHERE project_id=? ORDER BY position`, projectID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r ProjectRole
			var enabled int
			if err := rows.Scan(&r.TemplateID, &r.Position, &enabled); err != nil {
				// Closed on the way out, as the two loops below already do.
				// database/sql never reclaims a Rows that was neither drained
				// nor closed, so eight of these exhaust the read pool and every
				// later read in the daemon blocks for good.
				rows.Close()
				return nil, err
			}
			r.Enabled = enabled != 0
			topology = append(topology, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	projectOverrides := map[string]RoleOverrides{}
	rows, err := db.read.QueryContext(ctx, `SELECT template_id,`+roleOverrideCols+` FROM project_role_overrides WHERE project_id=?`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var o RoleOverrides
		var h, m, a, recv, prompt, gate sql.NullString
		var items, age sql.NullInt64
		if err := rows.Scan(&id, &h, &m, &a, &recv, &items, &age, &prompt, &gate); err != nil {
			rows.Close()
			return nil, err
		}
		if err := decodeOverrides(&o, h, m, a, recv, items, age, prompt, gate); err != nil {
			rows.Close()
			return nil, err
		}
		projectOverrides[id] = o
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Roles whose merged settings do not validate. Collected rather than
	// returned as an error, so the team still resolves and the operator can see
	// and fix the role that is wrong.
	var invalidRoles []string

	// Every template the team needs, in one query rather than one each.
	//
	// This loop used to call GetTemplate per role, which is the N+1 pattern and
	// showed: eight roles cost 222us against 42us for the single indexed join
	// this replaced, and the join did not move with role count. ResolveTeam is
	// on the board poll and on the spawn, routing, preflight and chat paths, so
	// the per-row query was being paid continuously.
	wanted := make([]string, 0, len(topology))
	for _, m := range topology {
		wanted = append(wanted, m.TemplateID)
	}
	templates, err := db.templatesByID(ctx, wanted)
	if err != nil {
		return nil, err
	}

	out := make([]ResolvedRole, 0, len(topology))
	for i, membership := range topology {
		base, ok := templates[membership.TemplateID]
		if !ok {
			// Unreachable while the foreign key holds — project_roles and
			// team_preset_roles both cascade on template deletion — and skipped
			// rather than fatal if it ever is, for the same reason the
			// validation below is not fatal: a read must not take the project
			// down with it.
			slog.Warn("a team references a role that is not in the library",
				"project", projectID, "template", membership.TemplateID)
			continue
		}
		// A copy per role: applyOverrides writes through the pointer, and the
		// map holds one template that several members could share.
		roleCopy := base
		t := &roleCopy
		if pr, ok := presetByTemplate[membership.TemplateID]; ok {
			applyOverrides(t, pr.RoleOverrides)
		}
		baseline := *t
		o := projectOverrides[membership.TemplateID]
		applyOverrides(t, o)

		// Deliberately not validated here. Resolving is a read, and a read that
		// can fail on data already stored takes the whole project down with it:
		// ResolveTeam is on the board poll, preflight, overmind's spawn, nydus
		// routing and chat, so one invalid combination made a project impossible
		// to open *or* to repair. The combinations are checked where they are
		// written — SetProjectTeam, and the preset and template editors — which
		// is where a person can still do something about the answer.
		//
		// The cross product is the gap: a template edit that passes its own
		// validation can still invalidate a role once a preset's overrides are
		// layered on it, because neither write path sees both. Reported rather
		// than papered over; the role arrives as stored and preflight is what
		// refuses to spawn it.
		if err := t.Validate(); err != nil {
			invalidRoles = append(invalidRoles, fmt.Sprintf("%s: %v", t.Name, err))
		}
		r := ResolvedRole{RoleTemplate: *t, Position: i, Enabled: membership.Enabled, RoleOverrides: o}
		r.Overridden = t.Harness != baseline.Harness || t.Model != baseline.Model || !slices.Equal(t.Args, baseline.Args) ||
			t.Receive != baseline.Receive || t.BatchMaxItems != baseline.BatchMaxItems || t.BatchMaxAgeSec != baseline.BatchMaxAgeSec ||
			t.Prompt != baseline.Prompt || t.Gate != baseline.Gate
		out = append(out, r)
	}
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Enabled {
			out[i].Terminal = true
			break
		}
	}
	if len(invalidRoles) > 0 {
		// Logged where it can be seen, not returned: the team resolves, and
		// preflight is the gate that refuses to spawn a role configured like
		// this. A read that fails leaves nothing to look at and nothing to fix.
		slog.Warn("a role's merged settings do not validate",
			"project", projectID, "roles", strings.Join(invalidRoles, "; "))
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
	var presetID sql.NullString
	var draft, topology int
	if err := s.Scan(&p.ID, &p.Path, &p.Name, &p.BaseBranch, &p.Integration, &draft, &created, &lastOpened,
		&p.ChatHarness, &p.ChatModel, &p.Icon, &presetID, &topology); err != nil {
		return nil, err
	}
	p.PRDraft = draft != 0
	p.TeamTopologyOverride = topology != 0
	if presetID.Valid {
		v := presetID.String
		p.TeamPresetID = &v
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

// SetIntegration changes how a project's finished work reaches its base branch.
func (db *DB) SetIntegration(ctx context.Context, projectID, mode string, prDraft bool) (*Project, error) {
	if !ValidIntegration(mode) {
		return nil, invalid("unknown integration mode %q; use merge, branch or pr", mode)
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE projects SET integration=?, pr_draft=? WHERE id=?`, mode, prDraft, projectID)
	if err != nil {
		return nil, fmt.Errorf("setting integration mode: %w", err)
	}
	if err := mustAffect(res, fmt.Sprintf("project %s", projectID)); err != nil {
		return nil, err
	}
	return db.GetProject(ctx, projectID)
}

// SetChatAgent chooses what answers questions in Chat.
//
// Empty for either means inherit from the terminal role, which is the default
// and a reasonable one — it just should not be the only option, since the
// reviewer is usually the most expensive model on the team and asking where a
// function lives does not need it.
// SetProjectName renames a project.
//
// The name is a label, not an identity: the path is what makes a project one
// project, and every reference anywhere is by id. So this is free to change,
// and nothing has to be updated behind it — including the derived mark, whose
// colour comes from the id precisely so that a rename does not move it.
func (db *DB) SetProjectName(ctx context.Context, projectID, name string) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, invalid("a project needs a name")
	}
	if len([]rune(name)) > maxProjectName {
		return nil, invalid("that name is too long; %d characters at most", maxProjectName)
	}
	if strings.ContainsAny(name, "\n\r\t\x00") {
		return nil, invalid("a project name cannot contain line breaks")
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE projects SET name = ? WHERE id = ?`, name, projectID)
	if err != nil {
		return nil, fmt.Errorf("renaming project %s: %w", projectID, err)
	}
	if err := mustAffect(res, fmt.Sprintf("project %s", projectID)); err != nil {
		return nil, err
	}
	return db.GetProject(ctx, projectID)
}

// maxProjectName bounds the label. Long enough for a real repository name and
// finite, because this renders in a fixed-width rail.
const maxProjectName = 120

// SetProjectIcon sets or clears a project's mark.
//
// The value is a path inside the project — its own favicon, logo or app icon —
// so what the switcher shows is the identity the repository already has rather
// than something invented for it in a settings form.
//
// Checked for shape here and for containment where it is read: a stored path is
// not a trusted path, because the thing that eventually opens it is a file
// server on a cockpit with no authentication. Both ends check.
func (db *DB) SetProjectIcon(ctx context.Context, projectID, icon string) (*Project, error) {
	icon = strings.TrimSpace(icon)
	if icon != "" {
		if err := validIconPath(icon); err != nil {
			return nil, err
		}
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE projects SET icon = ? WHERE id = ?`, icon, projectID)
	if err != nil {
		return nil, fmt.Errorf("setting the project icon: %w", err)
	}
	if err := mustAffect(res, fmt.Sprintf("project %s", projectID)); err != nil {
		return nil, err
	}
	return db.GetProject(ctx, projectID)
}

// iconImageExts are the image types a project mark may be. The API refuses to
// serve anything else; this refuses to store it, so a path that could never be
// displayed cannot be saved in the first place.
var iconImageExts = []string{".ico", ".png", ".svg", ".jpg", ".jpeg", ".webp", ".gif", ".avif"}

// maxIconPath bounds the stored path. Generous for a nested assets directory,
// finite because this is a free-text field on an unauthenticated surface.
const maxIconPath = 512

func validIconPath(icon string) error {
	if len(icon) > maxIconPath {
		return invalid("that path is too long to be a project icon")
	}
	if strings.ContainsAny(icon, "\n\r\t\x00") {
		return invalid("an icon path cannot contain control characters")
	}
	if strings.HasPrefix(icon, "/") || strings.HasPrefix(icon, "~") || filepath.IsAbs(icon) {
		return invalid("an icon is a path inside the project, not an absolute one")
	}
	// Every segment, not just the string: "a/../../etc" contains no leading
	// "..", and "assets/../.." is neither absolute nor prefixed.
	for _, part := range strings.Split(filepath.ToSlash(icon), "/") {
		if part == ".." {
			return invalid("an icon is a path inside the project")
		}
	}
	ext := strings.ToLower(filepath.Ext(icon))
	if !slices.Contains(iconImageExts, ext) {
		return invalid("an icon must be an image: %s", strings.Join(iconImageExts, ", "))
	}
	return nil
}

func (db *DB) SetChatAgent(ctx context.Context, projectID, harness, model string) (*Project, error) {
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE projects SET chat_harness = ?, chat_model = ? WHERE id = ?`,
		harness, model, projectID); err != nil {
		return nil, fmt.Errorf("setting the chat agent: %w", err)
	}
	return db.GetProject(ctx, projectID)
}

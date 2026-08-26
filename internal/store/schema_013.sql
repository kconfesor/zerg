-- zerg schema, version 13: reusable team presets, layered project role overrides,
-- and draft pull-request delivery.

CREATE TABLE team_presets (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    builtin    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE team_preset_roles (
    preset_id                 TEXT NOT NULL REFERENCES team_presets(id) ON DELETE CASCADE,
    template_id               TEXT NOT NULL REFERENCES role_templates(id) ON DELETE CASCADE,
    position                  INTEGER NOT NULL,
    enabled                   INTEGER NOT NULL DEFAULT 1,
    harness_override          TEXT,
    model_override            TEXT,
    args_override             TEXT,
    receive_override          TEXT CHECK (receive_override IS NULL OR receive_override IN ('task','batch')),
    batch_max_items_override  INTEGER,
    batch_max_age_sec_override INTEGER,
    prompt_override           TEXT,
    gate_override             TEXT CHECK (gate_override IS NULL OR gate_override IN ('none','approval')),
    PRIMARY KEY (preset_id, template_id)
);
CREATE INDEX idx_team_preset_roles_order ON team_preset_roles (preset_id, position);

-- Existing project_roles remain the project's optional topology override. Role
-- field overrides move to their own table so a project can override one prompt
-- without snapshotting the preset's ordering or membership.
CREATE TABLE project_role_overrides (
    project_id                 TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    template_id                TEXT NOT NULL REFERENCES role_templates(id) ON DELETE CASCADE,
    harness_override           TEXT,
    model_override             TEXT,
    args_override              TEXT,
    receive_override           TEXT CHECK (receive_override IS NULL OR receive_override IN ('task','batch')),
    batch_max_items_override   INTEGER,
    batch_max_age_sec_override INTEGER,
    prompt_override            TEXT,
    gate_override              TEXT CHECK (gate_override IS NULL OR gate_override IN ('none','approval')),
    PRIMARY KEY (project_id, template_id)
);

ALTER TABLE projects ADD COLUMN team_preset_id TEXT REFERENCES team_presets(id) ON DELETE SET NULL;
ALTER TABLE projects ADD COLUMN team_topology_override INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN pr_draft INTEGER NOT NULL DEFAULT 0;

-- A stable id lets new-project creation choose the built-in without looking it
-- up by a user-editable name. Seed fills its roles after the role library exists.
INSERT INTO team_presets (id, name, builtin, created_at, updated_at)
VALUES ('builtin-default-team', 'Default', 1, '1970-01-01T00:00:00Z', '1970-01-01T00:00:00Z');

-- Preserve every existing project's effective topology and model/args values.
UPDATE projects SET team_topology_override = 1
WHERE EXISTS (SELECT 1 FROM project_roles pr WHERE pr.project_id = projects.id);
INSERT INTO project_role_overrides
    (project_id, template_id, model_override, args_override)
SELECT project_id, template_id, model_override, args_override
FROM project_roles
WHERE model_override IS NOT NULL OR args_override IS NOT NULL;

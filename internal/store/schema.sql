-- zerg schema, version 1.
--
-- Two scopes share one file: the global role library and per-project state.
-- See ARCHITECTURE.md §4.1 and §9.

-- ── global library ────────────────────────────────────────────────────────

CREATE TABLE role_templates (
    id                TEXT    PRIMARY KEY,
    name              TEXT    NOT NULL UNIQUE,
    harness           TEXT    NOT NULL,
    model             TEXT    NOT NULL DEFAULT '',
    args              TEXT    NOT NULL DEFAULT '[]',    -- JSON array
    receive           TEXT    NOT NULL DEFAULT 'task'   CHECK (receive IN ('task','batch')),
    batch_max_items   INTEGER NOT NULL DEFAULT 8,
    batch_max_age_sec INTEGER NOT NULL DEFAULT 300,
    prompt            TEXT    NOT NULL DEFAULT '',
    gate              TEXT    NOT NULL DEFAULT 'none'   CHECK (gate IN ('none','approval')),
    builtin           INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- ── projects ──────────────────────────────────────────────────────────────

CREATE TABLE projects (
    id             TEXT PRIMARY KEY,
    path           TEXT NOT NULL UNIQUE,
    name           TEXT NOT NULL,
    base_branch    TEXT NOT NULL DEFAULT 'main',
    created_at     TEXT NOT NULL,
    last_opened_at TEXT
);

-- Which templates a project uses, in what order. An override lives here
-- rather than in a side table: it is a property of the pairing, not a patch.
CREATE TABLE project_roles (
    project_id     TEXT    NOT NULL REFERENCES projects(id)       ON DELETE CASCADE,
    template_id    TEXT    NOT NULL REFERENCES role_templates(id) ON DELETE CASCADE,
    position       INTEGER NOT NULL,
    enabled        INTEGER NOT NULL DEFAULT 1,
    model_override TEXT,
    args_override  TEXT,
    PRIMARY KEY (project_id, template_id)
);

CREATE INDEX idx_project_roles_order ON project_roles (project_id, position);

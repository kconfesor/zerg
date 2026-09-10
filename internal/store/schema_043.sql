-- A subtask's last gate is an approval to integrate into its feature, not a
-- terminal approval to land on base. Keep the destination on the approval so
-- deciding (or recovering) it never guesses from a mutable role pipeline.
ALTER TABLE approvals ADD COLUMN feature_id TEXT REFERENCES tasks(id) ON DELETE SET NULL;

-- A person may send the reviewed whole back, without cancelling its work.
ALTER TABLE feature_reviews ADD COLUMN decided_by TEXT NOT NULL DEFAULT 'supervisor';

-- Keep feature accounting even when a finished child is later deleted and
-- task_id becomes NULL. Project totals still read each turn exactly once.
ALTER TABLE usage_turns ADD COLUMN feature_id TEXT REFERENCES tasks(id) ON DELETE SET NULL;
UPDATE usage_turns SET feature_id = (
    SELECT CASE WHEN kind = 'feature' THEN id ELSE parent_id END
    FROM tasks WHERE tasks.id = usage_turns.task_id
);
CREATE INDEX idx_usage_feature ON usage_turns(feature_id);

-- Sidecars have no work lease. Record their work windows so planning/review
-- tokens and transcripts belong to the feature rather than only the project.
CREATE TABLE sidecar_work (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    started_at TEXT NOT NULL,
    finished_at TEXT
);
CREATE INDEX idx_sidecar_work_role ON sidecar_work(project_id, role, started_at);
CREATE UNIQUE INDEX idx_sidecar_work_open ON sidecar_work(project_id, role) WHERE finished_at IS NULL;

-- zerg schema, version 9: a claim state for a decision being carried out.
--
-- Approving a terminal handoff merges before the decision is recorded, and the
-- merge cannot run inside the write transaction — a git subprocess must never
-- hold the single writer. So the transaction that checked "still pending" is
-- released, the merge runs, and a second transaction records the outcome.
--
-- That gap is a race: two decisions could both read pending, both merge, and
-- the later one overwrite the earlier. An approve racing a reject recorded
-- "rejected" over a branch that had already landed.
--
-- 'integrating' closes it. A decision claims the approval with a guarded
-- update before doing anything irreversible; a second caller finds it no longer
-- pending and stops. SQLite cannot alter a CHECK, so the table is rebuilt.
CREATE TABLE approvals_new (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    state      TEXT NOT NULL CHECK (state IN ('pending','integrating','approved','rejected')),
    note       TEXT,
    created_at TEXT NOT NULL,
    decided_at TEXT
);

INSERT INTO approvals_new (id, project_id, message_id, state, note, created_at, decided_at)
SELECT id, project_id, message_id, state, note, created_at, decided_at FROM approvals;

DROP TABLE approvals;
ALTER TABLE approvals_new RENAME TO approvals;
CREATE INDEX idx_approvals_pending ON approvals (project_id, state);

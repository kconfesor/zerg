-- zerg schema, version 41: materialising a plan. Issue #40, phase 3.
--
-- Child tasks, a feature branch, and blocked-until-integrated scheduling land
-- together, because any subset of them puts work in the wrong place: a child
-- that can complete without a feature head lands on base, and a dependent
-- queued because its sibling said "done" starts from a tree that does not
-- contain the work.
--
-- blocked is a column, not a new tasks.state. The states live in a CHECK, and
-- schema_014.sql is the record of what rebuilding the table would destroy.
-- A queued card is one a role will pick up; a blocked card is one nothing will,
-- and the board must not draw them the same.
--
-- priority lives on the task so a handoff cannot drop it: messages.priority is
-- what the queue reads, and it was hardcoded to 50 at every write.
--
-- child_task_id is SET NULL on delete so removing a child does not take the
-- plan item with it; the revision is the record of what was planned.
--
-- feature_runs holds the branch and the shas, so integration is idempotent
-- after a crash and a later review can name the head it looked at.
ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 50;
ALTER TABLE tasks ADD COLUMN blocked INTEGER NOT NULL DEFAULT 0;
ALTER TABLE feature_plan_items ADD COLUMN child_task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL;

CREATE TABLE feature_runs (
    feature_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    branch     TEXT NOT NULL,
    base_sha   TEXT NOT NULL DEFAULT '',
    head_sha   TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'conflict', 'done', 'cancelled')),
    created_at TEXT NOT NULL
);

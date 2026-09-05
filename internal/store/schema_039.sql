-- zerg schema, version 39: a feature is a group of cards, not a card in a lane.
--
-- Issue #40, phase 1. A feature reuses `tasks` so the board, the trail, history,
-- cost, hidden, pinned and delete-cascade come for free, but it is not work:
-- it has no route, Claim never sees it, and every query that lists cards for a
-- person excludes it. kind is the discriminator; parent_id is the grouping.
--
-- Lifecycle is not folded into tasks.state. That column is a four-value CHECK,
-- and a feature will later move through planning, blocked execution, review
-- and landing. Bending those into queued/working/done/rejected makes state
-- mean something different depending on kind, which is the query that someone
-- writes next. Companion tables wait for the phases that need them.
--
-- Columns, not a rebuilt table. tasks cascades, and schema_014.sql is the
-- record of what DROP would destroy. parent_id SET NULL on delete so removing
-- a feature ungroups its cards rather than taking them with it; cascading a
-- live hierarchy is refused later, when children are actually running.
ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'work'
    CHECK (kind IN ('work', 'feature'));
ALTER TABLE tasks ADD COLUMN parent_id TEXT REFERENCES tasks(id) ON DELETE SET NULL;
CREATE INDEX idx_tasks_parent ON tasks (parent_id);

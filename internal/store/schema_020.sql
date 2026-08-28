-- zerg schema, version 20: a task can keep its transcript.

-- Events are the tier that ages out: roughly 40 MB a day at five active roles,
-- and they exist to replay recent work, so the sweep drops them past the
-- retention window while costs, metrics and outcomes stay. That is the right
-- default and the wrong answer for the one card someone will want to read in
-- six months, which is usually the card that went wrong.
--
-- Pinned exempts a task from the sweep. Nothing else changes: the card is
-- ordinary in every other way, and unpinning lets the next sweep take it.
ALTER TABLE tasks ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;

-- The sweep now asks whether an event's task is pinned, and the history screen
-- asks whether a task still has a transcript at all. Both are a lookup by
-- task_id, which had no index: events carried one on (project_id, id) for the
-- activity stream and one on ts for the sweep's cutoff, and neither helps here.
CREATE INDEX events_task ON events(task_id);

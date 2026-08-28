-- What a runner learned about serving this project, and whether to run it
-- without being asked.
--
-- Prose, not fields. The first attempt at this was a table of commands typed
-- by the operator -- a config file with a form in front of it, in a tool whose
-- premise is that agents read the repository and work things out. Splitting
-- what an agent learned into command/cwd/env would be the same mistake one
-- layer down: it assumes the shape of the answer, and the shape is different
-- for a compose stack, a monorepo with three apps, and a binary that wants a
-- database first.
--
-- Written by the runner, read by the next one, editable by the operator when
-- it is wrong.
CREATE TABLE run_notes (
    project_id TEXT NOT NULL PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    note       TEXT NOT NULL,
    -- Who last wrote it: the runner, or the person correcting it.
    author     TEXT NOT NULL DEFAULT 'runner',
    updated_at TEXT NOT NULL
);

-- Whether finishing a task starts a preview of it.
--
-- Off by default and per project, because every run is an agent turn and
-- therefore money. On, it answers "what does it look like" before anybody
-- thinks to ask.
ALTER TABLE projects ADD COLUMN auto_run INTEGER NOT NULL DEFAULT 0;

-- The operator-authored targets are gone; see the rework on issue #9. Dropped
-- rather than left: a table nothing reads is a thing the next person has to
-- work out the status of, and its two migrations have already run on a real
-- database, so they cannot be edited away.
DROP TABLE deploy_targets;

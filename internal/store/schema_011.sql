-- zerg schema, version 11: a task id means one project's task, and a send
-- happens once.
--
-- Two related integrity holes, both reachable from an agent.
--
-- The first: every table that carries a task_id also carries a project_id, and
-- nothing tied the two together. The protocol boundary scopes what an agent
-- passes, but the database would still accept a row claiming project A and
-- task B belonging to project C — so one mistake anywhere above became a
-- cross-project reference nothing could detect afterwards.
--
-- A composite foreign key is the textbook answer and is not available here.
-- SQLite cannot add a constraint to an existing table, so every one of these
-- tables would have to be rebuilt; DROP TABLE with foreign keys on performs an
-- implicit DELETE that fires ON DELETE CASCADE, which would take routes and
-- approvals with messages. And events, usage_turns and clarifications keep
-- their rows when a task is deleted (ON DELETE SET NULL) — as a composite key
-- SQLite would null every column of it, including a NOT NULL project_id.
--
-- Triggers enforce the same thing on the only paths that can break it, insert
-- and a task_id update, with no rebuild and no cascade to get wrong.

-- The parent key the checks are written against, and the index that makes
-- "this task, in this project" a lookup rather than a scan.
CREATE UNIQUE INDEX idx_tasks_project_id ON tasks (project_id, id);

CREATE TRIGGER messages_task_in_project_insert
BEFORE INSERT ON messages
WHEN NEW.task_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN
    SELECT RAISE(ABORT, 'message task_id belongs to another project');
END;

CREATE TRIGGER messages_task_in_project_update
BEFORE UPDATE OF task_id, project_id ON messages
WHEN NEW.task_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN
    SELECT RAISE(ABORT, 'message task_id belongs to another project');
END;

CREATE TRIGGER events_task_in_project_insert
BEFORE INSERT ON events
WHEN NEW.task_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN
    SELECT RAISE(ABORT, 'event task_id belongs to another project');
END;

CREATE TRIGGER usage_task_in_project_insert
BEFORE INSERT ON usage_turns
WHEN NEW.task_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN
    SELECT RAISE(ABORT, 'usage task_id belongs to another project');
END;

CREATE TRIGGER clarifications_task_in_project_insert
BEFORE INSERT ON clarifications
WHEN NEW.task_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN
    SELECT RAISE(ABORT, 'clarification task_id belongs to another project');
END;

-- ── send happens once ─────────────────────────────────────────────────────
--
-- The second hole: an agent whose response was lost retries, and the retry
-- creates a second hand-off. Two messages, two routes, two claims, two turns
-- of model work on the same result — and the operator sees a duplicate card
-- movement with no way to tell which one was real.
--
-- The lease the sender held is the operation's natural scope: it identifies
-- the unit of work being reported on, it already exists, and it is the
-- daemon's value rather than the agent's. op_key is the operation itself —
-- recipient, kind, commit and body — so a genuine second note still sends and
-- only a repeat of the identical call is absorbed.
ALTER TABLE messages ADD COLUMN source_lease_id TEXT REFERENCES leases(id) ON DELETE SET NULL;
ALTER TABLE messages ADD COLUMN op_key TEXT;

-- NULLs are distinct in a SQLite unique index, so everything that has no
-- source lease — an opening message, a seeded row — is unaffected.
CREATE UNIQUE INDEX idx_messages_idempotency ON messages (source_lease_id, op_key);

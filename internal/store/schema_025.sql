-- Artifacts: what an agent produced that a person wants to look at.
--
-- A file is stored content-addressed on disk and named here by its digest, so
-- the same output produced by two tasks costs one copy and survives the
-- worktree it was built in being pruned. A service stores no bytes at all: it
-- is a port a process is listening on, and it is only meaningful while that
-- process is alive.
--
-- ON DELETE CASCADE follows the task, like every other transcript row: an
-- artifact is part of the record of the work. Rows keyed to a project with no
-- task are for the ones produced outside a card, which nothing does yet but
-- the column allows rather than forcing a fake task.
CREATE TABLE artifacts (
    id         TEXT NOT NULL PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id    TEXT REFERENCES tasks(id) ON DELETE CASCADE,

    -- Who made it. Free text, like a review comment's author: a role removed
    -- from the library should not take the record of what it produced with it.
    role       TEXT NOT NULL DEFAULT '',

    kind       TEXT NOT NULL CHECK (kind IN ('file', 'image', 'service')),
    label      TEXT NOT NULL DEFAULT '',

    -- For a file: the digest that names it on disk, its type and its size.
    -- Empty for a service, which has no bytes.
    sha256     TEXT NOT NULL DEFAULT '',
    mime       TEXT NOT NULL DEFAULT '',
    bytes      INTEGER NOT NULL DEFAULT 0,

    -- The original name, so a download arrives called what the agent called it
    -- rather than sixty-four hex characters.
    name       TEXT NOT NULL DEFAULT '',

    -- For a service: the loopback port it is listening on. Zero for a file.
    port       INTEGER NOT NULL DEFAULT 0,

    -- A service outlives neither the swarm nor the daemon. Recorded so the
    -- cockpit can say "this was running" rather than offering a dead link.
    stopped_at TEXT,

    created_at TEXT NOT NULL,
    pinned     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX artifacts_task ON artifacts (task_id, created_at);
CREATE INDEX artifacts_project ON artifacts (project_id, created_at);

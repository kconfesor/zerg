-- zerg schema, version 6: the activity record.
--
-- Until now the only consumer of the event bus wrote to the daemon's stderr, so
-- "what did this agent actually do" was answerable only by having redirected
-- that stream to a file beforehand, and only until the next restart. The bus
-- already carried everything; nothing kept it.
--
-- The id is a monotonic ULID, which makes it three things at once: the primary
-- key, the chronological sort order, and the cursor an SSE client sends back as
-- Last-Event-ID to resume exactly where it dropped. No sequence column, no
-- timestamp comparison with ties to break.
CREATE TABLE events (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,

    -- Which card the work belonged to, resolved through the role's lease. Null
    -- is ordinary: an agent emits events before it claims anything.
    task_id    TEXT REFERENCES tasks(id) ON DELETE SET NULL,

    role       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    ts         TEXT NOT NULL,

    -- text is prose for a message, the reason for an error, empty otherwise.
    -- tool is the tool's name on tool_call.
    text       TEXT NOT NULL DEFAULT '',
    tool       TEXT NOT NULL DEFAULT '',

    -- data is a JSON object whose shape depends on kind: tool arguments for a
    -- tool_call, the token split and cost for a usage event. A per-kind column
    -- set would be mostly NULL and would need a migration every time a harness
    -- reports something new, which is the wrong trade for a display record.
    data       TEXT,

    -- An error the agent cannot recover from, so the view can mark the moment a
    -- role stopped rather than leaving it looking merely quiet.
    fatal      INTEGER NOT NULL DEFAULT 0
);

-- The one query that matters: this project's events after some cursor, in
-- order. Compound so the replay is an index scan rather than a sort.
CREATE INDEX events_project_id ON events(project_id, id);

-- Retention sweeps by age across all projects.
CREATE INDEX events_ts ON events(ts);

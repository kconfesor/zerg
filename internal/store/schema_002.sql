-- zerg schema, version 2: work.
--
-- The coordination layer. Every table here exists to make one of the
-- predecessor's failure modes impossible rather than merely unlikely.

-- A work period, Start to Stop. The predecessor had no such concept, so
-- "how many sessions, how long" was unanswerable.
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    end_reason TEXT
);

-- A card on the board.
--
-- lane says which role holds it; state says whether that role is actually
-- working on it. The predecessor had only the lane, so a card read as "in
-- cleaner's lane" the instant delivery happened, whether or not cleaner had
-- looked. Keeping both is what makes the board honest without delaying the
-- move to a moment the operator would find confusing.
--
-- active_ms accumulates lease durations, so wall time (completed - created)
-- and worked time are separable: a task six hours old with twelve minutes of
-- work was blocked, not hard.
CREATE TABLE tasks (
    id               TEXT    PRIMARY KEY,
    project_id       TEXT    NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    session_id       TEXT    REFERENCES sessions(id) ON DELETE SET NULL,
    name             TEXT    NOT NULL,
    body             TEXT    NOT NULL DEFAULT '',
    lane             TEXT    NOT NULL,
    state            TEXT    NOT NULL CHECK (state IN ('queued','working','done','rejected')),
    created_at       TEXT    NOT NULL,
    first_claimed_at TEXT,
    completed_at     TEXT,
    active_ms        INTEGER NOT NULL DEFAULT 0
);

-- A task name follows one card through the whole pipeline, so it must identify
-- exactly one card within a project.
CREATE UNIQUE INDEX idx_tasks_name ON tasks (project_id, name);
CREATE INDEX idx_tasks_lane ON tasks (project_id, lane);

-- One thing a role said. Immutable once written.
CREATE TABLE messages (
    id         TEXT    PRIMARY KEY,
    project_id TEXT    NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id    TEXT    REFERENCES tasks(id) ON DELETE CASCADE,
    from_role  TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('handoff','note')),
    priority   INTEGER NOT NULL DEFAULT 50,
    commit_sha TEXT,
    body       TEXT    NOT NULL DEFAULT '',
    terminal   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL
);

-- One message's delivery to one recipient.
--
-- The primary key is what makes delivery idempotent: the predecessor copied a
-- file per recipient and deduped on filename, so a genuinely different message
-- with a colliding name was silently dropped while its wake-up still fired.
CREATE TABLE routes (
    message_id   TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    to_role      TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('held','queued','claimed','done','rejected')),
    enqueued_at  TEXT,
    delivered_at TEXT,
    PRIMARY KEY (message_id, to_role)
);

CREATE INDEX idx_routes_queue ON routes (to_role, state);

-- A claim with a deadline.
--
-- This is the answer to the predecessor's permanent stall: an agent that
-- finished, saw an empty inbox, printed NO_TASK and stopped five milliseconds
-- before mail arrived left the queue wedged with no timer and no retry. Work
-- that is never acknowledged comes back.
CREATE TABLE leases (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    granted_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    acked_at   TEXT,
    expired_at TEXT
);

CREATE INDEX idx_leases_open ON leases (project_id, role, acked_at, expired_at);

CREATE TABLE lease_items (
    lease_id   TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL,
    to_role    TEXT NOT NULL,
    PRIMARY KEY (lease_id, message_id, to_role)
);

-- A human gate. A held route becomes queued only when someone approves it.
CREATE TABLE approvals (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    state      TEXT NOT NULL CHECK (state IN ('pending','approved','rejected')),
    note       TEXT,
    created_at TEXT NOT NULL,
    decided_at TEXT
);

CREATE INDEX idx_approvals_pending ON approvals (project_id, state);

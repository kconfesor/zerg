-- zerg schema, version 3: clarifications.
--
-- An agent that needs a human answer must have somewhere to put the question.
-- Telling agents not to ask in their pane and to use a helper script instead
-- means a question with no answer looks exactly like an agent that stopped for
-- no reason.

CREATE TABLE clarifications (
    id         TEXT NOT NULL PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id    TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    role       TEXT NOT NULL,
    question   TEXT NOT NULL,
    answer     TEXT,
    state      TEXT NOT NULL CHECK (state IN ('open','answered','cancelled')),
    created_at TEXT NOT NULL,
    answered_at TEXT
);

CREATE INDEX idx_clarifications_open ON clarifications (project_id, state);

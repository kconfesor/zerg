-- zerg schema, version 5: usage per turn.
--
-- Harnesses report tokens and cost on every turn. Until now the event carrying
-- them was logged to stderr and dropped, so a completed task left no record of
-- what it cost — the first real run spent real money and the only surviving
-- trace was a line in a log file that scrolled past.
--
-- Input is stored in three columns rather than one, because the three are
-- priced roughly 50x apart (cache read ~0.1x, uncached 1x, cache write
-- 1.25-2x). Summing them into "input tokens" would misstate the bill by an
-- order of magnitude, and would hide the thing worth watching: a prompt whose
-- cache stops hitting gets quietly ~10x more expensive per turn with no other
-- symptom.
CREATE TABLE usage_turns (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id     TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    role        TEXT NOT NULL,
    ts          TEXT NOT NULL,

    harness     TEXT NOT NULL DEFAULT '',
    provider    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',

    input_tokens       INTEGER NOT NULL DEFAULT 0,  -- uncached only
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,

    cost_usd    REAL NOT NULL DEFAULT 0,

    -- Whether the harness stated this cost or zerg derived it from a price
    -- table. Keeping the two distinguishable means a disagreement stays
    -- visible rather than being averaged into a number nobody can source.
    cost_source TEXT NOT NULL DEFAULT 'harness',

    -- A subscription turn has a real token cost and no marginal dollar cost.
    -- Mixing the two into one total makes both meaningless, so the dashboard
    -- separates them and needs this to do it.
    billing     TEXT NOT NULL DEFAULT ''
);

-- The three questions the dashboard asks: what did this project cost over a
-- period, what did this task cost, and where did a role's spend go.
CREATE INDEX usage_turns_project_ts ON usage_turns(project_id, ts);
CREATE INDEX usage_turns_task ON usage_turns(task_id);
CREATE INDEX usage_turns_role ON usage_turns(project_id, role, ts);

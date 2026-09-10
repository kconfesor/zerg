-- zerg schema, version 40: an inert plan. Issue #40, phase 2.
--
-- A feature is split into rows the queue will later read, and a document a
-- person reads. Neither is bent to serve the other. The digest of the rows
-- lives on the revision and the prose commit beside it, so a plan whose rows
-- moved after the prose was written is detectable rather than merely unlikely.
--
-- Revisions are immutable. Rejecting produces a new one rather than an edit,
-- which is what makes "the review measures work against something the
-- architect actually reasoned about" true rather than a convention.
--
-- No child tasks, no routes, no feature branch. Creating those is the step
-- that spends the money, and it waits on the operator accepting this. An
-- approval is not an `approvals` row: those are tied to a message and a route,
-- and a plan is tied to neither.
--
-- ON DELETE CASCADE from the feature, so deleting the grouping row does not
-- leave plan rows pointing at nothing. Items and deps cascade from the
-- revision the same way.
CREATE TABLE feature_plan_revisions (
    id               TEXT    PRIMARY KEY,
    feature_id       TEXT    NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    n                INTEGER NOT NULL,
    digest           TEXT    NOT NULL,
    prose_sha        TEXT    NOT NULL DEFAULT '',
    state            TEXT    NOT NULL CHECK (state IN ('pending', 'approved', 'rejected')),
    item_count       INTEGER NOT NULL,
    estimate_tokens  INTEGER NOT NULL DEFAULT 0,
    estimate_cost_usd REAL   NOT NULL DEFAULT 0,
    note             TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL,
    decided_at       TEXT,
    decided_by       TEXT    NOT NULL DEFAULT '',
    UNIQUE (feature_id, n)
);

CREATE INDEX idx_plan_revisions_feature ON feature_plan_revisions (feature_id, n);

CREATE TABLE feature_plan_items (
    id          TEXT    PRIMARY KEY,
    revision_id TEXT    NOT NULL REFERENCES feature_plan_revisions(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    name        TEXT    NOT NULL,
    body        TEXT    NOT NULL DEFAULT '',
    priority    INTEGER NOT NULL DEFAULT 50,
    UNIQUE (revision_id, position),
    UNIQUE (revision_id, name)
);

CREATE TABLE feature_plan_deps (
    revision_id TEXT NOT NULL REFERENCES feature_plan_revisions(id) ON DELETE CASCADE,
    from_item   TEXT NOT NULL REFERENCES feature_plan_items(id) ON DELETE CASCADE,
    to_item     TEXT NOT NULL REFERENCES feature_plan_items(id) ON DELETE CASCADE,
    PRIMARY KEY (revision_id, from_item, to_item),
    CHECK (from_item <> to_item)
);

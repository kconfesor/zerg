-- The agent's orientation for one approval: what the change is for, what each
-- file contributes, and where to start reading.
--
-- Derived rather than authored, which is why it is keyed by the commit it
-- described: a rejection and a new revision make the old guide a description
-- of a diff nobody is looking at any more, and the mismatch is how the UI
-- knows to offer a fresh one instead of serving the stale one.
CREATE TABLE review_guides (
    approval_id TEXT NOT NULL PRIMARY KEY REFERENCES approvals(id) ON DELETE CASCADE,
    commit_sha  TEXT NOT NULL,
    body        TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

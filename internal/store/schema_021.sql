-- zerg schema, version 21: a review is a conversation, anchored to the code.

-- Rejecting was one free-text note for a whole diff, and the exchange ended
-- there: the note travelled back with the card and whatever the role wrote in
-- reply arrived as a fresh handoff with no relation to what was asked. Nothing
-- recorded which file a remark was about, whether it had been answered, or
-- whether the answer settled it.
--
-- Its own tables rather than a shape squeezed into a message body. The anchor
-- and the state are the whole point: "which threads are still open on this
-- card" decides whether the work may merge, and that has to be a query rather
-- than a sentence somebody parses back out. This project has just finished
-- removing one of those, where a pull request's URL lived in prose.
CREATE TABLE review_threads (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,

    -- The card being reviewed. A thread is part of that card's record and goes
    -- with it: deleting a task already takes its messages and its transcript.
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,

    -- The approval the review was opened at, when it was opened at one. Kept
    -- as a reference rather than a parent: the card outlives the decision, and
    -- a thread opened at one gate is still worth reading at the next.
    approval_id TEXT REFERENCES approvals(id) ON DELETE SET NULL,

    -- Where the remark points: the commit the reviewer was reading, the file,
    -- and the line within it. Line 0 means the file as a whole, which is what
    -- a remark about a file's existence or its absence needs.
    commit_sha  TEXT NOT NULL DEFAULT '',
    file        TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,

    state       TEXT NOT NULL CHECK (state IN ('open', 'resolved')),
    created_at  TEXT NOT NULL,
    resolved_at TEXT
);

-- The question the gate asks: has this card anything still open on it.
CREATE INDEX review_threads_open ON review_threads (task_id, state);

CREATE TABLE review_comments (
    id         TEXT PRIMARY KEY,
    thread_id  TEXT NOT NULL REFERENCES review_threads(id) ON DELETE CASCADE,

    -- Who wrote it: the operator, a role, or the agent that answered a question
    -- about a hunk. Free text rather than a foreign key, because a role can be
    -- renamed or removed from the library and the record of what it said in a
    -- review should survive that.
    author     TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX review_comments_thread ON review_comments (thread_id, created_at);

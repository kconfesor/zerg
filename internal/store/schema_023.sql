-- zerg schema, version 23: where you got to in a review.

-- A twelve-file diff is one long scroll and no sense of progress, and an
-- approval is read in the gaps: on a phone, on the way somewhere, and finished
-- later at a desk. Without a record of what has been read, the second sitting
-- starts at the top again.
--
-- Per approval rather than per commit: the same commit reviewed at a different
-- gate is a different reading, and an approval is the thing being decided.
-- Rows go when the approval does, which is when the card is deleted or the
-- decision is made and the message it hangs on is gone.
CREATE TABLE review_seen (
    approval_id TEXT NOT NULL REFERENCES approvals(id) ON DELETE CASCADE,
    file        TEXT NOT NULL,
    seen_at     TEXT NOT NULL,
    PRIMARY KEY (approval_id, file)
);

-- zerg schema, version 22: a question is not a remark.

-- A review thread blocks the merge until somebody settles it, which is right
-- for "this loops forever" and wrong for "why is this recursive?". The second
-- is a person reading the change with help; making it an obligation would mean
-- that asking anything costs you a click to dismiss it, and a reviewer who
-- learns that stops asking.
--
-- So a thread says which it is. A remark is the reviewer's, and holds the gate.
-- A question is the reviewer's too, but it is asked of an agent and answered by
-- one, and it holds nothing. What turns an answer into an obligation is the
-- person deciding it should: raising a question makes it a remark.
ALTER TABLE review_threads ADD COLUMN kind TEXT NOT NULL DEFAULT 'remark'
    CHECK (kind IN ('remark', 'question'));

-- The gate's question, now narrowed to the threads that hold it.
DROP INDEX IF EXISTS review_threads_open;
CREATE INDEX review_threads_open ON review_threads (task_id, kind, state);

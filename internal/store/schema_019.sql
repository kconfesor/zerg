-- zerg schema, version 19: how a task ended, recorded rather than reconstructed.

-- A finished card said only that it was done. What happened to the work was
-- either thrown away or written into prose: nydus appends "Pull request: <url>"
-- to the terminal handoff's note, and a merge left nothing at all. Reading that
-- back means parsing a sentence, or asking the project how it integrates today,
-- which is a different answer from how it integrated then, since that setting
-- can be changed at any time.
--
-- outcome is what happened when the last role finished: merged, pr, branch.
-- outcome_ref is where the work went with it: the commit that was merged, or
-- the pull request's URL. Empty means a card that has not ended, or one that
-- ended before this column existed.
--
-- A card a person stopped is not given an outcome here. stopped_at already
-- records that, and it is a different event: work parked, not work delivered.
ALTER TABLE tasks ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN outcome_ref TEXT NOT NULL DEFAULT '';

-- The one case the old prose can be read back with confidence: the URL nydus
-- appended to a terminal handoff. Everything else stays empty rather than
-- guessed, because a merge and a branch left the same trace, which is none.
--
-- Parsing prose is what this column exists to stop; doing it once, here, is the
-- difference between a history that starts with what the database already knows
-- and one that starts blank.
UPDATE tasks
   SET outcome = 'pr',
       outcome_ref = (
           SELECT trim(substr(m.body, instr(m.body, 'Pull request: ') + 14))
             FROM messages m
            WHERE m.task_id = tasks.id AND m.terminal = 1
              AND instr(m.body, 'Pull request: ') > 0
            ORDER BY m.created_at DESC LIMIT 1)
 WHERE state = 'done'
   AND EXISTS (SELECT 1 FROM messages m
                WHERE m.task_id = tasks.id AND m.terminal = 1
                  AND instr(m.body, 'Pull request: ') > 0);

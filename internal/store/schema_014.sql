-- zerg schema, version 14: a card a person stopped is not a card a role rejected.
--
-- Both were written as state='rejected', so the board could not tell "the
-- reviewer turned this down" from "somebody parked it", and the card said the
-- wrong thing about its own history.
--
-- Recorded as a timestamp beside the state rather than as a new state value.
-- The states live in a CHECK constraint, and changing one in SQLite means
-- rebuilding the table — which here means DROP TABLE tasks, which with foreign
-- keys on is an implicit DELETE that cascades through messages, events, usage
-- and clarifications. Trading every transcript in the database for one enum
-- value is not a trade. This also answers "when", which the state never could.
--
-- Old rows keep a NULL: the two cases were never distinguished when they were
-- written, and inventing a history is worse than an old card reading the old
-- way.

ALTER TABLE tasks ADD COLUMN stopped_at TEXT;

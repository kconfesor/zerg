-- zerg schema, version 10: which harness answers questions.
--
-- Chat inherited the terminal role's harness and model, on the reasoning that
-- the role reviewing everything is the best-informed choice. That is a good
-- default and a poor rule: the reviewer is usually the most expensive model on
-- the team, and asking where a function lives does not need it. It is also the
-- wrong shape when the reviewer runs on a harness whose strengths are not
-- reading.
--
-- Empty means inherit, so the default is unchanged and nothing has to be set.
ALTER TABLE projects ADD COLUMN chat_harness TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN chat_model TEXT NOT NULL DEFAULT '';

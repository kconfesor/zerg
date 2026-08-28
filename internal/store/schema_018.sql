-- zerg schema, version 18: a role knows whether it ends a pipeline.

-- Migration 17 put a terminal flag on each team's role, so a pipeline chose its
-- own finisher and carried a control for it. That is a control most pipelines
-- never need: a reviewer or a cleaner ends the work wherever it appears, and a
-- planner never does, so the role is what knows. The team goes back to
-- finishing at its last enabled role, and what keeps a role added later from
-- taking that job is where it is placed when it joins: one that ends pipelines
-- goes to the end, and everything else goes in front of it.
--
-- 17 is left as it was rather than rewritten, because it had already run. A
-- database at user_version 17 has had the old text applied, and editing it
-- means that database never receives the change: this one came up refusing to
-- start, with "no such column: finisher", which is the failure that rule is
-- written against.
ALTER TABLE role_templates ADD COLUMN finisher INTEGER NOT NULL DEFAULT 0;

-- The two built-ins that end a pipeline. Named rather than matched on anything
-- clever, and only the built-ins: a role someone wrote is theirs to flag.
UPDATE role_templates SET finisher = 1 WHERE builtin = 1 AND name IN ('reviewer', 'cleaner');

-- And 17's column goes, unused. Nothing references it, it carries no index, and
-- leaving a dead flag next to a live one is how the next reader ends up
-- wondering which of them decides.
ALTER TABLE team_preset_roles DROP COLUMN terminal;

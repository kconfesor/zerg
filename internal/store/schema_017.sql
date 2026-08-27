-- zerg schema, version 17: how hard a role thinks, and which role finishes.

-- Thinking level, per role and overridable per team and per project, the same
-- three layers every other role field has. Both harnesses take one and call it
-- something different: claude --effort (low, medium, high, xhigh, max), pi
-- --thinking (off, minimal, low, medium, high, xhigh, max). Empty means the
-- harness's own default, which is what every role had until now.
--
-- Free text rather than a CHECK: the levels are the harness's to name, a new
-- one should not need a migration, and the level set differs between the two
-- already shipped. The picker offers what the adapter reports and preflight is
-- where a level a harness will not take gets reported with a remedy.
ALTER TABLE role_templates ADD COLUMN thinking TEXT NOT NULL DEFAULT '';
ALTER TABLE team_preset_roles ADD COLUMN thinking_override TEXT;
ALTER TABLE project_role_overrides ADD COLUMN thinking_override TEXT;

-- Which role finishes the work, as a flag rather than a position.
--
-- Terminality was the last enabled role in the pipeline, which reads well until
-- you add a role: the new one landed at the end and quietly took over
-- integrating, so the role that had been merging to the base branch stopped and
-- something that had never done it started. As a flag, the finisher is chosen
-- and stays chosen, and adding a role puts it before that one.
ALTER TABLE team_preset_roles ADD COLUMN terminal INTEGER NOT NULL DEFAULT 0;

-- Every team keeps the finisher it has today: its last enabled role.
UPDATE team_preset_roles
   SET terminal = 1
 WHERE enabled = 1
   AND position = (SELECT max(t.position) FROM team_preset_roles t
                    WHERE t.preset_id = team_preset_roles.preset_id AND t.enabled = 1);

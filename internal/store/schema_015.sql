-- zerg schema, version 15: a team can belong to one project.

-- Teams were global, so every team was every project's business: a pipeline
-- built around one repository's prompts, models and skills showed up in the
-- picker of a repository that has nothing to do with it, and editing it there
-- changed the first one. Ownership is what separates them. NULL means shared,
-- which is what Default is and what every team that already exists stays.
--
-- Added as a column rather than by rebuilding the table. team_preset_roles
-- cascades from here and projects.team_preset_id points at it, so a rebuild is
-- an implicit delete of every team's roles and every project's assignment.
--
-- ON DELETE CASCADE: a team that belongs to a deleted project goes with it.
-- Nothing else can be on that team, since a project may only select a team that
-- is shared or its own, so this cannot strand another project's pipeline.
ALTER TABLE team_presets ADD COLUMN project_id TEXT REFERENCES projects(id) ON DELETE CASCADE;

CREATE INDEX team_presets_project ON team_presets(project_id);

-- zerg schema, version 16: every project runs a team, and only a team.

-- The topology override is gone. It let a project freeze its own membership and
-- order while still naming a team, so a project could read as "on Calc pipeline"
-- and run something else, with the rail and the Team screen each describing a
-- different layer and neither saying so. Now that a team can belong to a project
-- (schema 15), a pipeline of one project's own is a team with an owner: in the
-- list, nameable, and edited in one place.
--
-- What a project keeps is its per-role *settings* layer, project_role_overrides,
-- which is a different thing: this repository's coder on a stronger model,
-- without a team of its own for it.

-- Which projects need a team made for them, worked out once because three
-- statements need the same answer and the condition is not short.
--
-- Two ways to need one: running a frozen shape that is not the shape of the team
-- named, or naming no team at all. A frozen shape identical to its team's is a
-- layer that was doing nothing, and those projects keep the team they are
-- already on rather than collecting a duplicate of it.
--
-- A project with the override on and no roles under it is included, and gets an
-- empty team. That pipeline ran nothing, deliberately, and leaving it out sent
-- it back to whichever team it was nominally on, so a database where nothing
-- ran came up running a coder and a reviewer.
CREATE TABLE zerg_016_own_pipeline AS
SELECT p.id AS project_id, 'own-' || p.id AS preset_id, '' AS name
FROM projects p
WHERE (p.team_topology_override = 1 OR p.team_preset_id IS NULL)
  AND NOT (
        p.team_preset_id IS NOT NULL
    AND (SELECT count(*) FROM project_roles r WHERE r.project_id = p.id)
      = (SELECT count(*) FROM team_preset_roles t WHERE t.preset_id = p.team_preset_id)
    AND NOT EXISTS (
          SELECT 1 FROM project_roles r
          WHERE r.project_id = p.id
            AND NOT EXISTS (
                  SELECT 1 FROM team_preset_roles t
                  WHERE t.preset_id = p.team_preset_id
                    AND t.template_id = r.template_id
                    AND t.position = r.position
                    AND t.enabled = r.enabled)
        )
  );

-- Naming them, in a second pass so the choice can be made against the other
-- rows of this table as well as against the teams already there.
--
-- Three candidates, each checked against both. The plain name is what anyone
-- would want; the id's tail disambiguates the ordinary collision; the whole id
-- is the one nothing else can be holding, and exists because the alternative to
-- a third candidate is a UNIQUE violation inside a migration, which is a daemon
-- that will not open the database at all rather than a team with an awkward
-- name.
UPDATE zerg_016_own_pipeline
   SET name = (
       SELECT CASE
           WHEN NOT EXISTS (SELECT 1 FROM team_presets t WHERE t.name = p.name || ' team')
            AND NOT EXISTS (SELECT 1 FROM zerg_016_own_pipeline o
                             JOIN projects q ON q.id = o.project_id
                            WHERE o.project_id < p.id AND q.name = p.name)
           THEN p.name || ' team'

           WHEN NOT EXISTS (SELECT 1 FROM team_presets t WHERE t.name = p.name || ' team ' || substr(p.id, -6))
            AND NOT EXISTS (SELECT 1 FROM zerg_016_own_pipeline o
                             JOIN projects q ON q.id = o.project_id
                            WHERE o.project_id < p.id
                              AND q.name = p.name
                              AND substr(q.id, -6) = substr(p.id, -6))
           THEN p.name || ' team ' || substr(p.id, -6)

           ELSE p.name || ' team ' || p.id
       END
       FROM projects p WHERE p.id = zerg_016_own_pipeline.project_id);

INSERT INTO team_presets (id, name, builtin, project_id, created_at, updated_at)
SELECT o.preset_id, o.name, 0, o.project_id,
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
FROM zerg_016_own_pipeline o;

-- The shape comes from project_roles and the per-role settings from the team the
-- project was naming: resolving applied that team's overrides to roles it also
-- had, even while the order was the project's, so dropping them here would
-- quietly change the model or prompt a role runs with. The project's own
-- override layer is untouched and still applies on top of this.
INSERT INTO team_preset_roles (
    preset_id, template_id, position, enabled,
    harness_override, model_override, args_override, receive_override,
    batch_max_items_override, batch_max_age_sec_override, prompt_override, gate_override)
SELECT
    o.preset_id, r.template_id, r.position, r.enabled,
    old.harness_override, old.model_override, old.args_override, old.receive_override,
    old.batch_max_items_override, old.batch_max_age_sec_override, old.prompt_override, old.gate_override
FROM zerg_016_own_pipeline o
JOIN projects p ON p.id = o.project_id
JOIN project_roles r ON r.project_id = o.project_id
LEFT JOIN team_preset_roles old
       ON old.preset_id = p.team_preset_id AND old.template_id = r.template_id;

UPDATE projects
   SET team_preset_id = (SELECT o.preset_id FROM zerg_016_own_pipeline o WHERE o.project_id = projects.id)
 WHERE EXISTS (SELECT 1 FROM zerg_016_own_pipeline o WHERE o.project_id = projects.id);

-- A project with no team and nothing to make one from starts where a new project
-- starts. Reachable by a project whose override was on with no roles under it,
-- which resolved to a pipeline of nothing.
UPDATE projects
   SET team_preset_id = 'builtin-default-team'
 WHERE team_preset_id IS NULL;

DROP TABLE zerg_016_own_pipeline;

-- Safe to drop: nothing references project_roles, it is the child in both of its
-- foreign keys, and every row that meant anything has been copied above. The
-- rule about not rebuilding tables is about the parents others cascade from,
-- which this is not.
DROP TABLE project_roles;

ALTER TABLE projects DROP COLUMN team_topology_override;

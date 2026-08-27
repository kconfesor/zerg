/**
 * Turning the team on screen back into something the daemon will accept.
 *
 * Two screens edit a project's pipeline now — the Team editor and the rail —
 * and both have to rebuild the whole team to change one thing, because
 * SetProjectTeam replaces the topology and the override layer wholesale rather
 * than diffing them. Sending an incomplete list does not mean "leave the rest
 * alone", it means "delete the rest", so the reconstruction is the part worth
 * having in one place with a test on it.
 */
import type {
  ProjectRole,
  ProjectTeamUpdate,
  ResolvedRole,
  RoleOverrides,
  TeamPreset,
} from '@/lib/api'

/**
 * A resolved role carries the project's own override layer and nothing above
 * it — resolveLayeredTeam applies the preset's overrides to the template and
 * then reports the project's separately — so copying these fields back out
 * round-trips them without freezing the team's overrides into the project.
 */
export function cloneOverrides(source: Partial<RoleOverrides>): RoleOverrides {
  return {
    harnessOverride: source.harnessOverride ?? null,
    modelOverride: source.modelOverride ?? null,
    argsOverride: source.argsOverride == null ? null : [...source.argsOverride],
    receiveOverride: source.receiveOverride ?? null,
    batchMaxItemsOverride: source.batchMaxItemsOverride ?? null,
    batchMaxAgeSecOverride: source.batchMaxAgeSecOverride ?? null,
    promptOverride: source.promptOverride ?? null,
    gateOverride: source.gateOverride ?? null,
  }
}

export function hasRoleOverrides(overrides: Partial<RoleOverrides>): boolean {
  return (
    overrides.harnessOverride != null ||
    overrides.modelOverride != null ||
    overrides.argsOverride != null ||
    overrides.receiveOverride != null ||
    overrides.batchMaxItemsOverride != null ||
    overrides.batchMaxAgeSecOverride != null ||
    overrides.promptOverride != null ||
    overrides.gateOverride != null
  )
}

/** The team as the daemon takes it: order is position, overrides come along. */
export function projectRoles(team: ResolvedRole[]): ProjectRole[] {
  return team.map((role) => ({
    templateId: role.id,
    enabled: role.enabled,
    ...cloneOverrides(role),
  }))
}

/**
 * A pipeline of this project's own, keeping the team it came from.
 *
 * The preset stays selected: its per-role overrides still apply, so this is
 * "that team, without the planner" rather than a copy that stops tracking it
 * everywhere else. What it costs is that later changes to the team's *shape*
 * no longer reach this project, which is why the rail says so and offers
 * followPreset back.
 */
export function ownPipeline(presetId: string | null, team: ResolvedRole[]): ProjectTeamUpdate {
  return { presetId, topologyOverride: true, roles: projectRoles(team) }
}

/**
 * Following a team's pipeline again, or adopting it for the first time.
 *
 * The project's own overrides come with it. An empty roles array here would
 * delete every project_role_overrides row, and pressing this on the team
 * already in use is exactly when a project has overrides worth keeping, including
every one migration 013 carried across from the old project_roles columns.
 * Filtered to the preset's own roles, since SetProjectTeam refuses an override
 * for a role the preset does not contain, and adopting a team that drops a role
 * legitimately drops that role's overrides with it.
 */
export function followPreset(preset: TeamPreset, team: ResolvedRole[]): ProjectTeamUpdate {
  const inPreset = new Set(preset.roles.map((r) => r.templateId))
  return {
    presetId: preset.id,
    topologyOverride: false,
    roles: team
      .filter((role) => inPreset.has(role.id) && hasRoleOverrides(role))
      .map((role) => ({ templateId: role.id, enabled: role.enabled, ...cloneOverrides(role) })),
  }
}

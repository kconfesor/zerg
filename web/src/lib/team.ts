/**
 * Turning the pipeline on screen back into rows the daemon will accept.
 *
 * Both writers rebuild the whole thing to change one part of it: SetProjectTeam
 * replaces a project's topology and override layer wholesale, and a team update
 * replaces its roles. Sending an incomplete list does not mean "leave the rest
 * alone", it means "delete the rest", so the reconstruction is worth having in
 * one place with a test on it.
 */
import type {
  ProjectTeamUpdate,
  RoleTemplate,
  ResolvedRole,
  RoleOverrides,
  TeamPreset,
  TeamPresetRole,
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
    thinkingOverride: source.thinkingOverride ?? null,
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
    overrides.thinkingOverride != null ||
    overrides.receiveOverride != null ||
    overrides.batchMaxItemsOverride != null ||
    overrides.batchMaxAgeSecOverride != null ||
    overrides.promptOverride != null ||
    overrides.gateOverride != null
  )
}

/**
 * A pipeline on screen turned back into a team's rows.
 *
 * The order and the enabled flags come from what is displayed, and the per-role
 * overrides from the team it is being written to or copied from, matched by
 * template. A resolved role carries the *project's* override layer, not the
 * team's, so building these out of the resolved rows alone would quietly drop
 * the model and prompt a team had chosen for each of its roles.
 */
export function presetRoles(pipeline: ResolvedRole[], source: TeamPreset | null): TeamPresetRole[] {
  const overrides = new Map((source?.roles ?? []).map((r) => [r.templateId, r]))
  return pipeline.map((role, position) => ({
    templateId: role.id,
    position,
    enabled: role.enabled,
    ...cloneOverrides(overrides.get(role.id) ?? {}),
  }))
}

/**
/**
 * Putting a project on a team, keeping the settings it overrode.
 *
 * An empty roles array would delete every project_role_overrides row, and this
 * is often pressed on the team already in use, which is exactly when a project
 * has overrides worth keeping. Filtered to the team's own roles, since the
 * daemon refuses an override for a role the team does not have, and moving to a
 * team without a role legitimately drops that role's settings with it.
 */
export function followPreset(preset: TeamPreset, team: ResolvedRole[]): ProjectTeamUpdate {
  const inPreset = new Set(preset.roles.map((r) => r.templateId))
  return {
    presetId: preset.id,
    roles: team
      .filter((role) => inPreset.has(role.id) && hasRoleOverrides(role))
      .map((role) => ({ templateId: role.id, ...cloneOverrides(role) })),
  }
}

/**
 * Where a role goes when it joins a pipeline.
 *
 * Work ends at the last enabled role, so appending blindly hands the job of
 * integrating to whatever was added most recently, taking it from the role that
 * had been doing it. A role that ends pipelines says so about itself
 * (`finisher`), so a reviewer or a cleaner goes to the end and anything else
 * goes in front of the ones already there. Nothing about this is a control the
 * pipeline has to carry.
 *
 * Returns the index to insert at.
 */
export function placeInPipeline(
  pipeline: { id: string }[],
  joining: RoleTemplate,
  library: RoleTemplate[],
): number {
  if (joining.finisher) return pipeline.length
  const finishers = new Set(library.filter((t) => t.finisher).map((t) => t.id))
  let at = pipeline.length
  while (at > 0 && finishers.has(pipeline[at - 1].id)) at--
  return at
}

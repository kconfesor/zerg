import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import AppSidebar from './AppSidebar.vue'
import type {
  Project,
  ProjectTeam,
  ProjectTeamUpdate,
  ResolvedRole,
  RoleStatus,
  RoleTemplate,
  SwarmStatus,
  TeamPreset,
} from '@/lib/api'

const project: Project = {
  id: '01M0VPTPS4TY4MC5MJPH0S4MJ2',
  path: '/src/calc2',
  name: 'CalcRust',
  baseBranch: 'main',
  integration: 'merge',
  prDraft: false,
  createdAt: '2026-01-01T00:00:00Z',
}

function role(name: string, over: Partial<ResolvedRole> = {}): ResolvedRole {
  return {
    id: `tpl-${name}`,
    name,
    harness: 'claude',
    model: 'sonnet',
    args: [],
    receive: 'task',
    batchMaxItems: 8,
    batchMaxAgeSec: 300,
    prompt: '',
    gate: 'none',
    builtin: true,
    position: 0,
    enabled: true,
    overridden: false,
    terminal: false,
    ...over,
    argsOverride: over.argsOverride ?? null,
  }
}

function live(name: string, state: string): RoleStatus {
  return { role: name, harness: 'claude', model: 'sonnet', state, restarts: 0, terminal: false }
}

function template(name: string): RoleTemplate {
  const { id, harness, model, args, receive, batchMaxItems, batchMaxAgeSec, prompt, gate, builtin } =
    role(name)
  return { id, name, harness, model, args, receive, batchMaxItems, batchMaxAgeSec, prompt, gate, builtin }
}

/** A team preset made of the roles named, in that order. */
function preset(id: string, name: string, names: string[]): TeamPreset {
  return {
    id,
    name,
    builtin: false,
    projectId: null,
    roles: names.map((n, position) => ({
      templateId: `tpl-${n}`,
      position,
      enabled: true,
      argsOverride: null,
    })),
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

function sidebar(
  status: SwarmStatus,
  team: ResolvedRole[],
  teamName = 'Calc pipeline',
  extra: {
    library?: RoleTemplate[]
    projectTeam?: Partial<ProjectTeam>
    preset?: TeamPreset | null
  } = {},
) {
  return mount(AppSidebar, {
    props: {
      view: 'board' as const,
      status,
      taskCount: 0,
      projectCount: 1,
      projects: [project],
      current: project,
      teamName,
      team,
      library: extra.library ?? team.map((r) => template(r.name)),
      projectTeam: {
        presetId: extra.preset === undefined ? null : (extra.preset?.id ?? null),
        topologyOverride: true,
        roles: team,
        ...extra.projectTeam,
      },
      preset: extra.preset ?? null,
      open: false,
    },
  })
}

/** The update the rail asked for, or a failure naming what it emitted instead. */
function emitted(w: ReturnType<typeof sidebar>): ProjectTeamUpdate {
  const updates = w.emitted('setTeam')
  if (!updates?.length) throw new Error('the rail emitted no team update')
  return updates[updates.length - 1][0] as ProjectTeamUpdate
}

async function edit(w: ReturnType<typeof sidebar>) {
  await w.get('[aria-label="Edit this pipeline"]').trigger('click')
  return w
}

describe('AppSidebar', () => {
  it('heads the role list with the team those roles belong to', () => {
    const w = sidebar({ running: true, roles: [live('coder', 'ready')] }, [role('coder')])
    expect(w.text()).toContain('Calc pipeline')
    expect(w.text()).toContain('coder')
    expect(w.text()).toContain('ready')
  })

  it("lists the team's roles when no swarm is running", () => {
    // The status carries no roles until something starts, so without the
    // fallback this panel is empty exactly when you are deciding whether to
    // start it.
    const w = sidebar({ running: false, roles: [] }, [
      role('coder', { position: 0 }),
      role('docs', { position: 1 }),
      role('parked', { position: 2, enabled: false }),
    ])
    expect(w.text()).toContain('Calc pipeline')
    expect(w.text()).toContain('coder')
    expect(w.text()).toContain('not started')
    // A role the team has turned off is not one of them.
    expect(w.text()).not.toContain('parked')
  })

  it('falls back to a plain heading when the project has no named team', () => {
    const w = sidebar({ running: false, roles: [] }, [role('coder')], '')
    expect(w.text()).toContain('Roles')
  })

  it('lists the roles that are off once you are editing them', async () => {
    // The read-only list is what is running, so it filters those out. The
    // editor is what you open to turn one back on, so it cannot.
    const w = await edit(
      sidebar({ running: false, roles: [] }, [
        role('coder', { position: 0 }),
        role('parked', { position: 1, enabled: false }),
      ]),
    )
    expect(w.text()).toContain('parked')
  })

  it('turns a role off for this project without touching the team it follows', async () => {
    const calc = preset('preset-calc', 'Calc pipeline', ['planner', 'coder'])
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('planner', { position: 0 }), role('coder', { position: 1 })],
        'Calc pipeline',
        { preset: calc, projectTeam: { presetId: calc.id, topologyOverride: false } },
      ),
    )
    await w.get('[aria-checked="true"]').trigger('click')

    const update = emitted(w)
    // The preset stays selected, so its per-role overrides still apply and this
    // is "Calc pipeline without the planner" rather than a detached copy. What
    // changes is that the pipeline is now the project's own.
    expect(update.presetId).toBe(calc.id)
    expect(update.topologyOverride).toBe(true)
    expect(update.roles.map((r) => [r.templateId, r.enabled])).toEqual([
      ['tpl-planner', false],
      ['tpl-coder', true],
    ])
  })

  it('refuses to turn off the only role that is left', async () => {
    // A team with nothing enabled cannot start and has nowhere to route a task,
    // which looks like a configured project that silently takes no work.
    const w = await edit(
      sidebar({ running: false, roles: [] }, [
        role('coder', { position: 0 }),
        role('parked', { position: 1, enabled: false }),
      ]),
    )
    const on = w.get('[aria-checked="true"]')
    expect(on.attributes('disabled')).toBeDefined()
    await on.trigger('click')
    expect(w.emitted('setTeam')).toBeUndefined()
  })

  it('adds a library role at the end of the pipeline, overrides intact', async () => {
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0, modelOverride: 'opus' })],
        'Calc pipeline',
        { library: [template('coder'), template('debugger')] },
      ),
    )
    await w.findAll('button').find((b) => b.text() === 'Add a role')!.trigger('click')
    await w.findAll('button').find((b) => b.text() === 'debugger')!.trigger('click')

    const update = emitted(w)
    expect(update.roles.map((r) => r.templateId)).toEqual(['tpl-coder', 'tpl-debugger'])
    // Sending the team back rebuilds the whole override layer, so a role's own
    // model has to survive the trip: SetProjectTeam deletes what it is not sent.
    expect(update.roles[0].modelOverride).toBe('opus')
    expect(update.roles[1].enabled).toBe(true)
  })

  it('reorders the pipeline, which is the route work takes', async () => {
    const w = await edit(
      sidebar({ running: false, roles: [] }, [
        role('coder', { position: 0 }),
        role('reviewer', { position: 1 }),
      ]),
    )
    await w.get('[aria-label="Move reviewer earlier"]').trigger('click')
    expect(emitted(w).roles.map((r) => r.templateId)).toEqual(['tpl-reviewer', 'tpl-coder'])
  })

  it('removes only the roles the followed team does not have', async () => {
    // A role the preset itself contains would come back the moment anything
    // re-followed the team, which reads as the button not working. That one is
    // turned off instead.
    const calc = preset('preset-calc', 'Calc pipeline', ['coder'])
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0 }), role('debugger', { position: 1 })],
        'Calc pipeline',
        { preset: calc, projectTeam: { presetId: calc.id } },
      ),
    )
    expect(w.find('[aria-label="Remove coder from this pipeline"]').exists()).toBe(false)
    await w.get('[aria-label="Remove debugger from this pipeline"]').trigger('click')
    expect(emitted(w).roles.map((r) => r.templateId)).toEqual(['tpl-coder'])
  })

  it('says when a project has stopped following its team, and offers the way back', async () => {
    const calc = preset('preset-calc', 'Calc pipeline', ['planner', 'coder'])
    const w = sidebar(
      { running: false, roles: [] },
      [role('planner', { position: 0, enabled: false }), role('coder', { position: 1, promptOverride: 'mine' })],
      'Calc pipeline',
      { preset: calc, projectTeam: { presetId: calc.id, topologyOverride: true } },
    )
    // Visible without opening the editor: changes to the team stop arriving
    // here, and someone reading the rail has to be able to find that out.
    expect(w.text()).toContain('own pipeline')

    await w.get('button[title*="follow Calc pipeline again"]').trigger('click')
    const update = emitted(w)
    expect(update.topologyOverride).toBe(false)
    expect(update.presetId).toBe(calc.id)
    // Following the team again drops the local shape, not the project's own
    // per-role settings.
    expect(update.roles.map((r) => [r.templateId, r.promptOverride])).toEqual([
      ['tpl-coder', 'mine'],
    ])
  })
})

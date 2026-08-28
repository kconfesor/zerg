import { afterEach, describe, expect, it } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import AppSidebar from './AppSidebar.vue'

// The naming dialog teleports to the body, and a wrapper nobody unmounts leaves
// it there: without this, one test presses the previous test's Create button.
enableAutoUnmount(afterEach)
import type {
  Project,
  ProjectTeam,
  ResolvedRole,
  RoleStatus,
  RoleTemplate,
  SwarmStatus,
  TeamPreset,
  TeamPresetRole,
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
    thinking: '',
    receive: 'task',
    batchMaxItems: 8,
    batchMaxAgeSec: 300,
    prompt: '',
    gate: 'none',
    finisher: false,
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
  const { id, harness, model, args, thinking, receive, batchMaxItems, batchMaxAgeSec, prompt, gate, finisher, builtin } =
    role(name)
  return { id, name, harness, model, args, thinking, receive, batchMaxItems, batchMaxAgeSec, prompt, gate, finisher, builtin }
}

/** A team made of the roles named, in that order. Shared unless owned. */
function preset(id: string, name: string, names: string[], owner: string | null = null): TeamPreset {
  return {
    id,
    name,
    builtin: false,
    projectId: owner,
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
    presets?: TeamPreset[]
    forkError?: string
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
        roles: team,
        ...extra.projectTeam,
      },
      preset: extra.preset ?? null,
      presets: extra.presets ?? (extra.preset ? [extra.preset] : []),
      forkError: extra.forkError ?? '',
      open: false,
    },
  })
}

/** The team the rail wrote, or a failure saying it wrote none. */
function saved(w: ReturnType<typeof sidebar>): TeamPreset {
  const writes = w.emitted('savePreset')
  if (!writes?.length) throw new Error('the rail saved no team')
  return writes[writes.length - 1][0] as TeamPreset
}

/** The copy the rail asked for: its name and the pipeline it carries. */
function forked(w: ReturnType<typeof sidebar>): { name: string; roles: TeamPresetRole[] } {
  const forks = w.emitted('forkTeam')
  if (!forks?.length) throw new Error('the rail asked for no copy')
  const [name, roles] = forks[forks.length - 1] as [string, TeamPresetRole[]]
  return { name, roles }
}

/** The naming dialog teleports to the body, so it is read there. */
function dialogButton(text: string): HTMLButtonElement {
  const found = [...document.body.querySelectorAll('button')].find(
    (b) => b.textContent?.trim() === text,
  )
  if (!found) throw new Error(`no ${text} button in ${document.body.textContent}`)
  return found as HTMLButtonElement
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

  it("writes straight into a team this project owns", async () => {
    // The team is the thing that holds a pipeline, and this one belongs to
    // this project, so there is nobody else to surprise.
    const mine = preset('preset-mine', 'Calc pipeline', ['planner', 'coder'], project.id)
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('planner', { position: 0 }), role('coder', { position: 1 })],
        'Calc pipeline',
        { preset: mine },
      ),
    )
    await w.get('[aria-checked="true"]').trigger('click')

    expect(w.emitted('forkTeam')).toBeUndefined()
    const team = saved(w)
    expect(team.id).toBe(mine.id)
    expect(team.roles.map((r) => [r.templateId, r.enabled])).toEqual([
      ['tpl-planner', false],
      ['tpl-coder', true],
    ])
  })

  it('writes in place when this project owns the team', async () => {
    // Reported from testing: editing a team belonging to this project opened
    // the copy dialog, under a sentence saying that team was shared. The
    // project was carrying a topology layer, and that was read as a reason to
    // make a second team out of one the project already had. The layer is gone
    // as of schema 16; owning the team is the whole test.
    const mine = preset('preset-mine', 'Team Credix', ['coder', 'reviewer'], project.id)
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0 }), role('reviewer', { position: 1 })],
        'Team Credix',
        { preset: mine },
      ),
    )
    expect(w.text()).not.toContain('is shared with every project')

    await w.get('[aria-label="Reorder reviewer"]').trigger('keydown.up')
    expect(w.emitted('forkTeam')).toBeUndefined()
    expect(document.body.textContent).not.toContain("Name this project's team")
    expect(saved(w).roles.map((r) => r.templateId)).toEqual(['tpl-reviewer', 'tpl-coder'])
  })

  it('copies a shared team into this project instead of changing it', async () => {
    // Turning the planner off on a shared team would turn it off for every
    // project on that team. The change makes the team this project's first.
    const shared = preset('preset-shared', 'Calc pipeline', ['planner', 'coder'])
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('planner', { position: 0 }), role('coder', { position: 1 })],
        'Calc pipeline',
        { preset: shared },
      ),
    )
    // Said before the click, not after it.
    expect(w.text()).toContain('is shared with every project')

    await w.get('[aria-checked="true"]').trigger('click')
    // Nothing is written until the copy has a name.
    expect(w.emitted('savePreset')).toBeUndefined()
    expect(w.emitted('forkTeam')).toBeUndefined()
    expect(document.body.textContent).toContain("Name this project's team")

    dialogButton('Create team').click()
    await w.vm.$nextTick()

    const fork = forked(w)
    expect(fork.name).toBe('CalcRust team')
    // The copy is what was running plus the change that prompted it.
    expect(fork.roles.map((r) => [r.templateId, r.enabled, r.position])).toEqual([
      ['tpl-planner', false, 0],
      ['tpl-coder', true, 1],
    ])
  })

  it("keeps the shared team's per-role settings in the copy", async () => {
    // A resolved role carries the *project's* override layer, not the team's,
    // so building the copy out of the rows on screen alone would drop the
    // model and prompt the team had chosen for each of its roles.
    const shared = preset('preset-shared', 'Calc pipeline', ['coder', 'reviewer'])
    shared.roles[1] = { ...shared.roles[1], modelOverride: 'opus', promptOverride: 'review hard' }
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0 }), role('reviewer', { position: 1 })],
        'Calc pipeline',
        { preset: shared },
      ),
    )
    await w.get('[aria-label="Reorder reviewer"]').trigger('keydown.up')
    dialogButton('Create team').click()
    await w.vm.$nextTick()

    const fork = forked(w)
    expect(fork.roles.map((r) => r.templateId)).toEqual(['tpl-reviewer', 'tpl-coder'])
    expect(fork.roles[0].modelOverride).toBe('opus')
    expect(fork.roles[0].promptOverride).toBe('review hard')
  })

  it('suggests a name that is not already taken, and asks again on a refusal', async () => {
    const shared = preset('preset-shared', 'Calc pipeline', ['coder', 'reviewer'])
    const taken = preset('preset-taken', 'CalcRust team', ['coder'], project.id)
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0 }), role('reviewer', { position: 1 })],
        'Calc pipeline',
        { preset: shared, presets: [shared, taken], forkError: 'a team called CalcRust team already exists' },
      ),
    )
    await w.get('[aria-label="Reorder reviewer"]').trigger('keydown.up')

    const field = document.body.querySelector('#fork-team-name') as HTMLInputElement
    expect(field.value).toBe('CalcRust team 2')
    // The refusal is rendered in the dialog that asked; the page banner behind
    // it is nowhere on a phone.
    expect(document.body.textContent).toContain('already exists')
  })

  it('drops a copy refusal when the dialog it belonged to is abandoned', async () => {
    // The message outlived its attempt: cancel, edit something else, and the
    // new dialog opened with the old refusal already in it, attached to a name
    // nobody had typed.
    const shared = preset('preset-shared', 'Calc pipeline', ['coder', 'reviewer'])
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0 }), role('reviewer', { position: 1 })],
        'Calc pipeline',
        { preset: shared, forkError: 'a team called CalcRust team already exists' },
      ),
    )
    await w.get('[aria-label="Reorder reviewer"]').trigger('keydown.up')
    dialogButton('Cancel').click()
    await w.vm.$nextTick()
    expect(w.emitted('clearForkError')).toBeTruthy()

    // Opening clears it too, so a refusal cannot precede the attempt it is
    // about: three in all, for the first open, the cancel, and this open.
    await w.get('[aria-label="Reorder reviewer"]').trigger('keydown.up')
    expect(w.emitted('clearForkError')).toHaveLength(3)
    // And while one is showing it is announced, not only coloured.
    expect(document.body.querySelector('[role="alert"]')?.textContent).toContain('already exists')
  })

  it('refuses to turn off or remove the only role that is left', async () => {
    // A team with nothing enabled cannot start and has nowhere to route a task,
    // which looks like a configured project that silently takes no work.
    const mine = preset('preset-mine', 'Calc pipeline', ['coder', 'parked'], project.id)
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0 }), role('parked', { position: 1, enabled: false })],
        'Calc pipeline',
        { preset: mine },
      ),
    )
    const on = w.get('[aria-checked="true"]')
    expect(on.attributes('disabled')).toBeDefined()
    expect(w.get('[aria-label="Remove coder from this pipeline"]').attributes('disabled')).toBeDefined()
    await on.trigger('click')
    expect(w.emitted('savePreset')).toBeUndefined()
  })

  it('composes two quick edits instead of the second undoing the first', async () => {
    // Every edit rebuilds the whole team from props, and props do not change
    // until the daemon has answered and the board has refreshed. Two clicks in
    // that window both start from the same pipeline, so the second sends a team
    // that never had the first change in it.
    const mine = preset('preset-mine', 'Calc pipeline', ['coder', 'reviewer', 'docs'], project.id)
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [
          role('coder', { position: 0 }),
          role('reviewer', { position: 1 }),
          role('docs', { position: 2 }),
        ],
        'Calc pipeline',
        { preset: mine },
      ),
    )

    // Nothing updates the props in between, which is the case being tested.
    await w.get('[aria-label="Reorder docs"]').trigger('keydown.up')
    await w.get('[title="Turn coder off for this project"]').trigger('click')

    const team = saved(w)
    expect(team.roles.map((r) => r.templateId)).toEqual(['tpl-coder', 'tpl-docs', 'tpl-reviewer'])
    expect(team.roles.find((r) => r.templateId === 'tpl-coder')!.enabled).toBe(false)
  })

  it('adds a library role at the end of the pipeline', async () => {
    const mine = preset('preset-mine', 'Calc pipeline', ['coder'], project.id)
    const w = await edit(
      sidebar({ running: false, roles: [] }, [role('coder', { position: 0 })], 'Calc pipeline', {
        preset: mine,
        library: [template('coder'), template('debugger')],
      }),
    )
    await w.findAll('button').find((b) => b.text() === 'Add a role')!.trigger('click')
    await w.findAll('button').find((b) => b.text() === 'debugger')!.trigger('click')

    const team = saved(w)
    expect(team.roles.map((r) => r.templateId)).toEqual(['tpl-coder', 'tpl-debugger'])
    expect(team.roles[1].enabled).toBe(true)
  })

  it('reorders and removes, which is the route work takes', async () => {
    const mine = preset('preset-mine', 'Calc pipeline', ['coder', 'reviewer'], project.id)
    const w = await edit(
      sidebar(
        { running: false, roles: [] },
        [role('coder', { position: 0 }), role('reviewer', { position: 1 })],
        'Calc pipeline',
        { preset: mine },
      ),
    )
    await w.get('[aria-label="Reorder reviewer"]').trigger('keydown.up')
    expect(saved(w).roles.map((r) => r.templateId)).toEqual(['tpl-reviewer', 'tpl-coder'])

    await w.get('[aria-label="Remove reviewer from this pipeline"]').trigger('click')
    expect(saved(w).roles.map((r) => r.templateId)).toEqual(['tpl-coder'])
  })
})

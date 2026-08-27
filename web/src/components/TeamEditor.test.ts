import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import TeamEditor from './TeamEditor.vue'
import type { ProjectTeam, ResolvedRole, RoleTemplate, TeamPreset } from '@/lib/api'

const coder: RoleTemplate = {
  id: 'coder',
  name: 'coder',
  harness: 'claude',
  model: 'sonnet',
  args: [],
  thinking: '',
  receive: 'task',
  batchMaxItems: 8,
  batchMaxAgeSec: 300,
  prompt: 'code',
  gate: 'none',
  builtin: true,
}

const reviewer: RoleTemplate = {
  ...coder,
  id: 'reviewer',
  name: 'reviewer',
  model: 'opus',
  receive: 'batch',
  prompt: 'review',
}

const project = { id: 'project-x', name: 'CalcRust' }

const defaultTeam: TeamPreset = {
  id: 'default',
  name: 'Default',
  builtin: true,
  projectId: null,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
  roles: [
    { templateId: 'coder', position: 0, enabled: true, terminal: false, argsOverride: null },
    { templateId: 'reviewer', position: 1, enabled: true, terminal: true, argsOverride: null },
  ],
}

const docsTeam: TeamPreset = {
  ...defaultTeam,
  id: 'docs-team',
  name: 'Docs team',
  builtin: false,
  roles: [{ templateId: 'reviewer', position: 0, enabled: true, terminal: true, argsOverride: null }],
}

const resolved: ResolvedRole[] = [
  {
    ...coder,
    position: 0,
    enabled: true,
    overridden: false,
    terminal: false,
    argsOverride: null,
  },
  {
    ...reviewer,
    position: 1,
    enabled: true,
    overridden: false,
    terminal: true,
    argsOverride: null,
  },
]

function editor(team: ProjectTeam, presets: TeamPreset[] = [defaultTeam, docsTeam]) {
  return mount(TeamEditor, {
    props: {
      library: [coder, reviewer],
      presets,
      projectId: project.id,
      projectName: project.name,
      projectTeam: team,
      harnesses: ['claude'],
      models: {},
      thinking: { claude: ['low', 'medium', 'high', 'xhigh', 'max'] },
      running: false,
    },
    global: { stubs: { RoleOverrideDialog: true } },
  })
}

describe('TeamEditor', () => {
  it('presents the team, roles and pipeline as three columns', () => {
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    const headings = w.findAll('h2').map((heading) => heading.text())
    expect(headings).toEqual(['Teams', 'Roles', 'Pipeline'])
    expect(w.text()).toContain('Default')
    // Which team is in use is said in words, not with a marker beside the name.
    expect(w.text()).toContain('in use')
    expect(w.findAll('[aria-label="Use this team"]')).toHaveLength(1)
    // The row that finishes says so in words, not in the protocol's noun.
    expect(w.text()).toContain('finishes the task')
  })

  it('offers no adopt button for the team already in use, and one for every other', () => {
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    // Exactly one, for Docs team. The team in use says so with the star and
    // the "in use" line instead of a control that cannot be pressed. The adopt
    // action is an icon now, so it is found by its label rather than its text.
    expect(w.findAll('[aria-label="Use this team"]')).toHaveLength(1)
    expect(w.findAll('button').map((b) => b.text())).not.toContain('In use')
  })

  it('offers no adopt control for the team in use, and says it is in use', () => {
    // "Follow this pipeline again" lived here, for a project that had frozen a
    // copy of its team's shape. Schema 16 removed that layer: a project running
    // its own pipeline is on its own team, so leaving one is adopting another.
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    expect(w.text()).toContain('in use')
    expect(w.find('[aria-label="Follow this pipeline again"]').exists()).toBe(false)
    expect(w.findAll('[aria-label="Use this team"]')).toHaveLength(1)
  })

  it('chooses which role finishes rather than counting off the end', async () => {
    // Terminality was whichever enabled role sat last, so adding a role to the
    // end took the job of integrating from the role that had been doing it.
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    // The row that finishes says so in words, not in the protocol's noun.
    expect(w.text()).toContain('finishes the task')

    await w.get('[aria-label="Make coder the finishing role"]').trigger('click')
    const saved = w.emitted('savePreset')!.at(-1)![0] as TeamPreset
    expect(saved.roles.filter((r) => r.terminal).map((r) => r.templateId)).toEqual(['coder'])
    // The daemon is what moves it to the end and clears the old one, so the
    // team goes out saying only which role it is.
    expect(saved.roles.map((r) => r.templateId)).toEqual(['coder', 'reviewer'])
  })

  it('adds a role without handing it the job of finishing', async () => {
    // Docs team runs the reviewer only, so adding the coder to it appends one.
    // The new role must not be the finisher: that is the whole bug the flag
    // replaced, where the role added last quietly took over integrating.
    const w = editor({ presetId: docsTeam.id, roles: resolved }, [docsTeam])
    await w.get('[aria-label="coder in Docs team"]').trigger('click')

    const saved = w.emitted('savePreset')!.at(-1)![0] as TeamPreset
    expect(saved.roles.map((r) => r.templateId)).toEqual(['reviewer', 'coder'])
    expect(saved.roles.find((r) => r.templateId === 'coder')!.terminal).toBe(false)
    expect(saved.roles.find((r) => r.templateId === 'reviewer')!.terminal).toBe(true)
  })

  it('keeps rename and delete on every row, and off the built-in', () => {
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    // Always rendered rather than revealed on hover: there is no hover on a
    // phone, and this column is the only place a team can be removed.
    expect(w.get('[aria-label="Delete Docs team"]').attributes('disabled')).toBeUndefined()
    expect(w.get('[aria-label="Delete Default"]').attributes('disabled')).toBeDefined()
    expect(w.get('[aria-label="Rename Default"]').attributes('disabled')).toBeDefined()
    expect(w.find('[aria-label="Clone Default"]').exists()).toBe(true)
  })


  it('keeps this project\'s teams apart from the shared ones', () => {
    // A team carries prompts, models and arguments chosen for one repository,
    // and a flat global list put those in front of every other repository,
    // where adopting one was a click and editing it changed the first project.
    const mine: TeamPreset = { ...docsTeam, id: 'mine', name: 'CalcRust review', projectId: project.id }
    const w = editor({ presetId: defaultTeam.id, roles: resolved }, [
      defaultTeam,
      mine,
    ])

    const text = w.text()
    expect(text).toContain('CalcRust only')
    expect(text).toContain('Shared with every project')
    // Its own team is listed under the project, the built-in under shared.
    expect(text.indexOf('CalcRust only')).toBeLessThan(text.indexOf('CalcRust review'))
    expect(text.indexOf('Shared with every project')).toBeLessThan(text.indexOf('Default'))
  })

  it('moves a team between shared and this project, and never the built-in', async () => {
    const mine: TeamPreset = { ...docsTeam, id: 'mine', name: 'CalcRust review', projectId: project.id }
    const w = editor({ presetId: defaultTeam.id, roles: resolved }, [
      defaultTeam,
      docsTeam,
      mine,
    ])

    // Sharing a project's team back strands nobody, so it happens on the press.
    await w.get('[aria-label="Share with every project"]').trigger('click')
    const shared = w.emitted('savePreset')![0][0] as TeamPreset
    expect(shared.id).toBe(mine.id)
    expect(shared.projectId).toBeNull()

    // Default is where a new project starts, so it cannot become one project's.
    expect(w.get('[aria-label="Default is shared"]').attributes('disabled')).toBeDefined()
  })

  it('gives a clone to this project, since that is what cloning a shared team is for', async () => {
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    await w.get('[aria-label="Clone Default"]').trigger('click')
    // The dialog teleports to the body, so the confirm is read there.
    const clone = [...document.body.querySelectorAll('button')].find(
      (b) => b.textContent?.trim() === 'Clone team',
    )
    clone!.click()
    await w.vm.$nextTick()

    const [, , owner] = w.emitted('createPreset')![0] as [string, unknown, string | null]
    expect(owner).toBe(project.id)
  })

  it('asks before moving a project to another team while agents are running', async () => {
    const w = mount(TeamEditor, {
      props: {
        library: [coder, reviewer],
        presets: [defaultTeam, docsTeam],
        projectId: project.id,
        projectName: project.name,
        projectTeam: { presetId: defaultTeam.id, roles: resolved },
        harnesses: ['claude'],
        models: {},
        thinking: { claude: ['low', 'medium', 'high', 'xhigh', 'max'] },
        running: true,
      },
      global: { stubs: { RoleOverrideDialog: true } },
    })
    await w.get('[aria-label="Use this team"]').trigger('click')
    // Asked, not refused, and nothing sent until the question is answered. The
    // dialog teleports to the body, so it is read there rather than off the
    // wrapper.
    expect(w.emitted('setTeam')).toBeUndefined()
    expect(document.body.textContent).toContain('Put this project on Docs team?')
    const confirm = [...document.body.querySelectorAll('button')].find(
      (b) => b.textContent?.trim() === 'Switch team',
    )
    confirm!.click()
    await w.vm.$nextTick()
    expect(w.emitted('setTeam')?.at(-1)?.[0]).toMatchObject({ presetId: docsTeam.id })
  })

  it('selects a team for editing without using it until the explicit button is pressed', async () => {
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    await w.findAll('button').find((button) => button.text().includes('Docs team'))!.trigger('click')

    expect(w.emitted('setTeam')).toBeUndefined()
    await w.get('[aria-label="Use this team"]').trigger('click')
    expect(w.emitted('setTeam')?.at(-1)?.[0]).toEqual({ presetId: docsTeam.id, roles: [] })
  })

  it('reorders the selected team in the pipeline', async () => {
    const w = editor({ presetId: defaultTeam.id, roles: resolved })
    await w.get('[aria-label="Move coder down"]').trigger('click')
    const updated = w.emitted('savePreset')?.at(-1)?.[0] as TeamPreset
    expect(updated.roles.map((role) => role.templateId)).toEqual(['reviewer', 'coder'])
  })
})

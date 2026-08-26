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

const defaultTeam: TeamPreset = {
  id: 'default',
  name: 'Default',
  builtin: true,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
  roles: [
    { templateId: 'coder', position: 0, enabled: true, argsOverride: null },
    { templateId: 'reviewer', position: 1, enabled: true, argsOverride: null },
  ],
}

const docsTeam: TeamPreset = {
  ...defaultTeam,
  id: 'docs-team',
  name: 'Docs team',
  builtin: false,
  roles: [{ templateId: 'reviewer', position: 0, enabled: true, argsOverride: null }],
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

function editor(team: ProjectTeam) {
  return mount(TeamEditor, {
    props: {
      library: [coder, reviewer],
      presets: [defaultTeam, docsTeam],
      projectTeam: team,
      harnesses: ['claude'],
      models: {},
      running: false,
    },
    global: { stubs: { RoleOverrideDialog: true } },
  })
}

describe('TeamEditor', () => {
  it('presents the team, roles and pipeline as three columns', () => {
    const w = editor({ presetId: defaultTeam.id, topologyOverride: false, roles: resolved })
    const headings = w.findAll('h2').map((heading) => heading.text())
    expect(headings).toEqual(['Teams', 'Roles', 'Pipeline'])
    expect(w.text()).toContain('Default')
    expect(w.find('[aria-label="Default is in use"]').exists()).toBe(true)
    expect(w.text()).toContain('terminal')
  })

  it('offers no adopt button for the team already in use, and one for every other', () => {
    const w = editor({ presetId: defaultTeam.id, topologyOverride: false, roles: resolved })
    const labels = w.findAll('button').map((b) => b.text())
    // Exactly one, for Docs team. The team in use says so with the star and
    // the "in use" line instead of a button that cannot be pressed.
    expect(labels.filter((t) => t === 'Use this team')).toHaveLength(1)
    expect(labels).not.toContain('In use')
  })

  it('offers the team in use a way back when the project overrode its pipeline', () => {
    const w = editor({ presetId: defaultTeam.id, topologyOverride: true, roles: resolved })
    expect(w.text()).toContain('in use, with local changes')
    expect(w.findAll('button').map((b) => b.text())).toContain('Follow this pipeline again')
  })

  it('keeps rename and delete on every row, and off the built-in', () => {
    const w = editor({ presetId: defaultTeam.id, topologyOverride: false, roles: resolved })
    // Always rendered rather than revealed on hover: there is no hover on a
    // phone, and this column is the only place a team can be removed.
    expect(w.get('[aria-label="Delete Docs team"]').attributes('disabled')).toBeUndefined()
    expect(w.get('[aria-label="Delete Default"]').attributes('disabled')).toBeDefined()
    expect(w.get('[aria-label="Rename Default"]').attributes('disabled')).toBeDefined()
    expect(w.find('[aria-label="Clone Default"]').exists()).toBe(true)
  })

  it('asks before moving a project to another team while agents are running', async () => {
    const w = mount(TeamEditor, {
      props: {
        library: [coder, reviewer],
        presets: [defaultTeam, docsTeam],
        projectTeam: { presetId: defaultTeam.id, topologyOverride: false, roles: resolved },
        harnesses: ['claude'],
        models: {},
        running: true,
      },
      global: { stubs: { RoleOverrideDialog: true } },
    })
    await w.findAll('button').find((b) => b.text() === 'Use this team')!.trigger('click')
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
    const w = editor({ presetId: defaultTeam.id, topologyOverride: false, roles: resolved })
    await w.findAll('button').find((button) => button.text().includes('Docs team'))!.trigger('click')

    expect(w.emitted('setTeam')).toBeUndefined()
    await w.findAll('button').find((button) => button.text() === 'Use this team')!.trigger('click')
    expect(w.emitted('setTeam')?.at(-1)?.[0]).toEqual({
      presetId: docsTeam.id,
      topologyOverride: false,
      roles: [],
    })
  })

  it('reorders the selected team in the pipeline', async () => {
    const w = editor({ presetId: defaultTeam.id, topologyOverride: false, roles: resolved })
    await w.get('[aria-label="Move coder down"]').trigger('click')
    const updated = w.emitted('savePreset')?.at(-1)?.[0] as TeamPreset
    expect(updated.roles.map((role) => role.templateId)).toEqual(['reviewer', 'coder'])
  })
})

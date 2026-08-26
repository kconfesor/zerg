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
    expect(w.text()).toContain('terminal')
  })

  it('selects a team for editing without using it until the explicit button is pressed', async () => {
    const w = editor({ presetId: defaultTeam.id, topologyOverride: false, roles: resolved })
    await w.findAll('button').find((button) => button.text().includes('Docs team'))!.trigger('click')

    expect(w.emitted('setTeam')).toBeUndefined()
    await w.findAll('button').find((button) => button.text() === 'Use this Team')!.trigger('click')
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

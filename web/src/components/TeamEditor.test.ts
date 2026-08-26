import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import TeamEditor from './TeamEditor.vue'
import type { ProjectTeam, ResolvedRole, RoleTemplate, TeamPreset } from '@/lib/api'

const coder: RoleTemplate = {
  id: 'coder',
  name: 'coder',
  harness: 'claude',
  model: 'sonnet',
  args: ['--library'],
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

const preset: TeamPreset = {
  id: 'default',
  name: 'Default',
  builtin: true,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
  roles: [
    {
      templateId: 'coder',
      position: 0,
      enabled: true,
      argsOverride: null,
      modelOverride: 'preset-model',
    },
    { templateId: 'reviewer', position: 1, enabled: true, argsOverride: null },
  ],
}

const resolved: ResolvedRole[] = [
  {
    ...coder,
    model: 'project-model',
    position: 0,
    enabled: true,
    overridden: true,
    terminal: false,
    modelOverride: 'project-model',
    argsOverride: [],
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
      presets: [preset],
      projectTeam: team,
      harnesses: ['claude'],
      models: {},
      running: false,
    },
    global: {
      stubs: {
        RoleOverrideDialog: true,
      },
    },
  })
}

describe('TeamEditor project topology', () => {
  it('materializes the effective pipeline without losing raw field overrides', async () => {
    const w = editor({ presetId: preset.id, topologyOverride: false, roles: resolved })
    await w.findAll('button').find((b) => b.text() === 'Customize pipeline')!.trigger('click')

    expect(w.emitted('setTeam')?.at(-1)?.[0]).toEqual({
      presetId: preset.id,
      topologyOverride: true,
      roles: [
        expect.objectContaining({
          templateId: 'coder',
          enabled: true,
          modelOverride: 'project-model',
          argsOverride: [],
        }),
        expect.objectContaining({
          templateId: 'reviewer',
          enabled: true,
          modelOverride: null,
          argsOverride: null,
        }),
      ],
    })
  })
})

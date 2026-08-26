import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import AppSidebar from './AppSidebar.vue'
import type { Project, ResolvedRole, RoleStatus, SwarmStatus } from '@/lib/api'

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

function sidebar(status: SwarmStatus, team: ResolvedRole[], teamName = 'Calc pipeline') {
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
      open: false,
    },
  })
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
})

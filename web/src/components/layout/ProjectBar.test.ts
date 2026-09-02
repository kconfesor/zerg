import { afterEach, describe, expect, it } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import ProjectBar from './ProjectBar.vue'
import type { Project, SwarmStatus } from '@/lib/api'

enableAutoUnmount(afterEach)

const project: Project = {
  id: '01M0VPTPS4TY4MC5MJPH0S4MJ2',
  path: '/src/calc2',
  name: 'CalcRust',
  baseBranch: 'main',
  integration: 'merge',
  prDraft: false,
  createdAt: '2026-01-01T00:00:00Z',
}

function bar(status: Partial<SwarmStatus>) {
  return mount(ProjectBar, {
    props: {
      current: project,
      status: { running: false, roles: [], ...status } as SwarmStatus,
      attentionCount: 0,
    },
    // The spend summary fetches on mount, and nothing here is about spend. Left
    // real it aborts that request at teardown and writes a stack trace per
    // test into output somebody has to read past to find a genuine failure.
    global: { stubs: { UsageSummary: true } },
  })
}

/** The controls, by what a screen reader would call them. */
function labels(w: ReturnType<typeof bar>): string[] {
  return w.findAll('button').map((b) => b.attributes('aria-label') ?? '')
}

// A project that is down and wants to stay down offers one control: start it.
describe('the run control', () => {
  it('offers only Start for a project nobody has asked to run', () => {
    const w = bar({ running: false, wanted: false })
    expect(labels(w)).toContain('Start agents')
    expect(labels(w).filter((l) => l.startsWith('Stop'))).toEqual([])
  })

  // The state a failed resume leaves: the operator asked for this project, the
  // daemon could not bring it back, and every later daemon start will try
  // again. Stop is what withdraws that, and it used to be rendered only while
  // something was running — hidden behind the state it exists to escape.
  it('offers Stop for a project that is down and still wanted', async () => {
    const w = bar({ running: false, wanted: true })
    const stop = w.findAll('button').find((b) => b.attributes('aria-label')?.startsWith('Stop'))
    expect(stop).toBeDefined()
    await stop!.trigger('click')
    expect(w.emitted('stop')).toHaveLength(1)
  })

  // And a running project still has exactly one, which is the Stop that takes
  // the agents down.
  it('offers only Stop for a running project', () => {
    const w = bar({ running: true, wanted: true })
    expect(labels(w)).toContain('Stop agents')
    expect(labels(w)).not.toContain('Start agents')
  })
})

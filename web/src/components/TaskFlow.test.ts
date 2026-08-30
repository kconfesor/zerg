import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import TaskFlow from './TaskFlow.vue'
import { api, type ActivityEvent, type TaskStep } from '@/lib/api'

vi.mock('@/lib/api', () => ({ api: { taskEvents: vi.fn() } }))
enableAutoUnmount(afterEach)

const taskEvents = vi.mocked(api.taskEvents)

function step(over: Partial<TaskStep> = {}): TaskStep {
  return {
    from: 'coder',
    to: 'reviewer',
    kind: 'handoff',
    body: 'did it',
    at: '2026-01-01T09:10:00.000Z',
    startedAt: '2026-01-01T09:00:00.000Z',
    windowStart: '2026-01-01T09:00:00.000Z',
    windowEnd: '2026-01-01T09:30:00.000Z',
    durationMs: 600_000,
    tokens: 300,
    costUsd: 0.25,
    ...over,
  }
}

function event(kind: string, over: Partial<ActivityEvent> = {}): ActivityEvent {
  return {
    id: kind + Math.random(),
    projectId: 'p',
    role: 'coder',
    kind: kind as ActivityEvent['kind'],
    at: '2026-01-01T09:05:00.000Z',
    ...over,
  }
}

function flow(steps: TaskStep[] = [step()]) {
  return mount(TaskFlow, { props: { taskId: 'task-1', steps, roles: ['coder', 'reviewer'] } })
}

beforeEach(() => {
  taskEvents.mockReset()
  taskEvents.mockResolvedValue({ events: [], truncated: false })
})

describe('a step of the trail', () => {
  it('shows what it cost and how long the role held it', () => {
    const w = flow()
    expect(w.text()).toContain('10m')
    expect(w.text()).toContain('$0.25')
  })

  it('reads its own window rather than the whole card', async () => {
    const w = flow()
    await w.findAll('button').find((b) => b.text().includes('what it did'))!.trigger('click')
    await flushPromises()

    const [id, opts] = taskEvents.mock.calls.at(-1)!
    expect(id).toBe('task-1')
    expect(opts).toMatchObject({ role: 'coder', from: '2026-01-01T09:00:00.000Z' })
    // The window is the daemon's own, the one the step's cost was summed over,
    // rather than the handoff plus a guess: the two disagreeing is how a step
    // gets charged for a turn that is missing from what it did.
    expect(opts).toMatchObject({ until: '2026-01-01T09:30:00.000Z' })
  })

  it('lists what the role did and counts the machinery', async () => {
    taskEvents.mockResolvedValue({
      events: [
        event('tool_call', { tool: 'Bash' }),
        event('message', { text: 'following the existing convention' }),
        event('tool_done', { text: 'ok' }),
        event('thinking', { text: '' }),
        event('usage'),
        event('turn_end'),
      ],
      truncated: false,
    })
    const w = flow()
    await w.findAll('button').find((b) => b.text().includes('what it did'))!.trigger('click')
    await flushPromises()

    expect(w.text()).toContain('Bash')
    expect(w.text()).toContain('following the existing convention')
    // "ok" from every tool result and a blank from every thinking event is a
    // wall of nothing between the lines that say what happened.
    expect(w.text()).not.toContain('ok')
    expect(w.text()).toContain('and 4 more of the machinery')
  })

  it('says a transcript is gone rather than showing an empty box', async () => {
    taskEvents.mockResolvedValue({ events: [], truncated: false })
    const w = flow()
    await w.findAll('button').find((b) => b.text().includes('what it did'))!.trigger('click')
    await flushPromises()
    expect(w.text()).toContain('No transcript kept')
  })

  it('offers nothing to open on a step with no window to read', () => {
    // The operator's own first message has no lease behind it and nothing
    // before it, so there is no window and no transcript to ask for.
    const w = flow([
      step({ from: 'operator', startedAt: undefined, windowStart: undefined, windowEnd: undefined, durationMs: 0 }),
    ])
    expect(w.findAll('button').some((b) => b.text().includes('what it did'))).toBe(false)
  })
})

describe('a card that skipped a role', () => {
  it('draws the column and says it was skipped', async () => {
    const w = mount(TaskFlow, {
      props: {
        taskId: 'task-1',
        steps: [step({ from: 'planner', to: 'reviewer' })],
        roles: ['planner', 'coder', 'reviewer'],
        skipped: ['coder'],
      },
    })
    await flushPromises()

    // The column is there. Left out, the diagram reads as a card that lost a
    // role somewhere, which is the thing this is meant to explain.
    expect(w.text()).toContain('coder')
    expect(w.text()).toContain('skipped')

    // And only that one. The other lifelines are ordinary columns.
    expect(w.findAll('span').filter((s) => s.text() === 'skipped')).toHaveLength(1)
  })
})

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
  taskEvents.mockResolvedValue([])
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
    // The window runs past the handoff on purpose: the turn that produced it
    // finishes after the message is written, and stopping at the handoff drops
    // the largest turn of the step.
    expect(new Date(opts!.until!).getTime()).toBeGreaterThan(new Date('2026-01-01T09:10:00.000Z').getTime())
  })

  it('lists what the role did and counts the machinery', async () => {
    taskEvents.mockResolvedValue([
      event('tool_call', { tool: 'Bash' }),
      event('message', { text: 'following the existing convention' }),
      event('tool_done', { text: 'ok' }),
      event('thinking', { text: '' }),
      event('usage'),
      event('turn_end'),
    ])
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
    taskEvents.mockResolvedValue([])
    const w = flow()
    await w.findAll('button').find((b) => b.text().includes('what it did'))!.trigger('click')
    await flushPromises()
    expect(w.text()).toContain('No transcript kept')
  })

  it('offers nothing to open on a step with no window to read', () => {
    // The operator's own first message has no lease behind it and nothing
    // before it, so there is no window and no transcript to ask for.
    const w = flow([step({ from: 'operator', startedAt: undefined, durationMs: 0 })])
    expect(w.findAll('button').some((b) => b.text().includes('what it did'))).toBe(false)
  })
})

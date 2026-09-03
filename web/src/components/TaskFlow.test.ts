import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import TaskFlow from './TaskFlow.vue'
import { api, type ActivityEvent, type TaskStep } from '@/lib/api'

vi.mock('@/lib/api', () => ({ api: { taskEvents: vi.fn(), approvalDiff: vi.fn() } }))
enableAutoUnmount(afterEach)

const taskEvents = vi.mocked(api.taskEvents)
const approvalDiff = vi.mocked(api.approvalDiff)

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

  it('keeps the column for a role the team no longer has', async () => {
    // A skipped role leaves no step behind — that is what being skipped is —
    // so nothing else can reconstruct it. Removed from the team, it used to
    // disappear, and the card then read as though it went through the whole
    // pipeline.
    const w = mount(TaskFlow, {
      props: {
        taskId: 'task-1',
        steps: [step({ from: 'planner', to: 'reviewer' })],
        roles: ['planner', 'reviewer'],
        skipped: ['coder'],
      },
    })
    await flushPromises()

    expect(w.text()).toContain('coder')
    expect(w.text()).toContain('skipped')
  })
})

// What the architect decided, and why.
//
// The trail carried the note, the evidence commit and the questions the sidecar
// answered, and drew "approved by supervisor". A card said a judgement had been
// made and gave a reader no way to find out what it was.
describe('a decision taken at a gate', () => {
  const gated = (over: Partial<NonNullable<TaskStep['gate']>> = {}) =>
    step({
      gate: {
        id: 'a1',
        state: 'approved',
        note: 'Approved with one correction. The **formatter** direction is fixed rather than papered over.',
        createdAt: '2026-01-01T09:10:00.000Z',
        decidedAt: '2026-01-01T09:12:00.000Z',
        waitedMs: 120_000,
        decidedBy: 'supervisor',
        evidenceSha: 'cafebabe0123456789abcdef0123456789abcdef',
        ...over,
      },
    })

  it('opens onto the rationale, the decider and the commit that holds it', async () => {
    const w = flow([gated()])
    await w.findAll('button').find((b) => b.text() === 'the decision')!.trigger('click')

    const panel = w.text()
    expect(panel).toContain('approved by supervisor')
    expect(panel).toContain('The formatter direction is fixed rather than papered over')
    expect(panel).toContain('cafebabe0123456789abcdef0123456789abcdef')
    // Markdown, not literal asterisks: an architect writes its reasoning the
    // way it writes everything else.
    expect(w.html()).toContain('<strong>formatter</strong>')
  })

  // Decisions taken before refs were resolved stored the literal string the
  // agent sent. Drawn as a commit, `HEAD` is a link to the reasoning that
  // resolves to something different in every tree that reads it, which is the
  // failure resolving the ref fixed. Those rows cannot be repaired.
  it('does not present a literal HEAD as the commit it is not', async () => {
    const w = flow([gated({ evidenceSha: 'HEAD' })])
    expect(w.text()).not.toContain('HEAD')

    await w.findAll('button').find((b) => b.text() === 'the decision')!.trigger('click')
    expect(w.text()).not.toContain('HEAD')
    expect(w.text()).not.toContain('git show')
    // The rest of the decision still reads.
    expect(w.text()).toContain('Approved with one correction')
  })

  it('carries the questions it settled on the way', async () => {
    const w = flow([
      step({
        clarifications: [
          {
            id: 'c1',
            role: 'planner',
            question: 'Sundays: close at 17:00 or 18:00?',
            answer: '17:00, Sunday closes an hour early',
            answeredBy: 'supervisor',
            state: 'answered',
            createdAt: '2026-01-01T09:05:00.000Z',
          },
        ],
      }),
    ])
    await w.findAll('button').find((b) => b.text() === 'the decision')!.trigger('click')

    expect(w.text()).toContain('Sundays: close at 17:00 or 18:00?')
    expect(w.text()).toContain('17:00, Sunday closes an hour early')
    expect(w.text()).toContain('answered by supervisor')
  })

  // A step with a gate and nothing written into it has nothing behind the
  // disclosure, and an empty panel reads as something having gone wrong.
  it('offers nothing to open when there is nothing recorded', () => {
    const w = flow([
      step({
        gate: {
          id: 'a1',
          state: 'pending',
          createdAt: '2026-01-01T09:10:00.000Z',
          waitedMs: 1_000,
        },
      }),
    ])
    expect(w.findAll('button').some((b) => b.text() === 'the decision')).toBe(false)
  })
})

// The rationale says what was concluded; this is what it was concluded about.
//
// The endpoint served a resolved approval all along, and Attention has always
// used it for pending ones, so the change a decision was taken over was
// reachable the whole time and nothing offered it. Reading a decision without
// it is taking the decider's word for what it decided about.
describe('the change a decision was taken over', () => {
  const gated = () =>
    step({
      gate: {
        id: 'a1',
        state: 'approved',
        note: 'Approved.',
        createdAt: '2026-01-01T09:10:00.000Z',
        decidedAt: '2026-01-01T09:12:00.000Z',
        waitedMs: 1_000,
        decidedBy: 'supervisor',
      },
    })

  const openDecision = async (w: ReturnType<typeof mount>) => {
    await w.findAll('button').find((b) => b.text() === 'the decision')!.trigger('click')
  }

  it('fetches it once, when asked, and shows the diff', async () => {
    approvalDiff.mockResolvedValue({
      files: [
        {
          path: 'docs/specs/sunday-hours.md',
          status: 'M',
          added: 41,
          removed: 96,
          diff: '@@ -1 +1 @@\n-Thursday to Saturday\n+Thursday to Sunday\n',
        },
      ],
      range: true,
      base: 'main',
      seen: [],
    })
    const w = flow([gated()])
    await openDecision(w)
    expect(approvalDiff).not.toHaveBeenCalled()

    await w.findAll('button').find((b) => b.text() === 'what it reviewed')!.trigger('click')
    await flushPromises()

    expect(approvalDiff).toHaveBeenCalledTimes(1)
    expect(approvalDiff).toHaveBeenCalledWith('a1')
    expect(w.text()).toContain('docs/specs/sunday-hours.md')
    expect(w.text()).toContain('+41')
    expect(w.text()).toContain('Thursday to Sunday')

    // Closing and reopening reads what is already here.
    await w.findAll('button').find((b) => b.text() === 'hide what it reviewed')!.trigger('click')
    await w.findAll('button').find((b) => b.text() === 'what it reviewed')!.trigger('click')
    await flushPromises()
    expect(approvalDiff).toHaveBeenCalledTimes(1)
  })

  // An empty box reads like a bug. A decision over nothing is a real answer.
  it('says so when no change is recorded against the decision', async () => {
    approvalDiff.mockResolvedValue({ files: [], range: false, base: 'main', seen: [] })
    const w = flow([gated()])
    await openDecision(w)
    await w.findAll('button').find((b) => b.text() === 'what it reviewed')!.trigger('click')
    await flushPromises()
    expect(w.text()).toContain('No change is recorded against this decision')
  })
})

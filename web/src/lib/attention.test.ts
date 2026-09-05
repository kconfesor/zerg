import { describe, expect, it } from 'vitest'
import { summarizeAttention } from './attention'
import type {
  Attention,
  Approval,
  Clarification,
  FeatureStall,
  PlanRevision,
  Task,
} from './api'

const approval = (id: string) => ({ id }) as Approval
const question = (id: string) => ({ id }) as Clarification
const looping = (id: string) => ({ id }) as Task

const queue = (over: Partial<Attention> = {}): Attention => ({
  approvals: [],
  clarifications: [],
  supervisor: { wanted: false, live: false },
  rework: { threshold: 3, tasks: [] },
  ...over,
})

describe('what the queue is holding', () => {
  it('counts each kind and names it', () => {
    expect(
      summarizeAttention(
        queue({
          approvals: [approval('a1'), approval('a2')],
          clarifications: [question('c1')],
          plans: [{ id: 'p1' } as PlanRevision],
        }),
      ),
    ).toBe('2 approvals · 1 plan · 1 question')
  })

  // A stalled feature is the one thing here nobody is working on: no agent
  // will touch it again until a person does something.
  it('counts a stalled feature', () => {
    expect(
      summarizeAttention(
        queue({ stalls: [{ featureId: 'f1', name: 'Billing', reason: 'failed' } as FeatureStall] }),
      ),
    ).toBe('1 stalled feature')
  })

  it('is singular for one of something', () => {
    expect(summarizeAttention(queue({ approvals: [approval('a1')] }))).toBe('1 approval')
  })

  it('leaves out the kinds that are not there', () => {
    expect(
      summarizeAttention(queue({ rework: { threshold: 3, tasks: [looping('t1'), looping('t2')] } })),
    ).toBe('2 looping cards')
  })

  // The panel already says "Nothing needs you", and a count of nothing is
  // worse than silence.
  it('says nothing when the queue is empty', () => {
    expect(summarizeAttention(queue())).toBe('')
    expect(summarizeAttention(null)).toBe('')
  })
})

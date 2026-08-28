import { describe, expect, it } from 'vitest'
import { placeInPipeline } from '@/lib/team'
import type { RoleTemplate } from '@/lib/api'

function role(id: string, finisher = false): RoleTemplate {
  return {
    id,
    name: id,
    harness: 'claude',
    model: 'sonnet',
    args: [],
    thinking: '',
    receive: 'task',
    batchMaxItems: 8,
    batchMaxAgeSec: 300,
    prompt: '',
    gate: 'none',
    finisher,
    builtin: true,
  }
}

const coder = role('coder')
const docs = role('docs')
const reviewer = role('reviewer', true)
const cleaner = role('cleaner', true)
const library = [coder, docs, reviewer, cleaner]

describe('where a role joins a pipeline', () => {
  it('puts an ordinary role in front of the one that ends the pipeline', () => {
    // Appending is what handed the job of integrating to whatever was added
    // last, taking it from the role that had been doing it.
    const pipeline = [coder, reviewer]
    expect(placeInPipeline(pipeline, docs, library)).toBe(1)
  })

  it('puts a role that ends pipelines at the end', () => {
    expect(placeInPipeline([coder, docs], reviewer, library)).toBe(2)
  })

  it('goes in front of every finisher already there, not just the last one', () => {
    // Two of them can end up adjacent: a team running a reviewer and then a
    // cleaner. A docs role belongs before both.
    expect(placeInPipeline([coder, reviewer, cleaner], docs, library)).toBe(1)
  })

  it('appends when nothing in the pipeline ends it', () => {
    expect(placeInPipeline([coder, docs], role('planner'), library)).toBe(2)
  })

  it('appends to an empty pipeline whatever the role is', () => {
    expect(placeInPipeline([], docs, library)).toBe(0)
    expect(placeInPipeline([], reviewer, library)).toBe(0)
  })
})

import { describe, expect, it } from 'vitest'
import { cloneOverrides, followPreset, hasRoleOverrides, presetRoles } from '@/lib/team'
import { moveWithin, placeInPipeline } from '@/lib/team'
import type { ResolvedRole, RoleTemplate, TeamPreset } from '@/lib/api'

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

describe('what survives a write', () => {
  const resolved = (over: Partial<ResolvedRole>): ResolvedRole => ({
    ...coder,
    position: 0,
    enabled: true,
    overridden: false,
    terminal: false,
    argsOverride: null,
    ...over,
  })

  it('carries a thinking override through a copy', () => {
    // Every one of these rebuilds the whole team, so a field the helpers do not
    // know about is not preserved, it is deleted: reordering, parking, cloning
    // or adopting a team erased the level a role was set to think at.
    expect(cloneOverrides({ thinkingOverride: 'xhigh' }).thinkingOverride).toBe('xhigh')
    expect(hasRoleOverrides({ thinkingOverride: 'xhigh', argsOverride: null })).toBe(true)
  })

  it('keeps a thinking-only project override when a team is adopted', () => {
    const team: TeamPreset = {
      id: 'team',
      name: 'Team',
      builtin: false,
      projectId: null,
      roles: [{ templateId: 'coder', position: 0, enabled: true, argsOverride: null }],
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-01T00:00:00Z',
    }
    const update = followPreset(team, [resolved({ thinkingOverride: 'max' })])
    expect(update.roles).toHaveLength(1)
    expect(update.roles[0].thinkingOverride).toBe('max')
  })

  it("carries the team's own thinking setting into a copy of that team", () => {
    const source: TeamPreset = {
      id: 'source',
      name: 'Source',
      builtin: false,
      projectId: null,
      roles: [
        { templateId: 'coder', position: 0, enabled: true, argsOverride: null, thinkingOverride: 'high' },
      ],
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-01T00:00:00Z',
    }
    const rows = presetRoles([resolved({})], source)
    expect(rows[0].thinkingOverride).toBe('high')
  })
})

describe('moving a role within a pipeline', () => {
  const list = ['a', 'b', 'c', 'd']

  it('drops between the two rows the pointer is between', () => {
    // "to" counts gaps in the list as it stands, so dropping item 0 into the
    // gap before c is index 2, and it lands second.
    expect(moveWithin(list, 0, 2)).toEqual(['b', 'a', 'c', 'd'])
    expect(moveWithin(list, 3, 1)).toEqual(['a', 'd', 'b', 'c'])
  })

  it('moves to either end', () => {
    expect(moveWithin(list, 2, 0)).toEqual(['c', 'a', 'b', 'd'])
    expect(moveWithin(list, 0, 4)).toEqual(['b', 'c', 'd', 'a'])
  })

  it('leaves the list alone when nothing moved', () => {
    expect(moveWithin(list, 1, 1)).toEqual(list)
    // The gap after itself is where it already is.
    expect(moveWithin(list, 1, 2)).toEqual(list)
  })
})

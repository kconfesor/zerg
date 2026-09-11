import { describe, expect, it } from 'vitest'
import { laneHarness, taskHarness } from '@/lib/utils'

const team = [
  { enabled: true, name: 'planner', harness: 'claude' },
  { enabled: true, name: 'coder', harness: 'pi' },
  { enabled: false, name: 'retired', harness: 'pi' },
]

describe('laneHarness', () => {
  it('is the enabled role sitting in that lane', () => {
    expect(laneHarness(team, 'coder')).toBe('pi')
  })

  it('ignores a disabled role of the same name', () => {
    expect(laneHarness(team, 'retired')).toBe('')
  })

  it('is empty for a lane with no role, like the done well', () => {
    expect(laneHarness(team, 'done')).toBe('')
  })
})

describe('taskHarness', () => {
  it('is the lane it is queued in, not any harness recorded yet', () => {
    expect(taskHarness(team, { state: 'queued', lane: 'coder', harnesses: ['claude'] })).toBe('pi')
  })

  it("is the lane working it now, not the first harness that ever touched it", () => {
    // A card that started under claude's planner and handed off to a pi coder
    // must not go on pulsing claude's mark while pi is the one spending
    // tokens right now.
    expect(
      taskHarness(team, { state: 'working', lane: 'coder', harnesses: ['claude'] }),
    ).toBe('pi')
  })

  it('falls back to the recorded harness if the lane has no role', () => {
    expect(taskHarness(team, { state: 'working', lane: 'ghost', harnesses: ['claude'] })).toBe(
      'claude',
    )
  })

  it('is the last harness recorded, once the card has left every lane', () => {
    expect(
      taskHarness(team, { state: 'done', lane: 'done', harnesses: ['claude', 'pi'] }),
    ).toBe('pi')
  })

  it('falls back to the lane only when nothing was ever recorded', () => {
    expect(taskHarness(team, { state: 'done', lane: 'done', harnesses: [] })).toBe('')
  })
})

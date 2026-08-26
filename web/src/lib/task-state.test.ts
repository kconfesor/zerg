import { describe, expect, it } from 'vitest'
import { taskState } from '@/lib/utils'

describe('taskState', () => {
  it("calls a person's stop a stop, not a rejection", () => {
    // Both are stored as `rejected`; the timestamp is the only difference, and
    // the board used to report a parked card as one a reviewer turned down.
    expect(taskState({ state: 'rejected', stoppedAt: '2026-08-26T15:41:21Z' })).toBe('stopped')
  })

  it("leaves a role's verdict alone", () => {
    expect(taskState({ state: 'rejected' })).toBe('rejected')
  })

  it('passes every other state through untouched', () => {
    for (const s of ['queued', 'working', 'done']) expect(taskState({ state: s })).toBe(s)
  })
})

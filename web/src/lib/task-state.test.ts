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

import { landing } from '@/lib/utils'

describe('landing', () => {
  const base = { baseBranch: 'main' }

  it('merges only when the project actually merges', () => {
    expect(landing({ ...base, integration: 'merge' })).toEqual({
      head: 'main',
      line: 'merges to main',
    })
  })

  it('says pull request, and says draft when it is one', () => {
    // The rail claimed "merges to main" whatever the project was set to, which
    // is a false statement about someone's repository two thirds of the time.
    expect(landing({ ...base, integration: 'pr' }).head).toBe('pull request')
    expect(landing({ ...base, integration: 'pr', prDraft: true }).head).toBe('draft PR')
    expect(landing({ ...base, integration: 'pr', prDraft: true }).line).toContain('draft')
  })

  it('lands nothing when the project lands nothing', () => {
    const l = landing({ ...base, integration: 'branch' })
    expect(l.head).toBe('its branch')
    expect(l.line).not.toContain('main')
  })
})

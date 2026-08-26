import { describe, expect, it } from 'vitest'
import { latest } from './latest'

describe('latest', () => {
  it('lets only the newest caller commit', () => {
    const newest = latest()
    const first = newest()
    const second = newest()
    expect(first()).toBe(false)
    expect(second()).toBe(true)
  })

  it('survives A → B → A, which comparing ids does not', () => {
    // The case the previous guard missed: two requests for the same project
    // are outstanding, the id matches for both, and the older one lands last.
    const newest = latest()
    const a1 = newest()
    const b = newest()
    const a2 = newest()
    expect(a1()).toBe(false)
    expect(b()).toBe(false)
    expect(a2()).toBe(true)
  })

  it('a single call is current', () => {
    const newest = latest()
    expect(newest()()).toBe(true)
  })
})

import { describe, expect, it } from 'vitest'
import { usePending } from './pending'

describe('usePending', () => {
  it('reports an operation as busy while it runs', async () => {
    const busy = usePending()
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))

    const running = busy.run('start', () => gate)
    expect(busy.is('start')).toBe(true)
    expect(busy.is('stop')).toBe(false)

    release()
    await running
    expect(busy.is('start')).toBe(false)
  })

  it('ignores a second press while the first is in flight', async () => {
    const busy = usePending()
    let calls = 0
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))

    const first = busy.run('start', async () => {
      calls++
      await gate
      return 'done'
    })
    const second = await busy.run('start', async () => {
      calls++
      return 'done'
    })

    expect(second).toBeUndefined()
    release()
    expect(await first).toBe('done')
    expect(calls).toBe(1)
  })

  it('clears the key when the operation throws', async () => {
    const busy = usePending()
    await expect(
      busy.run('start', async () => {
        throw new Error('nope')
      }),
    ).rejects.toThrow('nope')
    expect(busy.is('start')).toBe(false)
  })
})

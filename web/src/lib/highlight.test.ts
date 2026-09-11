import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// highlight.ts keeps one Worker across calls and reuses it until a request's
// deadline kills it. This stand-in is what lets a test prove the module
// tears down the worker a given timer actually sent its request to, not
// just whatever the shared `worker` variable happens to point at when that
// timer fires -- a real Worker never appears in this environment.
class FakeWorker {
  static instances: FakeWorker[] = []
  onmessage: ((e: MessageEvent) => void) | null = null
  terminated = false
  constructor() {
    FakeWorker.instances.push(this)
  }
  postMessage() {}
  terminate() {
    this.terminated = true
  }
}

describe('highlight', () => {
  beforeEach(() => {
    FakeWorker.instances = []
    vi.resetModules()
    vi.useFakeTimers()
    vi.stubGlobal('Worker', FakeWorker)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it("a timed-out request's cleanup only tears down the worker it actually used", async () => {
    const { highlight } = await import('./highlight')
    const code = 'x'.repeat(30)

    const a = highlight('a.ts', code) // spawns worker 1
    vi.advanceTimersByTime(10)
    const b = highlight('b.ts', code) // reuses worker 1; its deadline is 10ms behind a's
    expect(FakeWorker.instances).toHaveLength(1)

    vi.advanceTimersByTime(790) // a's deadline: times out, terminates worker 1
    expect(await a).toEqual({ timedOut: true })
    expect(FakeWorker.instances[0].terminated).toBe(true)

    const c = highlight('c.ts', code) // worker 1 is gone -- this spawns worker 2
    expect(FakeWorker.instances).toHaveLength(2)

    vi.advanceTimersByTime(10) // b's deadline, now that a's has already fired
    expect(await b).toEqual({ timedOut: true })

    // b's timer belonged to worker 1, already dead. Worker 2 -- serving c, a
    // request that started after b did and has nothing to do with it -- must
    // still be alive.
    expect(FakeWorker.instances[1].terminated).toBe(false)

    FakeWorker.instances[1].onmessage?.({ data: { id: 2, html: '<span>c</span>' } } as MessageEvent)
    expect(await c).toEqual({ html: '<span>c</span>' })
  })
})

import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from './api'

// The page starts the first three calls before this bundle is parsed, which on
// a phone over a tailnet is most of a round trip. The rule that keeps it an
// optimisation rather than a second code path: taken once, and any failure
// falls back to a real request so errors still arrive with their status.
describe('the answers the page fetched before the bundle loaded', () => {
  afterEach(() => {
    delete window.__zergBoot
    vi.unstubAllGlobals()
  })

  it('uses the boot answer instead of asking again', async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    window.__zergBoot = { projects: Promise.resolve([{ id: 'p1', name: 'Calc' }]) }

    const got = await api.projects()

    expect(got).toEqual([{ id: 'p1', name: 'Calc' }])
    expect(fetchMock).not.toHaveBeenCalled()
  })

  // A cache would be wrong here: the board refreshes this list to see new
  // projects, and serving it the answer from page load would freeze it.
  it('asks the daemon on every call after the first', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: async () => JSON.stringify([{ id: 'p2' }]),
    })
    vi.stubGlobal('fetch', fetchMock)
    window.__zergBoot = { projects: Promise.resolve([{ id: 'p1' }]) }

    await api.projects()
    const second = await api.projects()

    expect(second).toEqual([{ id: 'p2' }])
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  // The boot script has no error handling of its own on purpose: a daemon that
  // answered 500 to the early call must produce the same ApiError as any other
  // call, not a bare Error with a status code for a message.
  it('falls back to a real request when the early one failed', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 503,
      statusText: 'Service Unavailable',
      text: async () => JSON.stringify({ error: 'the daemon is starting' }),
    })
    vi.stubGlobal('fetch', fetchMock)
    window.__zergBoot = { projects: Promise.reject(new Error('503')) }

    await expect(api.projects()).rejects.toBeInstanceOf(ApiError)
    await expect(api.projects()).rejects.toMatchObject({
      status: 503,
      message: 'the daemon is starting',
    })
    expect(fetchMock).toHaveBeenCalled()
  })

  it('works with no boot script at all', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: async () => JSON.stringify(['claude']),
    })
    vi.stubGlobal('fetch', fetchMock)

    expect(await api.harnesses()).toEqual(['claude'])
  })
})

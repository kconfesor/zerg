import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import History from './History.vue'
import { api, type HistoryEntry } from '@/lib/api'

vi.mock('@/lib/api', () => ({ api: { history: vi.fn() } }))
enableAutoUnmount(afterEach)

function entry(name: string, over: Partial<HistoryEntry> = {}): HistoryEntry {
  return {
    id: name,
    projectId: 'p',
    name,
    body: '',
    lane: 'done',
    state: 'done',
    createdAt: '2026-01-01T09:00:00Z',
    completedAt: '2026-01-01T15:00:00Z',
    activeMs: 12 * 60_000,
    reworkCount: 0,
    tokens: 100,
    costUsd: 0.42,
    roles: ['coder', 'reviewer'],
    ...over,
  }
}

const history = vi.mocked(api.history)

function screen() {
  return mount(History, { props: { projectId: 'p', roles: ['coder', 'reviewer'] } })
}

beforeEach(() => {
  history.mockReset()
  history.mockResolvedValue({ entries: [entry('Factorial')], next: '' })
})

describe('History', () => {
  it('shows what a task cost, what it ended as, and where the time went', async () => {
    history.mockResolvedValue({
      entries: [entry('Factorial', { outcome: 'merged', reworkCount: 2 })],
      next: '',
    })
    const w = screen()
    await flushPromises()

    const text = w.text()
    expect(text).toContain('Factorial')
    expect(text).toContain('merged')
    // Six hours waited against twelve minutes worked is the reading this
    // screen exists for; either number alone says nothing.
    expect(text).toContain('6h 00m wall')
    expect(text).toContain('12m working')
    expect(text).toContain('$0.42')
    expect(text).toContain('coder → reviewer')
    expect(text).toContain('2') // laps
  })

  it('asks for a fresh list when the search changes, not another page', async () => {
    // A narrowed list starts at the top. Carrying the cursor over would ask for
    // "the page after that one" in a list that no longer has it, and the answer
    // is a page of nothing under a filter that matches plenty.
    vi.useFakeTimers()
    const w = screen()
    await flushPromises()

    await w.get('input[aria-label="Search history"]').setValue('fact')
    // Typed searches wait for a pause rather than firing per keystroke.
    expect(history.mock.calls).toHaveLength(1)
    vi.advanceTimersByTime(300)
    await flushPromises()
    vi.useRealTimers()

    expect(history.mock.calls.at(-1)![1]).toMatchObject({ q: 'fact', before: '' })
  })

  it('appends the next page rather than replacing what is on screen', async () => {
    history.mockResolvedValueOnce({ entries: [entry('Newest')], next: 'cursor-1' })
    const w = screen()
    await flushPromises()

    history.mockResolvedValueOnce({ entries: [entry('Older')], next: '' })
    await w.findAll('button').find((b) => b.text() === 'Older')!.trigger('click')
    await flushPromises()

    expect(history.mock.calls.at(-1)![1]).toMatchObject({ before: 'cursor-1' })
    expect(w.text()).toContain('Newest')
    expect(w.text()).toContain('Older')
    // And the cursor is spent: nothing offers a page that does not exist.
    expect(w.findAll('button').some((b) => b.text() === 'Older')).toBe(false)
  })

  it('says the difference between an empty project and an empty search', async () => {
    history.mockResolvedValue({ entries: [], next: '' })
    const w = screen()
    await flushPromises()
    expect(w.text()).toContain('Nothing has been worked on')
  })
})

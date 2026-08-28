import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import Attention from './Attention.vue'
import {
  api,
  type Attention as AttentionData,
  type ChangedFile,
  type ReviewThread,
} from '@/lib/api'

vi.mock('@/lib/api', () => ({
  api: {
    approvalDiff: vi.fn(),
    approvalFile: vi.fn(),
    approvalMergeable: vi.fn(),
    markFileSeen: vi.fn(),
    review: vi.fn(),
    resolveReviewThread: vi.fn(),
  },
}))
enableAutoUnmount(afterEach)

const diff = vi.mocked(api.approvalDiff)
const one = vi.mocked(api.approvalFile)
const mergeable = vi.mocked(api.approvalMergeable)
const seen = vi.mocked(api.markFileSeen)
const review = vi.mocked(api.review)

function file(path: string, over: Partial<ChangedFile> = {}): ChangedFile {
  return { path, status: 'M', added: 3, removed: 1, ...over }
}

const attention = (files: ChangedFile[], threads: ReviewThread[] = []): AttentionData => {
  diff.mockResolvedValue({ files, range: true, base: 'main', seen: [] })
  mergeable.mockResolvedValue({ clean: true, conflicts: [], baseAhead: 0 })
  review.mockResolvedValue(threads)
  seen.mockResolvedValue({ seen: [] })
  return {
    approvals: [
      {
        id: 'a1',
        messageId: 'm1',
        state: 'pending',
        taskId: 't1',
        taskName: 'sweep',
        commit: 'abc1234',
        terminal: true,
        createdAt: '2026-01-01T00:00:00Z',
      },
    ],
    clarifications: [],
    rework: { threshold: 3, tasks: [] },
  }
}

async function open(
  files: ChangedFile[],
  opts: Record<string, unknown> = {},
  threads: ReviewThread[] = [],
) {
  const w = mount(Attention, { props: { attention: attention(files, threads) }, ...opts })
  await flushPromises()
  // The panel opens the diff itself at a terminal gate; only click if it did not.
  const toggle = w.findAll('button').find((b) => /^Show\b/.test(b.text()))
  if (toggle) {
    await toggle.trigger('click')
    await flushPromises()
  }
  return w
}

const row = (w: ReturnType<typeof mount>, path: string) =>
  w.find(`[data-file="${path}"]`).text().replace(/\s+/g, ' ')

describe('a change too large to send in one piece', () => {
  // The header said "+0 −0" for every file whose diff was withheld, because it
  // counted plus signs in text that was never sent. The size is the whole
  // reason to open a file or leave it.
  it('states the size of a file it did not read', async () => {
    const w = await open([
      file('huge.rs', { added: 9000, removed: 0, tooLarge: true }),
      file('src40.rs', { added: 3, removed: 0, deferred: true }),
    ])
    expect(row(w, 'huge.rs')).toContain('+9000 −0')
    expect(row(w, 'src40.rs')).toContain('+3 −0')
  })

  // Opening by default would either fetch the whole change back, which is what
  // the limit exists to stop, or show "Loading…" under a request nobody made.
  it('leaves a deferred file closed until it is opened, then fetches it once', async () => {
    one.mockResolvedValue(file('src40.rs', { diff: '@@ -0,0 +1 @@\n+fn f40() {}\n' }))
    const w = await open([file('src40.rs', { added: 3, removed: 0, deferred: true })])

    expect(row(w, 'src40.rs')).not.toContain('Loading')
    expect(one).not.toHaveBeenCalled()

    await w.find('[data-file="src40.rs"] button').trigger('click')
    await flushPromises()

    expect(one).toHaveBeenCalledTimes(1)
    expect(one).toHaveBeenCalledWith('a1', 'src40.rs')
    expect(row(w, 'src40.rs')).toContain('fn f40()')
  })

  // j is the motion for reading through a change. Landing on the thirty-first
  // file and being told to reach for the mouse is not reading through it.
  it('fetches a deferred file the reader moves onto with the keyboard', async () => {
    one.mockResolvedValue(file('b.rs', { diff: '@@ -0,0 +1 @@\n+fn b() {}\n' }))
    const w = await open([file('a.rs', { diff: '@@ -1 +1 @@\n-x\n+y\n' }), file('b.rs', { deferred: true })])

    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'j' }))
    await flushPromises()

    expect(one).toHaveBeenCalledWith('a1', 'b.rs')
    expect(row(w, 'b.rs')).toContain('fn b()')
  })

  // The bar is pinned to the top of the panel for the whole read, so what it
  // names has to be the file under it rather than the file the reader started
  // on. The scroll tracking itself needs layout and is checked in a browser;
  // this covers the part that does not: pressing next renames the bar.
  it('names the file being read, and renames it on next', async () => {
    const w = await open([file('a.rs', { diff: '@@ -1 +1 @@\n-x\n+y\n' }), file('b.rs', { diff: '@@ -1 +1 @@\n-p\n+q\n' })])
    const bar = () => w.find('.sticky').text().replace(/\s+/g, ' ')

    expect(bar()).toContain('file 1 of 2')
    expect(bar()).toContain('a.rs')

    await w.findAll('.sticky button').find((b) => b.text() === 'next')!.trigger('click')
    await flushPromises()

    expect(bar()).toContain('file 2 of 2')
    expect(bar()).toContain('b.rs')
  })

  // "comment on this file" opened a box whose buttons were Remark and Ask: one
  // word covering two different acts, and the one most often done there was
  // putting a question to an agent. The reader says which now.
  it('opens an ask, with the cursor already in it', async () => {
    const w = await open([file('a.rs', { diff: '@@ -1 +1 @@\n-x\n+y\n' })], { attachTo: document.body })

    await w.findAll('button').find((b) => b.text() === 'Ask about this file')!.trigger('click')
    await flushPromises()

    const box = w.find('[data-composer]')
    expect((box.element as HTMLInputElement).placeholder).toContain('know about this code')
    // autofocus is honoured on a page load, not on an input mounted later, so
    // the box was opened with the cursor nowhere.
    expect(document.activeElement).toBe(box.element)
    const actions = w.findAll('[data-slot=input-group] button').map((b) => b.text())
    expect(actions).toContain('Ask')
    expect(actions).toContain('Remark instead')
  })

  // A prompt is a question, so pressing one is asking, whichever way the box
  // was opened.
  it('turns a remark into an ask when a prompt is used', async () => {
    const w = await open([file('a.rs', { diff: '@@ -1 +1 @@\n-x\n+y\n' })])

    await w.findAll('button').find((b) => b.text() === 'Remark on this file')!.trigger('click')
    await flushPromises()
    expect(w.findAll('[data-slot=input-group] button').map((b) => b.text())).toContain('Remark')

    await w.findAll('button').find((b) => b.text() === 'Why is it done this way?')!.trigger('click')
    await flushPromises()

    expect((w.find('[data-composer]').element as HTMLInputElement).value).toBe('Why is it done this way?')
    expect(w.findAll('[data-slot=input-group] button').map((b) => b.text())).toContain('Ask')
  })

  // Two files loading at once, which is what walking j through a change does.
  // Written back from a snapshot taken before the request, whichever response
  // landed last undid the other.
  it('keeps both files when two are fetched at once', async () => {
    const gate: Record<string, (f: ChangedFile) => void> = {}
    one.mockImplementation(
      (_id: string, path: string) =>
        new Promise<ChangedFile>((resolve) => {
          gate[path] = resolve
        }),
    )
    const w = await open([
      file('a.rs', { deferred: true }),
      file('b.rs', { deferred: true }),
    ])

    await w.find('[data-file="a.rs"] button').trigger('click')
    await w.find('[data-file="b.rs"] button').trigger('click')

    // Out of order, which is the case a snapshot cannot survive.
    gate['b.rs']!(file('b.rs', { diff: '@@ -0,0 +1 @@\n+fn b() {}\n' }))
    await flushPromises()
    gate['a.rs']!(file('a.rs', { diff: '@@ -0,0 +1 @@\n+fn a() {}\n' }))
    await flushPromises()

    expect(row(w, 'a.rs')).toContain('fn a()')
    expect(row(w, 'b.rs')).toContain('fn b()')
  })

  // A thread outlives the diff it was written on: reject a card and the next
  // revision can rename or delete the file a remark points at. Rendered only
  // under the files of the current listing, that remark was on screen nowhere
  // while it still held the merge.
  it('shows a remark whose file is no longer in the change', async () => {
    const w = await open([file('a.rs', { diff: '@@ -1 +1 @@\n-x\n+y\n' })], {}, [
      {
        id: 'th1',
        projectId: 'p',
        taskId: 't1',
        file: 'gone.rs',
        line: 12,
        kind: 'remark',
        state: 'open',
        createdAt: '2026-01-01T00:00:00Z',
        comments: [
          {
            id: 'c1',
            threadId: 'th1',
            author: 'operator',
            body: 'this caller was left behind',
            createdAt: '2026-01-01T00:00:00Z',
          },
        ],
      },
    ])

    const text = w.text().replace(/\s+/g, ' ')
    expect(text).toContain('From an earlier revision')
    expect(text).toContain('gone.rs:12')
    expect(text).toContain('this caller was left behind')
    // And it can be settled from there, which is the whole point.
    expect(w.findAll('button').some((b) => b.text() === 'settle')).toBe(true)
  })

  // A binary file and a file that did not change look identical when both
  // render nothing.
  it('says why a binary file shows nothing', async () => {
    const w = await open([file('logo.png', { status: 'A', added: 0, removed: 0, binary: true })])
    expect(row(w, 'logo.png')).toContain('Binary file')
  })
})

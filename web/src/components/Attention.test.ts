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
    approvalGuide: vi.fn(),
    requestGuide: vi.fn(),
    taskArtifacts: vi.fn(),
    pinArtifact: vi.fn(),
  },
  artifactBytes: (id: string) => `/api/artifacts/${id}/bytes`,
}))
enableAutoUnmount(afterEach)

const diff = vi.mocked(api.approvalDiff)
const one = vi.mocked(api.approvalFile)
const mergeable = vi.mocked(api.approvalMergeable)
const seen = vi.mocked(api.markFileSeen)
const review = vi.mocked(api.review)
const guide = vi.mocked(api.approvalGuide)
const askGuide = vi.mocked(api.requestGuide)

function file(path: string, over: Partial<ChangedFile> = {}): ChangedFile {
  return { path, status: 'M', added: 3, removed: 1, ...over }
}

const attention = (files: ChangedFile[], threads: ReviewThread[] = []): AttentionData => {
  diff.mockResolvedValue({ files, range: true, base: 'main', seen: [] })
  mergeable.mockResolvedValue({ clean: true, conflicts: [], baseAhead: 0 })
  review.mockResolvedValue(threads)
  vi.mocked(api.taskArtifacts).mockResolvedValue([])
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
    supervisor: { wanted: true, live: true },
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

  // The first ten minutes of any review is working out what the change is
  // trying to do. The guide is that part, cached; without one, the panel
  // offers to have the agent write it, and says who decides.
  it('offers a guide, and renders the one that exists', async () => {
    const w = await open([file('a.rs', { diff: '@@ -1 +1 @@\n-x\n+y\n' })])
    const offer = w.findAll('button').find((b) => b.text().includes('Map this change'))
    expect(offer).toBeDefined()
    expect(w.text()).toContain('it describes, you decide')

    guide.mockResolvedValue({
      approvalId: 'a1',
      commitSha: 'abc1234',
      body: '**What this is for.** Postfix factorial.',
      createdAt: '2026-01-01T00:00:00Z',
    })
    askGuide.mockResolvedValue({ status: 'reading' })
    await offer!.trigger('click')
    expect(askGuide).toHaveBeenCalledWith('a1')
  })

  it('shows a stored guide without being asked', async () => {
    guide.mockResolvedValue({
      approvalId: 'a1',
      commitSha: 'abc1234',
      body: 'Start with parse.rs, the rest follows it.',
      createdAt: '2026-01-01T00:00:00Z',
    })
    const w = await open([file('a.rs', { diff: '@@ -1 +1 @@\n-x\n+y\n' })])
    expect(w.text()).toContain('Start with parse.rs')
    expect(w.text()).toContain('reading guide')
    // And a way to have it rewritten after a rework.
    expect(w.findAll('button').some((b) => b.text() === 'rewrite it')).toBe(true)
  })

  // A binary file and a file that did not change look identical when both
  // render nothing.
  it('says why a binary file shows nothing', async () => {
    const w = await open([file('logo.png', { status: 'A', added: 0, removed: 0, binary: true })])
    expect(row(w, 'logo.png')).toContain('Binary file')
  })
})

// A gated planner commits a spec, so the approval renders a diff of one
// markdown file surrounded by the tools for interrogating code. None of them
// is the question in front of you: the document already says what it is for,
// "this file" is the only file, and the way to push back on a plan is to
// reject it with a reason.
describe('approving a plan rather than a change', () => {
  it('does not offer the tools for interrogating code', async () => {
    const w = await open([
      file('docs/specs/a-plan.md', { diff: '@@ -0,0 +1 @@\n+the plan\n' }),
    ])
    const labels = w.findAll('button').map((b) => b.text())
    expect(labels).not.toContain('Ask about this file')
    expect(labels).not.toContain('Remark on this file')
    expect(labels.some((l) => l.includes('Map this change'))).toBe(false)
    // The plan itself is still there to read.
    expect(w.text()).toContain('docs/specs/a-plan.md')
  })

  it('still offers them as soon as the change touches code', async () => {
    const w = await open([
      file('docs/specs/a-plan.md', { diff: '@@ -0,0 +1 @@\n+the plan\n' }),
      file('src/main.ts', { diff: '@@ -0,0 +1 @@\n+const x = 1\n' }),
    ])
    const labels = w.findAll('button').map((b) => b.text())
    expect(labels).toContain('Ask about this file')
  })
})

// Merge readiness belongs to the gate that merges.
//
// Every gate was told "will not fast-forward: this is behind main" and "main
// has moved 6 commits", including a planner handing a spec to the coder, where
// approving merges nothing and the next role rebases as a matter of course. A
// warning that shows where it cannot apply is one people learn to scroll past.
describe('merge readiness', () => {
  it('is neither shown nor asked for at a handoff', async () => {
    const data = attention([file('src/main.ts', { diff: '@@ -0,0 +1 @@\n+x\n' })])
    data.approvals[0].terminal = false
    mergeable.mockClear()

    const w = mount(Attention, { props: { attention: data } })
    await flushPromises()

    expect(mergeable).not.toHaveBeenCalled()
    expect(w.text()).not.toContain('will not fast-forward')
    expect(w.text()).not.toContain('has moved')
    expect(w.text()).not.toContain('merges cleanly')
  })

  it('is shown at the gate that lands the work', async () => {
    const data = attention([file('src/main.ts', { diff: '@@ -0,0 +1 @@\n+x\n' })])
    mergeable.mockClear()
    // Set after attention(), which stubs a clean merge of its own.
    mergeable.mockResolvedValue({ clean: false, diverged: true, conflicts: [], baseAhead: 6 })

    const w = mount(Attention, { props: { attention: data } })
    await flushPromises()

    expect(mergeable).toHaveBeenCalled()
    expect(w.text()).toContain('will not fast-forward')
    expect(w.text()).toContain('has moved 6')
  })
})

// A question with options the agent worked out itself.
//
// The operator used to read the choices as prose and retype one into a
// one-line box, which is where an answer stops matching what was offered: an
// agent looking for one of three names gets a paraphrase, or a typo.
describe('a question that is a choice', () => {
  const asking = (options?: string[]): AttentionData => ({
    approvals: [],
    clarifications: [
      {
        id: 'c1',
        taskId: 't1',
        role: 'coder',
        question: 'Where does the session live?',
        options,
        state: 'open',
        createdAt: '2026-01-01T00:00:00Z',
      },
    ],
    supervisor: { wanted: true, live: true },
    rework: { threshold: 3, tasks: [] },
  })

  const both = ['Redis, shared across instances', 'A signed cookie, no server state']

  const expand = async (w: ReturnType<typeof mount>) => {
    const trigger = w.findAll('button').find((b) => b.text().startsWith('Answer'))!
    await trigger.trigger('click')
    await flushPromises()
    return trigger
  }

  it('says what is being asked before anything is clicked, and how many answers there are', () => {
    const w = mount(Attention, { props: { attention: asking(both) } })
    expect(w.text()).toContain('Where does the session live?')
    expect(w.text()).toContain('2 options')
    // The form is behind the click; the question is not.
    expect(w.text()).not.toContain('Redis, shared across instances')
  })

  it('sends the option that was picked, verbatim', async () => {
    const w = mount(Attention, { props: { attention: asking(both) } })
    await expand(w)

    expect(w.text()).toContain('A signed cookie, no server state')
    const radios = w.findAll('[data-slot="radio-group-item"]')
    // Two options and Something else.
    expect(radios).toHaveLength(3)
    await radios[1].trigger('click')

    const answer = w.findAll('button').find((b) => b.text() === 'Answer')!
    await answer.trigger('click')
    expect(w.emitted('answer')).toEqual([['c1', 'A signed cookie, no server state']])
  })

  it('takes an answer the agent did not think of', async () => {
    const w = mount(Attention, { props: { attention: asking(both) } })
    await expand(w)

    // Nothing picked yet is nothing to send.
    expect(w.findAll('button').find((b) => b.text() === 'Answer')!.attributes('disabled')).toBeDefined()

    const radios = w.findAll('[data-slot="radio-group-item"]')
    await radios[2].trigger('click')
    const box = w.find('textarea')
    await box.setValue('Postgres, we already run one')

    await w.findAll('button').find((b) => b.text() === 'Answer')!.trigger('click')
    expect(w.emitted('answer')).toEqual([['c1', 'Postgres, we already run one']])
  })

  // Three radios with no accessible name are read out as three unrelated
  // options, and the question they belong to is on screen right above them.
  it('names the choices after the question being asked', async () => {
    const w = mount(Attention, { props: { attention: asking(both) } })
    await expand(w)

    const group = w.find('[role="radiogroup"]')
    expect(group.exists()).toBe(true)
    const labelledBy = group.attributes('aria-labelledby')
    expect(labelledBy).toBeTruthy()
    expect(w.find(`#${labelledBy}`).text()).toContain('Where does the session live?')

    // The box under Something else answers the same question.
    const radios = w.findAll('[data-slot="radio-group-item"]')
    await radios[2].trigger('click')
    expect(w.find('textarea').attributes('aria-labelledby')).toBe(labelledBy)
  })

  it('is still a box to write in when the agent offered nothing', async () => {
    const w = mount(Attention, { props: { attention: asking(undefined) } })
    // No disclosure to open: there is nothing behind it.
    expect(w.text()).not.toContain('option')
    const box = w.find('textarea')
    expect(box.exists()).toBe(true)

    await box.setValue('between 0 and 100')
    await w.findAll('button').find((b) => b.text() === 'Answer')!.trigger('click')
    expect(w.emitted('answer')).toEqual([['c1', 'between 0 and 100']])
  })
})

// The badge says what is happening, not what was asked for.
//
// `supervised` on a card is a request for an architect sidecar. Drawn from
// that alone, the badge read "architect is deciding" while the daemon was
// logging, to nobody, that there is no supervisor role in the library or that
// its harness would not start. The operator saw a card that needed no action
// and it never got one. Both causes are things a person fixes, so both have to
// reach that person.
describe('a card that asked for an architect', () => {
  const supervised = (supervisor: AttentionData['supervisor']): AttentionData => ({
    approvals: [],
    clarifications: [
      {
        id: 'c1',
        taskId: 't1',
        role: 'coder',
        question: 'Where does the session live?',
        state: 'open',
        createdAt: '2026-01-01T00:00:00Z',
        supervised: true,
      },
    ],
    supervisor,
    rework: { threshold: 3, tasks: [] },
  })

  it('says an architect is deciding only while one is running', () => {
    const w = mount(Attention, {
      props: { attention: supervised({ wanted: true, live: true, role: 'supervisor' }) },
    })
    expect(w.text()).toContain('architect is deciding')
    expect(w.text()).not.toContain('waiting for an architect')
  })

  it('names what an operator has to fix when no architect started', () => {
    const w = mount(Attention, {
      props: {
        attention: supervised({
          wanted: true,
          live: false,
          error: 'no role in the library has the supervisor purpose',
        }),
      },
    })
    expect(w.text()).not.toContain('architect is deciding')
    expect(w.text()).toContain('waiting for an architect')
    expect(w.text()).toContain('no role in the library has the supervisor purpose')
  })
})

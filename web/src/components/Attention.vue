<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useMediaQuery } from '@vueuse/core'
import type { Attention } from '@/lib/api'
import {
  AlertTriangle,
  BookOpen,
  Check,
  ChevronRight,
  GitMerge,
  HelpCircle,
  MessageSquare,
  X,
} from '@lucide/vue'
import { api, type ChangedFile, type Mergeable, type ReviewThread } from '@/lib/api'
import { renderMarkdown } from '@/lib/markdown'
import DiffView from '@/components/DiffView.vue'
import ReviewThreadView from '@/components/ReviewThread.vue'
import Artifacts from '@/components/Artifacts.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from '@/components/ui/input-group'

/**
 * Whether this is being read on a phone.
 *
 * Not a style detail: at 390px the composer's box measured 90 pixels wide with
 * Remark, Ask and Cancel beside it, which is not a field anyone can write a
 * remark in. Below this width the buttons go under the box instead.
 */
const narrow = useMediaQuery('(max-width: 639px)')

const props = defineProps<{
  attention: Attention | null
  /** The project these approvals belong to, so the gate can offer to run the
   *  commit it is deciding about. */
  projectId?: string | null
  compact?: boolean
  /**
   * Whether an operation is in flight, asked by key. A decision runs a merge or
   * opens a pull request, so a second press is not a wasted request — it races
   * the first through git.
   */
  busy?: (key: string) => boolean
}>()

/** Local shorthands, so the template does not repeat the key format. */
function deciding(id: string): boolean {
  return props.busy?.(`decide:${id}`) ?? false
}
function answering(id: string): boolean {
  return props.busy?.(`answer:${id}`) ?? false
}
const emit = defineEmits<{
  approve: [id: string]
  reject: [id: string, note: string]
  answer: [id: string, answer: string]
}>()

const notesOpen = ref<Record<string, boolean>>({})

/** Which files are expanded, keyed approvalId::path. */
const fileOpen = ref<Record<string, boolean>>({})

const key = (id: string, path: string) => `${id}::${path}`

/**
 * Whether a file starts open, which depends on what the gate is asking.
 *
 * At a hand-off the document is the work, so it opens. At the gate that
 * performs the merge you approved that document already, and the question has
 * become what the code does — so the spec starts collapsed and the code starts
 * open. A reader who wants it back is one click away; a reader who wants the
 * diff should not scroll through a spec to reach it.
 */
function defaultOpen(a: { id: string; terminal?: boolean }, f: ChangedFile): boolean {
  const k = key(a.id, f.path)
  if (k in fileOpen.value) return fileOpen.value[k]
  // A deferred file has no content yet and does not fetch itself: opening by
  // default would either read the whole change back (the thing the limit
  // exists to stop) or show "Loading…" under every file past the thirtieth,
  // for a request nobody made. It opens when the reader opens it.
  if (f.deferred) return false
  return a.terminal ? !isDoc(f) : true
}

function toggleFile(a: { id: string; terminal?: boolean }, f: ChangedFile) {
  const k = key(a.id, f.path)
  fileOpen.value = { ...fileOpen.value, [k]: !defaultOpen(a, f) }
}

/**
 * Added and removed counts, so a collapsed file still states its size.
 *
 * From git's own numstat rather than by counting plus signs in the diff, which
 * is what this did and got wrong the moment a diff was not sent: a 9000-line
 * file left unread for being over the size cap, and every file past the eager
 * limit, sat under a header reading "+0 −0". The number is the whole reason to
 * open a file or leave it, so it cannot come from the text that was withheld.
 */
function stat(f: ChangedFile): string {
  return `+${f.added ?? 0} −${f.removed ?? 0}`
}
const diffs = ref<
  Record<
    string,
    {
      open: boolean
      files: ChangedFile[]
      range?: boolean
      base?: string
      error?: string
      /** Files already read at this gate, as the daemon remembers them. */
      seen?: string[]
    }
  >
>({})

/** Markdown is shown as the document it is. Everything else is shown as a
 *  diff, because for a change to existing code the change *is* the point —
 *  while for a file the commit created, the diff is the document with a plus in
 *  front of every line. */
function isDoc(f: ChangedFile): boolean {
  return /\.(md|markdown|txt)$/i.test(f.path) && !!f.content
}

/**
 * Load what an approval is about as soon as it is shown.
 *
 * It was behind a toggle, which put the thing being decided one click further
 * away than the buttons that decide it. The document is the point of the card;
 * the card should open with it.
 */
/**
 * File by file, with somewhere to stand.
 *
 * A twelve-file diff is one long scroll and no sense of progress, and an
 * approval is read in the gaps: on a phone, and finished later at a desk.
 * Which files have been read is the daemon's to remember, so the second sitting
 * does not start at the top again.
 *
 * The mark is the reader's own. A file that scrolled past the viewport is not a
 * file that was read, and a review that decides for you what you have seen is
 * worse than one that keeps asking.
 */
const current = ref<Record<string, string>>({})

function seenFiles(id: string): string[] {
  return diffs.value[id]?.seen ?? []
}

const seenSets = computed(() => {
  const out: Record<string, Set<string>> = {}
  for (const [id, d] of Object.entries(diffs.value)) out[id] = new Set(d.seen ?? [])
  return out
})

function isSeen(id: string, path: string): boolean {
  return seenSets.value[id]?.has(path) ?? false
}

function unread(id: string): number {
  return (diffs.value[id]?.files ?? []).filter((f) => !isSeen(id, f.path)).length
}

/** Which file is being read: the one chosen, or the first not yet read. */
function reading(id: string): string {
  const files = diffs.value[id]?.files ?? []
  if (!files.length) return ''
  const chosen = current.value[id]
  if (chosen && files.some((f) => f.path === chosen)) return chosen
  return (files.find((f) => !isSeen(id, f.path)) ?? files[0]).path
}

function positionOf(id: string): { at: number; of: number } {
  const files = diffs.value[id]?.files ?? []
  return { at: files.findIndex((f) => f.path === reading(id)) + 1, of: files.length }
}

/**
 * Read a file the listing left alone.
 *
 * A large change lists every file and reads the first thirty, because a
 * hundred-file diff otherwise loads every file before anyone has opened one.
 * The rest arrive when they are looked at.
 */
/** Files being fetched now, so an open one says which of the two it is. */
const loading = ref(new Set<string>())

async function loadOne(id: string, path: string) {
  if (!diffs.value[id]) return
  loading.value = new Set(loading.value).add(path)
  try {
    const file = await api.approvalFile(id, path)
    // Merged into the state as it is now, not as it was before the request.
    // Walking j through a change starts one of these per file, and a read mark
    // lands in the same object: written back from a snapshot taken before the
    // await, whichever response came last undid the others.
    const now = diffs.value[id]
    if (!now) return
    diffs.value = {
      ...diffs.value,
      [id]: { ...now, files: now.files.map((f) => (f.path === path ? file : f)) },
    }
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  } finally {
    const rest = new Set(loading.value)
    rest.delete(path)
    loading.value = rest
  }
}

async function setSeen(id: string, path: string, seen: boolean) {
  const before = diffs.value[id]
  if (!before) return
  // Optimistic: the mark is the reader's own record and should not wait on a
  // round trip to appear.
  diffs.value = {
    ...diffs.value,
    [id]: {
      ...before,
      seen: seen ? [...seenFiles(id), path] : seenFiles(id).filter((f) => f !== path),
    },
  }
  try {
    const r = await api.markFileSeen(id, path, seen)
    diffs.value = { ...diffs.value, [id]: { ...diffs.value[id]!, seen: r.seen } }
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

/**
 * Move on, marking what was being read as read.
 *
 * Forward marks; backward does not. Going back to look at something again is
 * not a statement that you finished with it.
 */
function step(id: string, by: 1 | -1) {
  const files = diffs.value[id]?.files ?? []
  if (!files.length) return
  const here = files.findIndex((f) => f.path === reading(id))
  if (by === 1 && files[here]) void setSeen(id, files[here].path, true)
  const next = Math.min(files.length - 1, Math.max(0, here + by))
  arriveAt(id, files[next])
}

/**
 * Land on a file: make it current, open it, and fetch it if it was deferred.
 *
 * Moving to a file is the reader saying they want to read it, so the keyboard
 * path has to load one the listing left alone. Without this, j through a
 * forty-seven file change stopped at the thirty-first on "Not read yet" with
 * no hint that the way forward was the mouse.
 */
function arriveAt(id: string, f: ChangedFile) {
  current.value = { ...current.value, [id]: f.path }
  if (f.deferred && !loading.value.has(f.path)) {
    fileOpen.value = { ...fileOpen.value, [key(id, f.path)]: true }
    void loadOne(id, f.path)
  }
  focusFile(f.path)
}

/** The next file that still wants reading, rather than the next in the list. */
function nextUnread(id: string) {
  const files = diffs.value[id]?.files ?? []
  const here = files.findIndex((f) => f.path === reading(id))
  if (files[here]) void setSeen(id, files[here].path, true)
  const rest = [...files.slice(here + 1), ...files.slice(0, here)]
  const next = rest.find((f) => !isSeen(id, f.path))
  if (!next) return
  arriveAt(id, next)
}

/**
 * j and k move between files.
 *
 * Guarded on where the focus is: this panel is full of boxes for remarks and
 * questions, and a shortcut that fires while somebody is typing "just checking"
 * would jump the page twice and eat the letters.
 */
function onKey(e: KeyboardEvent) {
  if (e.metaKey || e.ctrlKey || e.altKey) return
  const el = e.target as HTMLElement | null
  if (el && (el.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName))) return
  const approvals = props.attention?.approvals ?? []
  // One approval is the ordinary case; with several, the shortcut drives the
  // first, and the buttons are there for the rest.
  const id = approvals.find((a) => a.commit && diffs.value[a.id]?.files.length)?.id
  if (!id) return
  if (e.key === 'j') {
    e.preventDefault()
    step(id, 1)
  } else if (e.key === 'k') {
    e.preventDefault()
    step(id, -1)
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKey)
  // The dialog builds its body around this panel, so the scroller exists by
  // the time the first frame is painted, not when this runs.
  requestAnimationFrame(() => {
    scroller = scrollerFor(root.value)
    scroller?.addEventListener('scroll', onScroll, { passive: true })
  })
})
onUnmounted(() => {
  window.removeEventListener('keydown', onKey)
  scroller?.removeEventListener('scroll', onScroll)
})

/** Scroll the file being read to the top of the panel, since the panel is what
 *  scrolls rather than the page. scroll-mt on the file leaves room for the bar,
 *  which is sticky and would otherwise land on top of the header. */
function focusFile(path: string) {
  requestAnimationFrame(() => {
    document.querySelector(`[data-file="${CSS.escape(path)}"]`)?.scrollIntoView({ block: 'start' })
  })
}

/** The panel's root, so the scroll tracking can find what scrolls it. */
const root = ref<HTMLElement | null>(null)

/**
 * Where the bar sits, and the line it reads the current file from.
 *
 * The bar follows the reader down the change, so what it says has to follow
 * the reader too: pinned to the top and still reporting "file 3 of 47" twelve
 * files later, next would jump backwards and the count would be a lie. The
 * file whose header has passed under the bar is the file being read.
 */
const BAR = 48

/** The scrolling ancestor: the dialog's body, or whatever holds this panel. */
function scrollerFor(el: HTMLElement | null): HTMLElement | null {
  for (let p = el?.parentElement; p; p = p.parentElement) {
    const y = getComputedStyle(p).overflowY
    if (y === 'auto' || y === 'scroll') return p
  }
  return null
}

let scroller: HTMLElement | null = null
let queued = false

function trackReading() {
  if (!root.value || !scroller) return
  const line = scroller.getBoundingClientRect().top + BAR
  for (const group of root.value.querySelectorAll<HTMLElement>('[data-approval]')) {
    const id = group.dataset.approval
    if (!id) continue
    let under: string | null = null
    for (const f of group.querySelectorAll<HTMLElement>('[data-file]')) {
      if (f.getBoundingClientRect().top > line) break
      under = f.dataset.file ?? null
    }
    if (under && current.value[id] !== under) {
      current.value = { ...current.value, [id]: under }
    }
  }
}

// One read per frame. Scroll fires far faster than the layout can be measured,
// and measuring in the handler is what makes a scroll janky.
function onScroll() {
  if (queued) return
  queued = true
  requestAnimationFrame(() => {
    queued = false
    trackReading()
  })
}

/**
 * The conversation about the code, anchored to it.
 *
 * Rejecting used to be one note for a whole diff, and the exchange ended there.
 * A remark now points at a file and a line, the answer lands on the thread that
 * asked, and the card does not merge while one is open, which the daemon
 * enforces rather than this panel.
 */
const threads = ref<Record<string, ReviewThread[]>>({})
/**
 * What the reader opened the box to do.
 *
 * The two acts are not the same thing and were behind one word: "comment on
 * this file" opened a box whose buttons were Remark and Ask, so the label
 * promised a note and the thing most often done there was putting a question
 * to an agent. The reader now says which, and the box answers to that: Enter
 * does it, and the other stays beside it for a mind changed mid-sentence.
 */
type Intent = 'ask' | 'remark'

const composing = ref<{
  task: string
  file: string
  line: number
  hunk: string
  intent: Intent
} | null>(null)
/** Threads waiting on an agent, so the box says so instead of looking empty. */
const awaiting = ref<Set<string>>(new Set())
const draft = ref('')
const replies = ref<Record<string, string>>({})
const reviewError = ref('')

async function loadThreads(taskId: string) {
  try {
    threads.value = { ...threads.value, [taskId]: await api.review(taskId) }
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

/**
 * Threads by the file they point at, built once per change to them.
 *
 * Not filtered per call. These are read from the template, which runs them on
 * every render of the card: three lookups and a line list for each of
 * forty-nine files, each one scanning every thread. Worse than the scanning,
 * a filter returns a *new array* each time, and a new array is a new prop --
 * so every DiffView on the card re-rendered whenever anything on it changed,
 * to move one border. Built as a map, the arrays keep their identity and Vue
 * skips the subtrees whose input did not move.
 */
const byFile = computed(() => {
  const out: Record<string, { threads: ReviewThread[]; lines: number[] }> = {}
  for (const [taskId, list] of Object.entries(threads.value)) {
    for (const t of list) {
      const at = (out[`${taskId}::${t.file ?? ''}`] ??= { threads: [], lines: [] })
      at.threads.push(t)
      // The lines the gutter marks: a remark still open, or any question.
      if (t.line > 0 && (t.state === 'open' || t.kind === 'question')) at.lines.push(t.line)
    }
  }
  return out
})

/** Shared empties, so "nothing here" is also the same value every time. */
const NO_THREADS: ReviewThread[] = []
const NO_LINES: number[] = []

function threadsFor(taskId: string | undefined, file: string): ReviewThread[] {
  return byFile.value[`${taskId ?? ''}::${file}`]?.threads ?? NO_THREADS
}

/**
 * Threads pointing at nothing in this revision.
 *
 * A thread outlives the diff it was written on: reject a card and the next
 * revision can rename or delete the file a remark was left on. Rendered only
 * under the files of the current listing, that remark appeared nowhere while
 * still holding the merge -- the daemon refused to approve, named a count, and
 * the panel offered nothing to settle.
 */
function orphanThreads(taskId: string | undefined, approvalID: string): ReviewThread[] {
  const here = new Set((diffs.value[approvalID]?.files ?? []).map((f) => f.path))
  return (threads.value[taskId ?? ''] ?? []).filter((t) => !here.has(t.file ?? ''))
}

/** Lines already carrying a thread, so the gutter can show where the
 *  conversation is without opening anything. */
function discussed(taskId: string | undefined, file: string): number[] {
  return byFile.value[`${taskId ?? ''}::${file}`]?.lines ?? NO_LINES
}

/** What holds the gate: remarks the reader has not settled. A question never
 *  counts, however many are open. */
function openCount(taskId: string | undefined): number {
  return (threads.value[taskId ?? ''] ?? []).filter(
    (t) => t.state === 'open' && t.kind !== 'question',
  ).length
}

function compose(
  taskId: string | undefined,
  file: string,
  line: number,
  hunk = '',
  intent: Intent = 'remark',
) {
  if (!taskId) return
  composing.value = { task: taskId, file, line, hunk, intent }
  draft.value = ''
  void nextTick(() => document.querySelector<HTMLInputElement>('[data-composer]')?.focus())
}

/**
 * The questions a reader actually asks at a hunk, one press away.
 *
 * Prefilled rather than canned: they go in the box, so the reader can change
 * one before asking. The agent answers them; it does not review the change and
 * does not decide anything, which is why asking opens a thread that holds
 * nothing at the gate.
 */
const PROMPTS = [
  'Why is it done this way?',
  'What breaks if this is wrong?',
  'What else did this change touch?',
]

/** Put a prompt in the box and make the box an ask, since a prompt is one. */
function askThis(question: string) {
  draft.value = question
  if (composing.value) composing.value = { ...composing.value, intent: 'ask' }
}

async function ask(approval: { id: string; commit?: string }, taskId: string | undefined, question: string) {
  const at = composing.value
  if (!taskId || !question.trim()) return
  reviewError.value = ''
  try {
    const thread = await api.askAboutChange(taskId, {
      approvalId: approval.id,
      commitSha: approval.commit,
      base: diffs.value[approval.id]?.base,
      file: at?.file,
      line: at?.line,
      hunk: at?.hunk,
      question,
    })
    composing.value = null
    draft.value = ''
    awaiting.value = new Set(awaiting.value).add(thread.id)
    await loadThreads(taskId)
    void waitForAnswer(taskId, thread.id, thread.comments.length)
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

/** A follow-up on the same thread, which is how a conversation continues. */
async function askAgain(approval: { id: string; commit?: string }, taskId: string | undefined, thread: ReviewThread) {
  const text = replies.value[thread.id]
  if (!taskId || !text?.trim()) return
  reviewError.value = ''
  try {
    await api.askAboutChange(taskId, {
      threadId: thread.id,
      approvalId: approval.id,
      commitSha: approval.commit,
      base: diffs.value[approval.id]?.base,
      file: thread.file,
      line: thread.line,
      question: text,
    })
    replies.value = { ...replies.value, [thread.id]: '' }
    awaiting.value = new Set(awaiting.value).add(thread.id)
    await loadThreads(taskId)
    void waitForAnswer(taskId, thread.id, (thread.comments.length ?? 0) + 1)
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

/**
 * The answer is written when the agent finishes its turn, which is tens of
 * seconds away, so this looks for it rather than leaving the reader wondering
 * whether anything happened. It gives up after the daemon's own timeout, by
 * which point the daemon has written whatever it could.
 */
async function waitForAnswer(taskId: string, threadId: string, had: number) {
  for (let i = 0; i < 60; i++) {
    await new Promise((r) => setTimeout(r, 3000))
    await loadThreads(taskId)
    const thread = (threads.value[taskId] ?? []).find((t) => t.id === threadId)
    if ((thread?.comments.length ?? 0) > had) break
  }
  const still = new Set(awaiting.value)
  still.delete(threadId)
  awaiting.value = still
}

async function raise(taskId: string | undefined, threadId: string) {
  if (!taskId) return
  reviewError.value = ''
  try {
    await api.raiseReviewThread(threadId)
    await loadThreads(taskId)
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

function composingHere(taskId: string | undefined, file: string): boolean {
  return composing.value?.task === taskId && composing.value?.file === file
}

async function startThread(approval: { id: string; commit?: string }) {
  const at = composing.value
  if (!at || !draft.value.trim()) return
  reviewError.value = ''
  try {
    await api.openReviewThread(at.task, {
      approvalId: approval.id,
      commitSha: approval.commit,
      file: at.file,
      line: at.line,
      body: draft.value,
    })
    composing.value = null
    draft.value = ''
    await loadThreads(at.task)
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

async function reply(taskId: string | undefined, threadId: string) {
  const text = replies.value[threadId]
  if (!taskId || !text?.trim()) return
  reviewError.value = ''
  try {
    await api.addReviewComment(threadId, text)
    replies.value = { ...replies.value, [threadId]: '' }
    await loadThreads(taskId)
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

async function settle(taskId: string | undefined, thread: ReviewThread) {
  if (!taskId) return
  reviewError.value = ''
  try {
    await api.resolveReviewThread(thread.id, thread.state === 'open')
    await loadThreads(taskId)
  } catch (e) {
    reviewError.value = e instanceof Error ? e.message : String(e)
  }
}

/**
 * Whether approving would actually land the work.
 *
 * Asked when the diff is, and separately from it: the merge runs in memory, so
 * it costs nothing and leaves nothing behind, and the answer is the one thing
 * about an approval that the diff cannot show. A diff read against a base that
 * has moved since the work was written is not a diff of what will land, which
 * is the ordinary way a clean-looking approval fails at the merge.
 */
const merges = ref<Record<string, Mergeable | { error: string } | undefined>>({})

async function loadMergeable(id: string) {
  try {
    merges.value = { ...merges.value, [id]: await api.approvalMergeable(id) }
  } catch (e) {
    merges.value = {
      ...merges.value,
      [id]: { error: e instanceof Error ? e.message : String(e) },
    }
  }
}

function mergeState(id: string): Mergeable | undefined {
  const m = merges.value[id]
  return m && !('error' in m) ? m : undefined
}
/** The branch this would land on, as the diff endpoint reported it. */
function base(id: string): string {
  return diffs.value[id]?.base || 'the base branch'
}

function mergeError(id: string): string {
  const m = merges.value[id]
  return m && 'error' in m ? m.error : ''
}

/**
 * The agent's orientation, per approval: the objective, the map, the order.
 *
 * The single biggest cost of reviewing someone else's change is the first ten
 * minutes of working out what it is trying to do. The guide is that part,
 * written by the agent that can read the whole repository, cached against the
 * commit it describes, and explicitly not a review: it says what, the person
 * says whether.
 */
const guides = ref<Record<string, { body?: string; pending?: boolean; error?: string }>>({})

async function loadGuide(id: string) {
  try {
    const g = await api.approvalGuide(id)
    guides.value = { ...guides.value, [id]: { body: g.body } }
  } catch {
    // No guide yet, or one for an earlier revision: the offer renders instead.
  }
}

async function requestGuide(id: string) {
  guides.value = { ...guides.value, [id]: { pending: true } }
  try {
    await api.requestGuide(id)
  } catch (e) {
    guides.value = {
      ...guides.value,
      [id]: { error: e instanceof Error ? e.message : String(e) },
    }
    return
  }
  // The agent reads in the background; poll until its note lands. Bounded, so
  // an agent that dies quietly hands back the offer rather than a spinner.
  for (let i = 0; i < 45; i++) {
    await new Promise((r) => setTimeout(r, 4000))
    try {
      const g = await api.approvalGuide(id)
      guides.value = { ...guides.value, [id]: { body: g.body } }
      return
    } catch {
      // still reading
    }
  }
  guides.value = {
    ...guides.value,
    [id]: { error: 'No guide arrived. The agent may not be configured; try again.' },
  }
}

watch(
  () => props.attention?.approvals?.map((a) => a.id).join(',') ?? '',
  () => {
    for (const a of props.attention?.approvals ?? []) {
      if (a.commit && !diffs.value[a.id]) void loadFiles(a.id)
      if (a.commit && !guides.value[a.id]) void loadGuide(a.id)
      if (a.commit && !merges.value[a.id]) void loadMergeable(a.id)
      if (a.taskId && !threads.value[a.taskId]) void loadThreads(a.taskId)
    }
  },
  { immediate: true },
)

async function loadFiles(id: string) {
  diffs.value = { ...diffs.value, [id]: { open: true, files: [] } }
  try {
    const r = await api.approvalDiff(id)
    diffs.value = {
      ...diffs.value,
      [id]: { open: true, files: r.files ?? [], range: r.range, base: r.base, seen: r.seen ?? [] },
    }
  } catch (e) {
    diffs.value = {
      ...diffs.value,
      [id]: { open: true, files: [], error: e instanceof Error ? e.message : String(e) },
    }
  }
}

async function toggleDiff(id: string) {
  const cur = diffs.value[id]
  if (cur?.open) {
    diffs.value = { ...diffs.value, [id]: { ...cur, open: false } }
    return
  }
  if (cur?.files?.length) {
    diffs.value = { ...diffs.value, [id]: { ...cur, open: true } }
    return
  }
  await loadFiles(id)
}

const notes = ref<Record<string, string>>({})
const answers = ref<Record<string, string>>({})

function empty(a: Attention | null): boolean {
  if (!a) return true
  return !a.approvals.length && !a.clarifications.length && !a.rework.tasks.length
}
</script>

<template>
  <div v-if="props.attention" ref="root" class="flex flex-col gap-3">
    <!-- Nothing waiting should feel like calm, not like a broken panel. -->
    <div
      v-if="empty(props.attention)"
      class="text-muted-foreground flex flex-col items-center gap-1 py-12 text-center"
    >
      <span class="text-[var(--status-good)]/70 text-lg leading-none">✓</span>
      <p class="text-xs">Nothing needs you.</p>
      <p class="text-[11px]">Approvals, questions and looping cards appear here.</p>
    </div>

    <!-- Approvals: a spec waiting to be read before anything downstream runs. -->
    <article
      v-for="a in props.attention.approvals"
      :key="a.id"
      class="rise bg-card border-l-primary border border-l-2 p-3"
    >
      <div class="mb-2.5 flex flex-wrap items-center gap-2">
        <Badge>approval</Badge>
        <span class="text-xs font-semibold">{{ a.taskName || 'untitled' }}</span>
        <span class="text-muted-foreground text-[11px]">from {{ a.fromRole }}</span>
      </div>
      <!-- What the role decided. The note is the substance of the decision and
           was the first thing missing from this card. -->
      <div v-if="a.body" class="mb-2.5">
        <div
          class="md min-w-0 overflow-x-auto text-xs leading-relaxed"
          :class="notesOpen[a.id] ? '' : 'line-clamp-4'"
          v-html="renderMarkdown(a.body)"
        />
        <!-- The note is a covering line; the document below it is the work.
             Long notes are clamped rather than given the top of the card. -->
        <button
          v-if="(a.body?.length ?? 0) > 280"
          type="button"
          class="text-muted-foreground hover:text-foreground mt-0.5 text-[11px]"
          @click="notesOpen[a.id] = !notesOpen[a.id]"
        >
          {{ notesOpen[a.id] ? 'Less' : 'More' }}
        </button>
      </div>

      <!-- What this produced, at the moment somebody is deciding about it.
           For anything with a screen, "what does it look like" is the question
           that comes before "what changed". -->
      <Artifacts
        :task-id="a.taskId"
        :project-id="props.projectId ?? undefined"
        :commit="a.commit"
        class="mb-2.5"
      />

      <!-- And what it actually wrote. Deciding from a description of a change
           rather than the change is approving blind, and for a planner's spec
           the committed file *is* the deliverable. Loaded on demand: most
           approvals are read, not all are expanded, and a diff is far larger
           than anything else here. -->
      <div v-if="a.commit" class="mb-2.5">
        <button
          v-if="diffs[a.id]?.files.length"
          type="button"
          class="text-muted-foreground hover:text-foreground mb-1.5 flex min-h-8 items-center gap-1 text-[11px] sm:min-h-0"
          @click="toggleDiff(a.id)"
        >
          <ChevronRight
            :size="12"
            :class="['transition-transform', diffs[a.id]?.open ? 'rotate-90' : '']"
            aria-hidden="true"
          />
          {{ diffs[a.id]?.open ? 'Hide' : 'Show' }}
          {{ diffs[a.id]!.files.length === 1 ? diffs[a.id]!.files[0].path : `${diffs[a.id]!.files.length} files` }}
          <code class="ml-1 opacity-70">{{ a.commit!.slice(0, 8) }}</code>
        </button>

        <div v-if="diffs[a.id]?.open" :data-approval="a.id" class="mt-2">
          <p v-if="diffs[a.id]?.error" class="text-destructive text-[11px]">
            {{ diffs[a.id]?.error }}
          </p>
          <p v-else-if="!diffs[a.id]?.files.length" class="text-muted-foreground text-[11px]">
            Loading…
          </p>

          <p
            v-if="diffs[a.id]?.range && diffs[a.id]?.files.length"
            class="text-muted-foreground mb-1.5 text-[11px]"
          >
            Everything that would land on <code>{{ diffs[a.id]?.base }}</code>,
            {{ diffs[a.id]!.files.length }}
            {{ diffs[a.id]!.files.length === 1 ? 'file' : 'files' }}.
          </p>

          <!-- The agent's orientation, above the files it describes. Not a
               review: it says what the change is for and where to start, and
               the decision stays with the person reading. -->
          <div
            v-if="guides[a.id]?.body"
            class="border-l-primary bg-muted/20 mb-1.5 border border-l-2 p-2"
          >
            <div class="text-muted-foreground mb-1 flex items-center gap-1.5 text-[10px]">
              <BookOpen :size="10" aria-hidden="true" class="shrink-0" />
              <span>reading guide · written by the project's agent · it describes, you decide</span>
              <button
                type="button"
                class="hover:text-foreground focus-visible:outline-ring ml-auto shrink-0 underline-offset-2 hover:underline focus-visible:outline-2"
                title="Ask the agent to read the change again and rewrite this"
                @click="requestGuide(a.id)"
              >
                rewrite it
              </button>
            </div>
            <div
              class="md min-w-0 overflow-x-auto text-xs leading-relaxed"
              v-html="renderMarkdown(guides[a.id]!.body!)"
            />
          </div>
          <div
            v-else-if="diffs[a.id]?.files.length"
            class="bg-muted/20 mb-1.5 flex flex-wrap items-center gap-2 border border-dashed px-2 py-1.5"
          >
            <template v-if="guides[a.id]?.pending">
              <BookOpen :size="12" aria-hidden="true" class="text-muted-foreground shrink-0" />
              <span class="text-muted-foreground text-[11px] italic">
                the agent is reading the change…
              </span>
            </template>
            <template v-else>
              <button
                type="button"
                class="hover:text-foreground text-muted-foreground focus-visible:outline-ring flex items-center gap-1.5 text-[11px] font-medium focus-visible:outline-2"
                @click="requestGuide(a.id)"
              >
                <BookOpen :size="12" aria-hidden="true" class="text-primary shrink-0" />
                Map this change
              </button>
              <span class="text-muted-foreground text-[10px]">
                what it is for, what each file adds, where to start · it describes, you decide
              </span>
              <span v-if="guides[a.id]?.error" class="text-destructive w-full text-[10px]">
                {{ guides[a.id]?.error }}
              </span>
            </template>
          </div>

          <!-- Where you are and what is left, pinned.
               A twelve-file diff is otherwise one long scroll with no sense of
               progress, and this one may have been started on a phone
               yesterday. Sticky because next is useless at the top of a
               forty-seven file change: by the time you have read a file, the
               control for leaving it has scrolled away, and scrolling back up
               to press it loses your place. It names the file it is over, so
               it reads as attached to what is under it rather than to the
               card. -->
          <div
            v-if="(diffs[a.id]?.files.length ?? 0) > 1"
            class="bg-card sticky top-0 z-20 mb-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 border px-2 py-1 text-[10px] shadow-sm"
          >
            <span class="text-muted-foreground tabular-nums">
              file {{ positionOf(a.id).at }} of {{ positionOf(a.id).of }}
            </span>
            <span class="min-w-0 max-w-[45%] truncate font-mono" :title="reading(a.id)">
              {{ reading(a.id) }}
            </span>
            <span v-if="unread(a.id)" class="text-muted-foreground">
              · {{ unread(a.id) }} not read yet
            </span>
            <span v-else class="text-[var(--status-good)]">· all read</span>
            <span class="ml-auto flex items-center gap-1">
              <Button size="xs"
                variant="ghost"
                class="h-7 px-2 text-[11px] sm:h-5 sm:px-1.5 sm:text-[10px]" @click="step(a.id, -1)">
                previous
              </Button>
              <Button size="xs"
                variant="ghost"
                class="h-7 px-2 text-[11px] sm:h-5 sm:px-1.5 sm:text-[10px]" @click="step(a.id, 1)">
                next
              </Button>
              <Button
                v-if="unread(a.id)"
                size="xs"
                variant="outline"
                class="h-7 px-2 text-[11px] sm:h-5 sm:px-1.5 sm:text-[10px]"
                title="The next file you have not read (j)"
                @click="nextUnread(a.id)"
              >
                next unread
              </Button>
            </span>
          </div>
          <p class="text-muted-foreground mb-1.5 hidden text-[10px] sm:block">
            j and k move, and mark what you leave as read
          </p>

          <div
            v-for="f in diffs[a.id]?.files ?? []"
            :key="f.path"
            :data-file="f.path"
            :class="[
              'mb-3 scroll-mt-12 border last:mb-0',
              reading(a.id) === f.path && 'border-l-2 border-l-[var(--primary)]',
              isSeen(a.id, f.path) && reading(a.id) !== f.path && 'opacity-70',
            ]"
          >
            <!-- The header is the toggle. A file you are not reading should
                 cost one line, not a screen. -->
            <button
              type="button"
              class="hairline-b hover:bg-muted focus-visible:outline-ring flex w-full items-center gap-2 px-2 py-1 text-left focus-visible:outline-2 focus-visible:-outline-offset-2"
              @click="toggleFile(a, f), f.deferred && loadOne(a.id, f.path)"
            >
              <ChevronRight
                :size="12"
                :class="['shrink-0 transition-transform', defaultOpen(a, f) ? 'rotate-90' : '']"
                aria-hidden="true"
              />
              <Badge variant="outline">{{ f.status }}</Badge>
              <code class="min-w-0 flex-1 truncate text-[11px]">{{ f.path }}</code>
              <span class="text-muted-foreground shrink-0 font-mono text-[10px] tabular-nums">
                {{ stat(f) }}
              </span>
              <span v-if="isDoc(f)" class="text-muted-foreground shrink-0 text-[10px]">doc</span>
              <!-- Files with something open on them, so "next" can mean the
                   next thing that needs you. -->
              <span
                v-if="threadsFor(a.taskId, f.path).some((t) => t.kind === 'remark' && t.state === 'open')"
                class="shrink-0 text-[10px] text-[var(--status-warning)]"
              >
                open remark
              </span>
            </button>

            <!-- The reader's own mark. Scrolling past a file is not reading it,
                 and a review that decides that for you is worse than one that
                 keeps asking. -->
            <label
              class="hairline-b text-muted-foreground flex items-center gap-2 px-2 py-2 text-[11px] sm:gap-1.5 sm:py-1 sm:text-[10px]"
            >
              <input
                type="checkbox"
                class="size-4 sm:size-3"
                :checked="isSeen(a.id, f.path)"
                :aria-label="`Mark ${f.path} read`"
                @change="setSeen(a.id, f.path, ($event.target as HTMLInputElement).checked)"
              />
              read
            </label>

            <!-- A document, rendered. This is the thing being approved; showing
                 it as a diff makes the reader reconstruct it line by line. -->
            <div
              v-if="isDoc(f) && defaultOpen(a, f)"
              class="md max-h-[26rem] min-w-0 overflow-auto px-3 py-2 text-xs leading-relaxed"
              v-html="renderMarkdown(f.content ?? '')"
            />
            <!-- What was not read, and why. A file with no diff is otherwise
                 indistinguishable from a file that did not change. -->
            <p
              v-else-if="defaultOpen(a, f) && f.binary"
              class="text-muted-foreground px-2 py-1.5 text-[11px]"
            >
              Binary file, {{ f.status === 'D' ? 'removed' : 'changed' }}. Nothing to read here.
            </p>
            <p
              v-else-if="defaultOpen(a, f) && f.tooLarge"
              class="text-muted-foreground px-2 py-1.5 text-[11px]"
            >
              Too large to show: {{ f.added }} added, {{ f.removed }} removed. Read it in the
              repository.
            </p>
            <p
              v-else-if="defaultOpen(a, f) && f.deferred"
              class="text-muted-foreground px-2 py-1.5 text-[11px]"
            >
              {{ loading.has(f.path) ? 'Loading…' : 'Not read yet. Open it to load this file.' }}
            </p>
            <div v-else-if="defaultOpen(a, f)" class="max-h-96 overflow-y-auto py-1">
              <DiffView
                v-if="f.diff"
                :diff="f.diff"
                :discussed="discussed(a.taskId, f.path)"
                @comment="(line, hunk) => compose(a.taskId, f.path, line, hunk)"
              />
              <p v-else class="text-muted-foreground px-2 text-[11px]">(no diff)</p>
            </div>

            <!-- The conversation about this file, under it. A remark points at
                 a line; an answer lands on the thread that asked rather than
                 arriving as an unrelated handoff. -->
            <div
              v-if="defaultOpen(a, f) && (threadsFor(a.taskId, f.path).length || composingHere(a.taskId, f.path))"
              class="hairline-t flex flex-col gap-2 px-2 py-2"
            >
              <ReviewThreadView
                v-for="t in threadsFor(a.taskId, f.path)"
                :key="t.id"
                v-model:reply="replies[t.id]"
                :thread="t"
                :awaiting="awaiting.has(t.id)"
                @settle="settle(a.taskId, t)"
                @raise="raise(a.taskId, t.id)"
                @send="reply(a.taskId, t.id)"
                @ask="askAgain(a, a.taskId, t)"
              />

              <div v-if="composingHere(a.taskId, f.path)" class="flex flex-col gap-1">
                <p class="text-muted-foreground text-[10px]">
                  <template v-if="composing?.intent === 'ask'">asking about</template>
                  <template v-else>a remark on</template>
                  <template v-if="composing?.line"> line {{ composing?.line }}</template>
                  <template v-else> this file</template>
                  <template v-if="composing?.intent === 'ask'">
                    · the agent answers here, it decides nothing
                  </template>
                  <template v-else> · it holds the merge until you settle it</template>
                </p>
                <InputGroup>
                  <InputGroupInput
                    v-model="draft"
                    data-composer
                    :placeholder="
                      composing?.intent === 'ask'
                        ? 'what do you want to know about this code?'
                        : 'what is wrong here, or what should change'
                    "
                    @keyup.enter="
                      composing?.intent === 'ask' ? ask(a, a.taskId, draft) : startThread(a)
                    "
                  />
                  <InputGroupAddon :align="narrow ? 'block-end' : 'inline-end'">
                    <!-- The act the reader asked for leads; the other is beside
                         it for a mind changed halfway through the sentence. -->
                    <InputGroupButton
                      v-if="composing?.intent === 'ask'"
                      variant="default"
                      size="sm"
                      :disabled="!draft.trim()"
                      title="Ask the project's agent. It answers here; it does not decide anything."
                      @click="ask(a, a.taskId, draft)"
                    >
                      Ask
                    </InputGroupButton>
                    <InputGroupButton
                      v-else
                      variant="default"
                      size="sm"
                      :disabled="!draft.trim()"
                      @click="startThread(a)"
                    >
                      Remark
                    </InputGroupButton>
                    <InputGroupButton
                      v-if="composing?.intent === 'ask'"
                      size="sm"
                      variant="ghost"
                      :disabled="!draft.trim()"
                      @click="startThread(a)"
                    >
                      Remark instead
                    </InputGroupButton>
                    <InputGroupButton
                      v-else
                      size="sm"
                      variant="ghost"
                      :disabled="!draft.trim()"
                      title="Ask the project's agent. It answers here; it does not decide anything."
                      @click="ask(a, a.taskId, draft)"
                    >
                      Ask instead
                    </InputGroupButton>
                    <InputGroupButton size="sm" variant="ghost" @click="composing = null">
                      Cancel
                    </InputGroupButton>
                  </InputGroupAddon>
                </InputGroup>
                <!-- The questions a reader actually asks at a hunk, one press
                     away and editable before sending: they go in the box. A
                     question is an ask, so pressing one makes the box an ask,
                     whichever way it was opened. -->
                <div class="flex flex-wrap gap-1">
                  <button
                    v-for="q in PROMPTS"
                    :key="q"
                    type="button"
                    class="text-muted-foreground hover:text-foreground focus-visible:outline-ring border px-2 py-1.5 text-[11px] focus-visible:outline-2 sm:px-1.5 sm:py-0.5 sm:text-[10px]"
                    @click="askThis(q)"
                  >
                    {{ q }}
                  </button>
                </div>
              </div>
            </div>

            <!-- The two things to do about a file rather than a line: a
                 remark a line cannot carry (a file that should not exist, or
                 one that is missing), and a question about it. One button
                 saying "comment on this file" covered both and named neither,
                 and what it mostly opened was a question to an agent. -->
            <div
              v-if="defaultOpen(a, f) && !composingHere(a.taskId, f.path)"
              class="hairline-t flex"
            >
              <button
                type="button"
                class="hover:bg-muted focus-visible:outline-ring flex flex-1 items-center gap-1.5 px-2 py-2.5 text-left text-[11px] font-medium focus-visible:outline-2 focus-visible:-outline-offset-2 sm:py-1.5"
                @click="compose(a.taskId, f.path, 0, '', 'ask')"
              >
                <HelpCircle :size="12" aria-hidden="true" class="text-primary shrink-0" />
                Ask about this file
              </button>
              <button
                type="button"
                class="hover:bg-muted focus-visible:outline-ring flex flex-1 items-center justify-end gap-1.5 px-2 py-2.5 text-right text-[11px] font-medium focus-visible:outline-2 focus-visible:-outline-offset-2 sm:py-1.5"
                @click="compose(a.taskId, f.path, 0, '', 'remark')"
              >
                Remark on this file
                <MessageSquare :size="12" aria-hidden="true" class="text-[var(--status-warning)] shrink-0" />
              </button>
            </div>
          </div>

          <!-- Threads whose file is not in this revision. They still hold the
               merge, and under the old shape they were on screen nowhere: the
               daemon refused to approve, named a count, and there was nothing
               to settle. -->
          <div
            v-if="orphanThreads(a.taskId, a.id).length"
            class="mt-3 border border-dashed p-2"
          >
            <p class="text-muted-foreground mb-2 text-[10px]">
              From an earlier revision. These files are not in the change as it stands now, and
              these remarks still have to be settled.
            </p>
            <div class="flex flex-col gap-2">
              <ReviewThreadView
                v-for="t in orphanThreads(a.taskId, a.id)"
                :key="t.id"
                v-model:reply="replies[t.id]"
                :thread="t"
                :awaiting="awaiting.has(t.id)"
                anchored
                @settle="settle(a.taskId, t)"
                @raise="raise(a.taskId, t.id)"
                @send="reply(a.taskId, t.id)"
                @ask="askAgain(a, a.taskId, t)"
              />
            </div>
          </div>
        </div>
      </div>

      <p v-if="reviewError" class="text-destructive text-[11px]">{{ reviewError }}</p>

      <!-- What is still unsettled, next to the button it stops. The daemon
           refuses the approval as well; this is so the reason is on screen
           before it is pressed rather than after. -->
      <p v-if="openCount(a.taskId)" class="flex items-center gap-1.5 text-[11px]">
        <MessageSquare :size="12" class="text-[var(--status-warning)] shrink-0" aria-hidden="true" />
        <span class="text-[var(--status-warning)]">
          {{ openCount(a.taskId) }} open
          {{ openCount(a.taskId) === 1 ? 'thread' : 'threads' }}: settle them, or reject
        </span>
      </p>

      <!-- Whether it would land, beside the button that lands it. An approval
           is the moment a person decides, and until now nothing said whether
           the decision could be carried out. -->
      <p
        v-if="a.commit && (mergeState(a.id) || mergeError(a.id))"
        class="mt-1 mb-2.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px]"
      >
        <template v-if="mergeState(a.id)?.clean">
          <GitMerge :size="12" class="text-[var(--status-good)] shrink-0" aria-hidden="true" />
          <span class="text-muted-foreground">merges cleanly</span>
        </template>
        <!-- Diverged rather than conflicted: the merge itself is fine and the
             landing still refuses, because this project fast-forwards. What
             fixes it is a rebase, not a resolution, so it does not read as a
             conflict. -->
        <template v-else-if="mergeState(a.id)?.diverged">
          <AlertTriangle :size="12" class="text-[var(--status-warning)] shrink-0" aria-hidden="true" />
          <span class="text-[var(--status-warning)] min-w-0 flex-1">
            will not fast-forward: this is behind {{ base(a.id) }} and has to be rebased on it
          </span>
        </template>
        <template v-else-if="mergeState(a.id)">
          <AlertTriangle :size="12" class="text-[var(--status-warning)] shrink-0" aria-hidden="true" />
          <span class="text-[var(--status-warning)] min-w-0 flex-1">
            conflicts with {{ base(a.id) }} in
            {{ (mergeState(a.id)?.conflicts ?? []).join(', ') }}
          </span>
        </template>
        <span v-else class="text-muted-foreground">{{ mergeError(a.id) }}</span>
        <!-- Said even when the merge is clean: it is the difference between
             reviewing what will land and reviewing what was written. -->
        <span v-if="(mergeState(a.id)?.baseAhead ?? 0) > 0" class="text-muted-foreground">
          · {{ base(a.id) }} has moved {{ mergeState(a.id)?.baseAhead }}
          commit{{ mergeState(a.id)?.baseAhead === 1 ? '' : 's' }} since this was written
        </span>
      </p>

      <!-- The note and the two decisions are one control: the text only means
           anything to Reject, and a field floating beside two buttons did not
           say which. -->
      <InputGroup class="h-11 has-[>[data-align=block-end]]:h-auto">
        <InputGroupInput
          v-model="notes[a.id]"
          class="h-11 text-sm md:text-sm"
          placeholder="reason, if rejecting"
        />
        <InputGroupAddon :align="narrow ? 'block-end' : 'inline-end'" class="gap-1.5">
          <!-- The mark rather than the word. Two buttons whose text differs by
               one syllable are read by shape long before they are read by
               letter, and the shapes here should be as different as the two
               outcomes are: a tick that lands the work, a cross that sends it
               back. Both keep their name for anyone hovering, tabbing or
               listening, since an icon alone tells a screen reader nothing. -->
          <InputGroupButton
            variant="default"
            size="sm"
            class="h-9 w-11 [&>svg]:size-4.5"
            :disabled="deciding(a.id)"
            title="Approve: land this work"
            aria-label="Approve"
            @click="emit('approve', a.id)"
          >
            <Check aria-hidden="true" />
          </InputGroupButton>
          <InputGroupButton
            variant="destructive"
            size="sm"
            class="h-9 w-11 [&>svg]:size-4.5"
            :disabled="deciding(a.id)"
            title="Reject: send it back to whoever wrote it, with the review"
            aria-label="Reject"
            @click="emit('reject', a.id, notes[a.id] ?? '')"
          >
            <X aria-hidden="true" />
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
    </article>

    <!-- Questions. Without somewhere to put these, an agent waiting on an
         answer looks exactly like one that stopped for no reason. -->
    <article
      v-for="c in props.attention.clarifications"
      :key="c.id"
      class="rise bg-card border border-l-2 border-l-[var(--status-warning)] p-3"
    >
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <Badge variant="secondary">question</Badge>
        <span class="text-muted-foreground text-[11px]">{{ c.role }} asks</span>
      </div>
      <!-- Markdown, as an approval's note already is. An agent writes its
           question the way it writes everything else — backticks around the
           values it is asking about, a blank line between the decision and the
           ones it is only flagging — and rendering it as one flat string
           collapsed the paragraph breaks too, which is what turned a structured
           question into a wall. The renderer escapes before it builds any tag,
           so nothing an agent read out of the repository becomes markup. -->
      <div class="md mb-2.5 min-w-0 overflow-x-auto text-xs leading-relaxed break-words" v-html="renderMarkdown(c.question)" />
      <InputGroup>
        <InputGroupInput
          v-model="answers[c.id]"
          placeholder="your answer"
          :disabled="answering(c.id)"
          @keyup.enter="emit('answer', c.id, answers[c.id] ?? '')"
        />
        <InputGroupAddon align="inline-end">
          <InputGroupButton
            variant="default"
            size="sm"
            :disabled="answering(c.id)"
            @click="emit('answer', c.id, answers[c.id] ?? '')"
          >
            Answer
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
    </article>

    <!-- Cards going in circles. Informational: rework is legitimate, and
         blocking the first bounce would train everyone to ignore this panel. -->
    <article
      v-for="t in props.attention.rework.tasks"
      :key="t.id"
      class="rise border border-l-2 border-[var(--status-warning)]/35 border-l-[var(--status-warning)] bg-[var(--status-warning)]/[0.06] p-3"
    >
      <div class="mb-1.5 flex flex-wrap items-center gap-2">
        <Badge variant="secondary">looping</Badge>
        <span class="text-xs font-semibold">{{ t.name }}</span>
      </div>
      <p class="text-muted-foreground text-[11px] leading-relaxed">
        Sent backward <span class="tabular text-foreground">{{ t.reworkCount }}</span> times, at or
        over the threshold of {{ props.attention.rework.threshold }}. Two roles probably disagree
        about something worth settling.
      </p>
    </article>
  </div>
</template>

<script setup lang="ts">
/**
 * A unified diff, coloured.
 *
 * Rendered line by line rather than dumped into a <pre>, because at the final
 * gate this is the thing being decided and a wall of monospace with leading
 * plus and minus signs is a thing people skim rather than read. Additions and
 * removals get a ground; hunk headers get a rule; everything else stays quiet.
 *
 * Line numbers are tracked from the hunk headers, so a finding can be pointed
 * at rather than described — "the third line of the second block" is not a
 * useful thing to say to anyone.
 */
import { computed, ref } from 'vue'

const props = defineProps<{
  diff: string
  /** Lines that already carry a review thread, marked so a reader can see
   *  where the conversation is without opening anything. */
  discussed?: number[]
}>()

/**
 * A line is where a remark points.
 *
 * The gutter is the target rather than the whole row: selecting the code to
 * copy it is the other thing people do here, and a row that opens a comment box
 * on every click fights that.
 */
const emit = defineEmits<{ comment: [line: number, hunk: string] }>()

/**
 * The lines around a click, which is what a question about "this" needs.
 *
 * Sent with the question rather than fetched from the commit, so the agent is
 * asked about what the reader is actually looking at, including the removals:
 * a hunk without them is a question about the result rather than the change.
 */
function hunkAround(index: number): string {
  const start = Math.max(0, index - 6)
  return rows.value
    .slice(start, index + 7)
    .filter((r) => r.kind !== 'meta')
    .map((r) => (r.kind === 'add' ? '+' : r.kind === 'del' ? '-' : ' ') + r.text)
    .join('\n')
}

const marked = computed(() => new Set(props.discussed ?? []))

/** The line a remark on this row would point at: the version that still exists
 *  after the change, falling back to the one it replaced. */
function anchor(r: Row): number {
  return r.newNo ?? r.oldNo ?? 0
}

/** One run of a changed line: the characters, and whether they are the edit. */
interface Seg {
  text: string
  hot: boolean
}

/**
 * What actually changed inside each changed line.
 *
 * A whole line painted red and its replacement painted green makes the reader
 * diff them by eye, which is the machine's job. Where a removed run of lines is
 * replaced by an equal run, each pair is compared token by token and only the
 * tokens that differ get the strong ground; renaming one argument in a long
 * signature reads as that one argument.
 *
 * Equal-length runs only. Pairing three removed lines with one added one
 * highlights nonsense, and the tools that try are wrong often enough that not
 * trying reads better. A pair that shares less than a third of its tokens gets
 * no marking either: everything hot is the same as nothing hot.
 */
const segs = computed<Map<number, Seg[]>>(() => {
  const out = new Map<number, Seg[]>()
  const r = rows.value
  for (let i = 0; i < r.length; i++) {
    if (r[i].kind !== 'del') continue
    let j = i
    while (j < r.length && r[j].kind === 'del') j++
    let k = j
    while (k < r.length && r[k].kind === 'add') k++
    if (k - j === j - i) {
      for (let p = 0; p < j - i; p++) {
        const pair = pairSegs(r[i + p].text, r[j + p].text)
        if (pair) {
          out.set(i + p, pair[0])
          out.set(j + p, pair[1])
        }
      }
    }
    i = k - 1
  }
  return out
})

/** Words, runs of spaces, and single other characters: the units an edit is
 *  read in. */
function tokens(s: string): string[] {
  return s.match(/\w+|\s+|[^\w\s]/g) ?? []
}

function pairSegs(a: string, b: string): [Seg[], Seg[]] | null {
  const ta = tokens(a)
  const tb = tokens(b)
  const n = ta.length
  const m = tb.length
  // A line long enough to make the comparison quadratic in a way that matters
  // is a line nobody is reading token by token anyway.
  if (!n || !m || n > 300 || m > 300) return null

  const dp: Uint16Array[] = Array.from({ length: n + 1 }, () => new Uint16Array(m + 1))
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = ta[i] === tb[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
    }
  }
  // Lines that share too little are a rewrite, not an edit. Measured over the
  // words: every pair of lines shares spaces and parentheses, and counting
  // those made a full rewrite look like an edit with most of its line hot.
  const words = (t: string[]) => t.filter((x) => /\w/.test(x)).length
  const shared = sharedWords(ta, tb, dp)
  if ((2 * shared) / Math.max(1, words(ta) + words(tb)) < 0.3) return null

  const keepA = new Array<boolean>(n).fill(false)
  const keepB = new Array<boolean>(m).fill(false)
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (ta[i] === tb[j]) {
      keepA[i] = keepB[j] = true
      i++
      j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      i++
    } else {
      j++
    }
  }
  return [runs(ta, keepA), runs(tb, keepB)]
}

/** How many word tokens the LCS keeps, walking the same path the marking does. */
function sharedWords(ta: string[], tb: string[], dp: Uint16Array[]): number {
  let i = 0
  let j = 0
  let shared = 0
  while (i < ta.length && j < tb.length) {
    if (ta[i] === tb[j]) {
      if (/\w/.test(ta[i])) shared++
      i++
      j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      i++
    } else {
      j++
    }
  }
  return shared
}

function runs(t: string[], keep: boolean[]): Seg[] {
  const out: Seg[] = []
  for (let i = 0; i < t.length; i++) {
    const hot = !keep[i]
    if (out.length && out[out.length - 1].hot === hot) out[out.length - 1].text += t[i]
    else out.push({ text: t[i], hot })
  }
  return out
}

/**
 * A hunk header a person can read.
 *
 * "@@ -12,7 +12,9 @@ fn parse(s: &Scanner)" is git's bookkeeping. The reader
 * wants the two facts inside it: where in the file they are, and what function
 * they are in, which git already puts after the second @@ when it can find it.
 */
function hunkLabel(text: string): string {
  const m = text.match(/@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)/)
  if (!m) return text
  const newStart = Number(m[3])
  const newCount = m[4] === undefined ? 1 : Number(m[4])
  const oldStart = Number(m[1])
  const oldCount = m[2] === undefined ? 1 : Number(m[2])
  let where: string
  if (newCount === 0) {
    where = oldCount === 1 ? `removed line ${oldStart}` : `removed lines ${oldStart}-${oldStart + oldCount - 1}`
  } else {
    where = newCount === 1 ? `line ${newStart}` : `lines ${newStart}-${newStart + newCount - 1}`
  }
  return m[5] ? `${where} · ${m[5]}` : where
}

interface Row {
  kind: 'add' | 'del' | 'ctx' | 'hunk' | 'meta'
  text: string
  oldNo?: number
  newNo?: number
}

const rows = computed<Row[]>(() => {
  const out: Row[] = []
  let oldNo = 0
  let newNo = 0

  for (const line of (props.diff ?? '').split('\n')) {
    if (line.startsWith('@@')) {
      // @@ -12,7 +12,9 @@ — the two starting line numbers.
      const m = line.match(/@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
      oldNo = m ? Number(m[1]) : 0
      newNo = m ? Number(m[2]) : 0
      out.push({ kind: 'hunk', text: line })
      continue
    }
    // The file headers repeat what the caller already shows above the diff.
    if (/^(diff --git|index |--- |\+\+\+ |new file|deleted file|similarity|rename )/.test(line)) {
      out.push({ kind: 'meta', text: line })
      continue
    }
    // "\ No newline at end of file" is git's note about the line above, not a
    // line of the file. Counted as context it advanced both numbers, so every
    // line after it in that file was numbered one too high and a remark left
    // on one pointed at the wrong place.
    if (line.startsWith('\\')) {
      out.push({ kind: 'meta', text: line })
      continue
    }
    // The split leaves an empty tail after the final newline. Rendered as
    // context it is a blank row at the end of every diff; git writes a blank
    // context line as a single space, so nothing real is lost by skipping it.
    if (line === '') continue
    if (line.startsWith('+')) {
      out.push({ kind: 'add', text: line.slice(1), newNo: newNo++ })
    } else if (line.startsWith('-')) {
      out.push({ kind: 'del', text: line.slice(1), oldNo: oldNo++ })
    } else {
      out.push({ kind: 'ctx', text: line.slice(1), oldNo: oldNo++, newNo: newNo++ })
    }
  }
  return out
})

/**
 * Hunks whose change is only whitespace.
 *
 * A reformat that touches four hundred lines and changes nothing is the most
 * common way a diff becomes unreadable: the change worth reading is somewhere
 * in it, and finding it means checking every line to see whether anything moved
 * but the indentation.
 *
 * Runs of whitespace collapsed to one space, not stripped. Stripped, "admin
 * user" and "adminuser" compare equal and a semantic change is labelled
 * whitespace; collapsed, only spacing differences match, which is what the
 * word is supposed to mean. Indentation still folds, which is the case this is
 * for -- including a Python re-indent, which is why the reader asks for the
 * fold rather than arriving to find it already done.
 */
const space = (text: string) => text.replace(/\s+/g, ' ').trim()
const hunks = computed(() => {
  const out: { start: number; end: number; whitespaceOnly: boolean }[] = []
  let start = -1
  const close = (end: number) => {
    if (start < 0) return
    const slice = rows.value.slice(start, end)
    const added = slice.filter((r) => r.kind === 'add').map((r) => space(r.text))
    const removed = slice.filter((r) => r.kind === 'del').map((r) => space(r.text))
    out.push({
      start,
      end,
      whitespaceOnly:
        added.length > 0 &&
        added.length === removed.length &&
        added.every((line, i) => line === removed[i]),
    })
    start = -1
  }
  rows.value.forEach((r, i) => {
    if (r.kind === 'hunk') {
      close(i)
      start = i
    }
  })
  close(rows.value.length)
  return out
})

/** Which hunk a row belongs to, and whether that hunk is only whitespace. */
function hunkOf(index: number) {
  return hunks.value.find((h) => index >= h.start && index < h.end)
}

const unfolded = ref<Set<number>>(new Set())

/**
 * Whether whitespace-only hunks are folded, which starts false.
 *
 * Hiding part of a change by default is the panel deciding what the reader
 * does not need to see, which is the one thing a review must not do -- and the
 * detection is a guess about meaning: whitespace is syntax in Python, in YAML,
 * and in any string literal. Offered, counted, and left to the reader, the
 * same way scrolling past a file is not the same as reading it.
 */
const folding = ref(false)

/** How many hunks the fold would take out, for the offer to be specific. */
const whitespaceHunks = computed(() => hunks.value.filter((h) => h.whitespaceOnly).length)

function hidden(index: number): boolean {
  if (!folding.value) return false
  const h = hunkOf(index)
  if (!h?.whitespaceOnly || unfolded.value.has(h.start)) return false
  // The header stays: it is the line that says what was folded and offers to
  // show it.
  return index !== h.start
}

function foldedCount(index: number): number {
  if (!folding.value) return 0
  const h = hunkOf(index)
  return h?.whitespaceOnly && !unfolded.value.has(h.start) ? h.end - h.start - 1 : 0
}

function unfold(index: number) {
  const h = hunkOf(index)
  if (h) unfolded.value = new Set(unfolded.value).add(h.start)
}

/** Added and removed counts, which is the shape of a change at a glance. */
const stat = computed(() => ({
  added: rows.value.filter((r) => r.kind === 'add').length,
  removed: rows.value.filter((r) => r.kind === 'del').length,
}))
</script>

<template>
  <div class="font-mono text-[11px] leading-relaxed">
    <p class="text-muted-foreground mb-1 flex items-center gap-2 px-2">
      <span class="tabular-nums">
        <span class="text-[var(--status-good)]">+{{ stat.added }}</span>
        <span class="text-destructive ml-1.5">−{{ stat.removed }}</span>
      </span>
      <!-- Offered rather than done: what counts as "only whitespace" is a guess
           about meaning, and it is wrong in Python, in YAML and in any string
           with a space in it. -->
      <button
        v-if="whitespaceHunks"
        type="button"
        class="hover:text-foreground focus-visible:outline-ring ml-auto underline-offset-2 hover:underline focus-visible:outline-2"
        @click="folding = !folding"
      >
        {{ folding ? 'show' : 'hide' }} {{ whitespaceHunks }} whitespace-only
        {{ whitespaceHunks === 1 ? 'hunk' : 'hunks' }}
      </button>
    </p>

    <div class="overflow-x-auto">
      <div
        v-for="(r, i) in rows"
        v-show="!hidden(i)"
        :key="i"
        :class="[
          'flex gap-2 px-2 whitespace-pre',
          r.kind === 'add' && 'bg-[var(--status-good)]/10',
          r.kind === 'del' && 'bg-destructive/10',
          r.kind === 'hunk' && 'text-muted-foreground bg-muted/60 my-1',
          r.kind === 'meta' && 'text-muted-foreground/60',
        ]"
      >
        <!-- Both sides, so a line can be cited by number in either version.
             The new-side number is also the button that starts a thread there:
             the gutter rather than the row, so selecting code to copy it still
             works. -->
        <span class="text-muted-foreground/50 w-8 shrink-0 text-right tabular-nums select-none">
          {{ r.oldNo ?? '' }}
        </span>
        <button
          v-if="anchor(r) && r.kind !== 'meta' && r.kind !== 'hunk'"
          type="button"
          :class="[
            'w-8 shrink-0 text-right tabular-nums select-none',
            marked.has(anchor(r))
              ? 'text-[var(--primary)] font-semibold'
              : 'text-muted-foreground/50 hover:text-foreground',
          ]"
          :title="marked.has(anchor(r)) ? 'This line is being discussed' : 'Comment on this line'"
          :aria-label="`Comment on line ${anchor(r)}`"
          @click="emit('comment', anchor(r), hunkAround(i))"
        >
          {{ r.newNo ?? r.oldNo }}
        </button>
        <span v-else class="text-muted-foreground/50 w-8 shrink-0 text-right tabular-nums select-none">
          {{ r.newNo ?? '' }}
        </span>
        <span
          class="w-2 shrink-0 select-none"
          :class="r.kind === 'add' ? 'text-[var(--status-good)]' : 'text-destructive'"
        >
          {{ r.kind === 'add' ? '+' : r.kind === 'del' ? '−' : '' }}
        </span>
        <span v-if="r.kind === 'hunk'" class="min-w-0" :title="r.text">{{ hunkLabel(r.text) }}</span>
        <span v-else-if="segs.has(i)" class="min-w-0">
          <template v-for="(sg, si) in segs.get(i)" :key="si"
            ><span
              v-if="sg.hot"
              :class="
                r.kind === 'add'
                  ? 'bg-[var(--status-good)]/30 rounded-[2px]'
                  : 'bg-destructive/25 rounded-[2px]'
              "
              >{{ sg.text }}</span
            ><template v-else>{{ sg.text }}</template></template
          >
        </span>
        <span v-else class="min-w-0">{{ r.text }}</span>
        <!-- A reformat says so in one line rather than in four hundred. -->
        <button
          v-if="foldedCount(i)"
          type="button"
          class="text-muted-foreground hover:text-foreground focus-visible:outline-ring ml-2 shrink-0 underline-offset-2 hover:underline focus-visible:outline-2"
          @click="unfold(i)"
        >
          whitespace only, {{ foldedCount(i) }} lines hidden
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
/**
 * Browse any branch, tag or commit of a project's repository.
 *
 * Independent of any task, approval or feature -- see
 * docs/design/code-explorer.md.
 */
import { computed, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, type RepoBlob, type RepoRef, type TreeEntry } from '@/lib/api'
import { highlight } from '@/lib/highlight'
import { latest } from '@/lib/latest'
import {
  ChevronLeft,
  File,
  Folder,
  GitBranch,
  LoaderCircle,
  MessageCircleQuestion,
  Tag,
} from '@lucide/vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

const props = defineProps<{ projectId: string }>()
const emit = defineEmits<{ crumb: [label: string] }>()
const route = useRoute()
const router = useRouter()

const refs = ref<RepoRef[]>([])
const loadingRefs = ref(false)

const selectedRef = ref('')
/** The commit sha the ref resolved to, pinned once a directory is opened so
 *  a moving branch does not answer two different things to two clicks in the
 *  same session -- see decision 4. */
const resolvedSha = ref('')
const dir = ref('') // '' is the repository root
const entries = ref<TreeEntry[]>([])
const loadingTree = ref(false)

const selectedFile = ref('')
const blob = ref<RepoBlob | null>(null)
const loadingFile = ref(false)
/** Highlighted HTML for the open file, or null for plain text -- no grammar
 *  for this extension, the file was too small to bother, or highlighting
 *  timed out. See lib/highlight.ts. */
const highlighted = ref<string | null>(null)
const highlightNote = ref('')
// One sequence for every navigation request -- picking a ref, opening a
// directory, opening a file, going back. Each of those can be in flight when
// the next one starts (a slow "main" tree response landing after "other" was
// already picked, say), and only the request behind the newest action may
// write what it fetched. Going back does not fetch anything itself but still
// takes a turn, so a tree or file load already in flight when a person backs
// out of it is discarded rather than reappearing after the fact.
const nav = latest()

const error = ref('')

const branches = computed(() => refs.value.filter((r) => r.kind === 'branch'))
const tags = computed(() => refs.value.filter((r) => r.kind === 'tag'))

/** Which of the three panes a phone shows. Above sm all three are visible at
 *  once; below it, only one -- a tree and a file both scroll past a phone's
 *  height, and stacking all three the way a bounded three-column form does
 *  elsewhere in this app would mean scrolling past the whole tree every time
 *  a file is opened. */
const mobilePane = computed<'refs' | 'tree' | 'file'>(() => {
  if (selectedFile.value) return 'file'
  if (selectedRef.value) return 'tree'
  return 'refs'
})

async function loadRefs() {
  error.value = ''
  loadingRefs.value = true
  const current = nav()
  try {
    const res = await api.repoRefs(props.projectId)
    if (!current()) return
    refs.value = res
  } catch (e) {
    if (!current()) return
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (current()) loadingRefs.value = false
  }
}

async function loadTree() {
  if (!selectedRef.value) return
  loadingTree.value = true
  error.value = ''
  const current = nav()
  try {
    // Pinned to the sha this ref already resolved to, not the ref name again
    // -- decision 4. Only openRef clears resolvedSha, so opening a directory
    // underneath the ref that is already open asks about the same commit
    // rather than re-resolving a branch that may have moved since.
    const res = await api.repoTree(props.projectId, resolvedSha.value || selectedRef.value, dir.value)
    if (!current()) return
    resolvedSha.value = res.resolvedSha
    entries.value = res.entries
  } catch (e) {
    if (!current()) return
    error.value = e instanceof Error ? e.message : String(e)
    entries.value = []
  } finally {
    if (current()) loadingTree.value = false
  }
}

function openRef(name: string) {
  selectedRef.value = name
  resolvedSha.value = ''
  dir.value = ''
  selectedFile.value = ''
  blob.value = null
  loadTree()
}

/** A branch or tag typed by hand rather than picked from the list -- a
 *  commit sha, or a ref this project has that the picker does not bother
 *  filtering to (a `zerg-*` housekeeping branch, say). Same resolution as a
 *  click: an unreal one comes back as an ordinary error, not a crash. */
const manualRef = ref('')
function goToManualRef() {
  const name = manualRef.value.trim()
  if (!name) return
  openRef(name)
}

function openDir(path: string) {
  dir.value = path
  selectedFile.value = ''
  blob.value = null
  loadTree()
}

/** Up one level, the way a person reads a path: drop the last segment. */
function openParentDir() {
  const parts = dir.value.split('/').filter(Boolean)
  parts.pop()
  openDir(parts.join('/'))
}

async function openFile(path: string) {
  selectedFile.value = path
  blob.value = null
  highlighted.value = null
  highlightNote.value = ''
  loadingFile.value = true
  error.value = ''
  clearSelection()
  const current = nav()
  try {
    // The sha the tree already resolved, not the ref name again -- decision 4.
    const res = await api.repoFile(props.projectId, resolvedSha.value || selectedRef.value, path)
    if (!current()) return
    blob.value = res.blob
    loadingFile.value = false
    if (!res.blob.binary && !res.blob.tooLarge && res.blob.content) {
      const result = await highlight(path, res.blob.content)
      if (!current()) return
      if ('html' in result) highlighted.value = result.html
      else if ('timedOut' in result) highlightNote.value = 'Not highlighted: took too long.'
      else highlightNote.value = 'Not highlighted: ' + result.error
    }
  } catch (e) {
    if (!current()) return
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (current()) loadingFile.value = false
  }
}

function backToTree() {
  nav() // discard a file load still in flight for the file just left
  selectedFile.value = ''
  blob.value = null
  highlighted.value = null
  highlightNote.value = ''
  clearSelection()
}

function backToRefs() {
  nav() // discard a tree or file load still in flight for the ref just left
  selectedRef.value = ''
  resolvedSha.value = ''
  dir.value = ''
  entries.value = []
}

/** ref, then each directory on the way to the current one, each a click back
 *  to that level. */
const crumbs = computed(() => {
  const parts = dir.value.split('/').filter(Boolean)
  const out: { name: string; path: string }[] = [{ name: selectedRef.value, path: '' }]
  let acc = ''
  for (const p of parts) {
    acc = acc ? `${acc}/${p}` : p
    out.push({ name: p, path: acc })
  }
  return out
})

const fileLines = computed(() => blob.value?.content?.split('\n') ?? [])
// One text node for the whole gutter rather than one element per line -- a
// div-per-line gutter put 60,000 extra DOM nodes on the page for a file at
// the size cap, measurably slowing layout for nothing a person ever looks
// at individually the way a line of code is.
const lineNumbers = computed(() =>
  Array.from({ length: fileLines.value.length }, (_, i) => i + 1).join('\n'),
)

// Surfaced in the page header (App.vue) rather than only in the tree pane's
// own crumb bar, which is hidden on a phone once a file is open -- the pane
// showing the file is the one place that lost track of which ref and path
// it belonged to.
const headerCrumb = computed(() => {
  if (!selectedRef.value) return ''
  const parts = crumbs.value.map((c) => c.name)
  if (selectedFile.value) parts.push(selectedFile.value.split('/').pop() ?? selectedFile.value)
  return parts.join(' / ')
})
watch(headerCrumb, (v) => emit('crumb', v), { immediate: true })

// ── explain ────────────────────────────────────────────────────────────
// "Explain this" while browsing -- see docs/design/code-explorer.md
// decisions 8-11. Polled for, not held open: an agent turn is tens of
// seconds, the same reasoning the approval-review "ask" flow already uses.

const explainOpen = ref(false)
const explainTitle = ref('')
const explainStatus = ref<'reading' | 'done' | 'error' | ''>('')
const explainAnswer = ref('')
const explainErrorMsg = ref('')
let explainPoll: number | undefined

function stopExplainPoll() {
  window.clearInterval(explainPoll)
  explainPoll = undefined
}

async function pollExplain(jobId: string) {
  try {
    const job = await api.repoExplainStatus(props.projectId, jobId)
    if (job.status === 'reading') return
    stopExplainPoll()
    explainStatus.value = job.status
    explainAnswer.value = job.answer ?? ''
    explainErrorMsg.value = job.error ?? ''
  } catch (e) {
    stopExplainPoll()
    explainStatus.value = 'error'
    explainErrorMsg.value = e instanceof Error ? e.message : String(e)
  }
}

async function runExplain(path: string, selection: string, title: string) {
  const ref = resolvedSha.value || selectedRef.value
  if (!ref || !path) return
  stopExplainPoll()
  explainTitle.value = title
  explainAnswer.value = ''
  explainErrorMsg.value = ''
  explainStatus.value = 'reading'
  explainOpen.value = true
  try {
    const { jobId } = await api.repoExplain(props.projectId, ref, path, selection)
    explainPoll = window.setInterval(() => pollExplain(jobId), 1500)
  } catch (e) {
    explainStatus.value = 'error'
    explainErrorMsg.value = e instanceof Error ? e.message : String(e)
  }
}

function explainFile() {
  if (selectedFile.value) runExplain(selectedFile.value, '', selectedFile.value)
}

function explainFolder(path: string) {
  runExplain(path, '', path)
}

function explainSelection() {
  if (selectedFile.value && selectionText.value) {
    runExplain(selectedFile.value, selectionText.value, `Selection in ${selectedFile.value}`)
  }
  clearSelection()
}

function closeExplain() {
  explainOpen.value = false
  stopExplainPoll()
}

onUnmounted(() => {
  stopExplainPoll()
  emit('crumb', '')
})

// A selection made inside the file content offers to explain just that,
// rather than the whole file -- a person reading a specific excerpt wants an
// answer about exactly what is highlighted. Not a per-line affordance the
// way a diff's gutter is: at a whole file's scale that would be a button per
// line of a document nobody asked to annotate.
const contentEl = ref<HTMLElement | null>(null)
const selectionText = ref('')
const selectionPos = ref<{ top: number; left: number } | null>(null)

function clearSelection() {
  selectionText.value = ''
  selectionPos.value = null
}

function onContentMouseUp() {
  const sel = window.getSelection()
  const text = sel?.toString().trim() ?? ''
  if (!text || !contentEl.value || sel!.rangeCount === 0 || !contentEl.value.contains(sel!.anchorNode)) {
    clearSelection()
    return
  }
  const rect = sel!.getRangeAt(0).getBoundingClientRect()
  selectionText.value = text
  selectionPos.value = { top: rect.bottom + 6, left: rect.left }
}

// ── shareable location ──────────────────────────────────────────────────
// The URL is where this belongs, per decision 4: a link opened later should
// show the same commit and the same file, not whatever "main" has since
// become. One-directional both ways rather than a live two-way binding --
// component state writes the query, the query is only ever read back once,
// on the way in -- so there is no loop between the two.

/** Read once, on the way in: a ref (name or sha) and a path (file or
 *  directory, told apart by asking) restored from a link, before anything
 *  the person does themselves overwrites it. */
async function restoreFromQuery() {
  const ref = route.query.ref
  const path = route.query.path
  if (typeof ref !== 'string' || !ref) return

  selectedRef.value = ref
  resolvedSha.value = ''
  await loadTree() // resolves ref at the repository root
  if (typeof path !== 'string' || !path || !resolvedSha.value) return

  // The link does not say whether path is a file or a directory: ask for it
  // as a file first, since sharing "look at this file" is the case worth
  // keeping quiet in the common path -- a directory link still resolves
  // correctly, just behind one failed probe.
  try {
    await api.repoFile(props.projectId, resolvedSha.value, path)
    openFile(path)
  } catch {
    openDir(path)
  }
}

/** Written on every navigation: the resolved sha once one exists (a moving
 *  branch name pins itself the moment it is actually opened), and whichever
 *  of a file or a directory is open. Replace, not push -- browsing a
 *  repository is not a sequence of pages to walk back through with the
 *  browser's own back button, which the tree/file back buttons already do
 *  within this view. */
function syncQuery() {
  const query: Record<string, string> = { ...(route.query as Record<string, string>) }
  if (selectedRef.value) query.ref = resolvedSha.value || selectedRef.value
  else delete query.ref
  const path = selectedFile.value || dir.value
  if (path) query.path = path
  else delete query.path
  router.replace({ query })
}
watch([selectedRef, resolvedSha, dir, selectedFile], syncQuery)

watch(
  () => props.projectId,
  async () => {
    nav() // discard anything still in flight for the project just left
    selectedRef.value = ''
    resolvedSha.value = ''
    dir.value = ''
    entries.value = []
    selectedFile.value = ''
    blob.value = null
    await loadRefs()
    await restoreFromQuery()
  },
  { immediate: true },
)
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col gap-3 sm:flex-row">
    <!-- Refs: branches and tags, independent of any task. -->
    <section
      :class="[
        'min-w-0 flex-col border sm:flex sm:w-48 sm:shrink-0',
        mobilePane === 'refs' ? 'flex' : 'hidden',
      ]"
    >
      <div class="hairline-b bg-background sticky top-0 px-2 py-1.5 text-xs font-semibold tracking-wide">
        Branches &amp; tags
      </div>
      <!-- A ref the list does not show: a commit sha, or a branch the picker
           does not bother filtering to. Same resolution as clicking one --
           an unreal ref comes back as an ordinary error below, not a crash. -->
      <form class="hairline-b flex items-center gap-1 p-1.5" @submit.prevent="goToManualRef">
        <Input
          v-model="manualRef"
          placeholder="Branch, tag or commit…"
          aria-label="Go to a branch, tag or commit"
          class="h-7 text-[11px]"
        />
      </form>
      <div class="min-h-0 flex-1 overflow-y-auto">
        <p v-if="loadingRefs" class="text-muted-foreground p-2 text-[11px]">Reading refs…</p>
        <p v-else-if="!refs.length" class="text-muted-foreground p-2 text-[11px]">
          No branches or tags.
        </p>
        <template v-else>
          <button
            v-for="b in branches"
            :key="'branch-' + b.name"
            type="button"
            :class="[
              'hover:bg-muted focus-visible:outline-ring flex w-full items-center gap-1.5 px-2 py-2.5 text-left text-xs sm:py-1.5 transition-colors focus-visible:outline-2',
              selectedRef === b.name && 'bg-primary/[0.08] font-medium',
            ]"
            @click="openRef(b.name)"
          >
            <GitBranch :size="12" class="text-muted-foreground shrink-0" aria-hidden="true" />
            <span class="truncate">{{ b.name }}</span>
          </button>
          <button
            v-for="t in tags"
            :key="'tag-' + t.name"
            type="button"
            :class="[
              'hover:bg-muted focus-visible:outline-ring flex w-full items-center gap-1.5 px-2 py-2.5 text-left text-xs sm:py-1.5 transition-colors focus-visible:outline-2',
              selectedRef === t.name && 'bg-primary/[0.08] font-medium',
            ]"
            @click="openRef(t.name)"
          >
            <Tag :size="12" class="text-muted-foreground shrink-0" aria-hidden="true" />
            <span class="truncate">{{ t.name }}</span>
          </button>
        </template>
      </div>
    </section>

    <!-- Tree: one directory's immediate children, fetched lazily per click. -->
    <section
      :class="[
        'min-w-0 flex-col border sm:flex sm:w-64 sm:shrink-0',
        mobilePane === 'tree' ? 'flex' : 'hidden',
      ]"
    >
      <div class="hairline-b bg-background sticky top-0 flex items-center gap-1 px-2 py-1.5">
        <button
          type="button"
          class="hover:bg-muted focus-visible:outline-ring grid size-6 shrink-0 place-items-center transition-colors focus-visible:outline-2 sm:hidden"
          title="Back to branches and tags"
          aria-label="Back to branches and tags"
          @click="backToRefs"
        >
          <ChevronLeft :size="14" aria-hidden="true" />
        </button>
        <div class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto text-xs font-semibold tracking-wide whitespace-nowrap">
          <template v-for="(c, i) in crumbs" :key="c.path">
            <span v-if="i > 0" class="text-muted-foreground" aria-hidden="true">/</span>
            <button
              type="button"
              class="hover:underline focus-visible:outline-ring shrink-0 focus-visible:outline-2"
              :disabled="c.path === dir"
              :class="c.path === dir && 'text-muted-foreground no-underline'"
              @click="openDir(c.path)"
            >
              {{ c.name }}
            </button>
          </template>
        </div>
      </div>
      <div class="min-h-0 flex-1 overflow-y-auto">
        <p v-if="loadingTree" class="text-muted-foreground p-2 text-[11px]">Reading directory…</p>
        <p v-else-if="!selectedRef" class="text-muted-foreground p-2 text-[11px]">
          Pick a branch or tag to browse it.
        </p>
        <p v-else-if="!entries.length" class="text-muted-foreground p-2 text-[11px]">Empty.</p>
        <template v-else>
          <button
            v-if="dir"
            type="button"
            class="hover:bg-muted focus-visible:outline-ring text-muted-foreground flex w-full items-center gap-1.5 px-2 py-2.5 text-left text-xs transition-colors focus-visible:outline-2 sm:py-1.5"
            @click="openParentDir"
          >
            <Folder :size="12" class="shrink-0" aria-hidden="true" />
            ..
          </button>
          <div v-for="e in entries" :key="e.path" class="flex items-stretch">
            <button
              type="button"
              :class="[
                'hover:bg-muted focus-visible:outline-ring flex min-w-0 flex-1 items-center gap-1.5 px-2 py-2.5 text-left text-xs transition-colors focus-visible:outline-2 sm:py-1.5',
                e.path === selectedFile && 'bg-primary/[0.08] font-medium',
              ]"
              @click="e.kind === 'tree' ? openDir(e.path) : openFile(e.path)"
            >
              <Folder v-if="e.kind === 'tree'" :size="12" class="text-muted-foreground shrink-0" aria-hidden="true" />
              <File v-else :size="12" class="text-muted-foreground shrink-0" aria-hidden="true" />
              <span class="min-w-0 flex-1 truncate">{{ e.name }}</span>
              <span
                v-if="e.kind === 'blob' && e.size !== undefined"
                class="tabular text-muted-foreground shrink-0 text-[10px]"
              >
                {{ e.size.toLocaleString() }}
              </span>
            </button>
            <!-- Explain this folder, without navigating into it -- decision 10:
                 a folder is guide-shaped exactly like a file, just naming a
                 directory instead of a path. -->
            <button
              v-if="e.kind === 'tree'"
              type="button"
              class="hover:bg-muted hover:text-foreground focus-visible:outline-ring text-muted-foreground grid size-9 shrink-0 place-items-center transition-colors focus-visible:outline-2 sm:size-7"
              title="Explain this folder"
              :aria-label="`Explain ${e.name}`"
              @click.stop="explainFolder(e.path)"
            >
              <MessageCircleQuestion :size="12" aria-hidden="true" />
            </button>
          </div>
        </template>
      </div>
    </section>

    <!-- File: one blob's exact content, highlighted when a grammar and the
         time budget both allow it, plain text otherwise. -->
    <section
      :class="[
        'min-w-0 flex-1 flex-col border sm:flex',
        mobilePane === 'file' ? 'flex' : 'hidden',
      ]"
    >
      <div class="hairline-b bg-background sticky top-0 flex items-center gap-1.5 px-2 py-1.5">
        <button
          type="button"
          class="hover:bg-muted focus-visible:outline-ring grid size-6 shrink-0 place-items-center transition-colors focus-visible:outline-2 sm:hidden"
          title="Back to the directory"
          aria-label="Back to the directory"
          @click="backToTree"
        >
          <ChevronLeft :size="14" aria-hidden="true" />
        </button>
        <span class="min-w-0 flex-1 truncate text-xs font-semibold tracking-wide">
          {{ selectedFile || 'Select a file' }}
        </span>
        <button
          v-if="selectedFile"
          type="button"
          class="hover:bg-muted hover:text-foreground focus-visible:outline-ring text-muted-foreground flex shrink-0 items-center gap-1 px-1.5 py-1 text-[11px] transition-colors focus-visible:outline-2"
          title="Explain this file"
          @click="explainFile"
        >
          <MessageCircleQuestion :size="12" aria-hidden="true" />
          <span class="hidden sm:inline">Explain</span>
        </button>
      </div>
      <div class="min-h-0 flex-1 overflow-auto">
        <p v-if="loadingFile" class="text-muted-foreground flex items-center gap-1.5 p-2 text-[11px]">
          <LoaderCircle :size="12" aria-hidden="true" class="spin" />
          Reading file…
        </p>
        <p v-else-if="!selectedFile" class="text-muted-foreground p-2 text-[11px]">
          Pick a branch or tag, then a file, to read it here.
        </p>
        <p v-else-if="blob?.binary" class="text-muted-foreground p-2 text-[11px]">
          Binary file, {{ blob.size.toLocaleString() }} bytes. Not shown.
        </p>
        <p v-else-if="blob?.tooLarge" class="text-muted-foreground p-2 text-[11px]">
          {{ blob.size.toLocaleString() }} bytes, too large to show here.
        </p>
        <div v-else-if="blob">
          <p v-if="highlightNote" class="text-muted-foreground border-b px-2 py-1 text-[10px]">
            {{ highlightNote }}
          </p>
          <div ref="contentEl" class="flex font-mono text-[11px] leading-snug" @mouseup="onContentMouseUp">
            <pre
              class="tabular text-muted-foreground shrink-0 select-none px-2 py-2 text-right font-mono text-[11px] leading-snug"
            >{{ lineNumbers }}</pre>
            <!-- Shiki's own output: it HTML-escapes the source text itself
                 before wrapping it in colour spans, so this is markup Shiki
                 built, never the repo's raw bytes rendered as markup -- the
                 v-html this app's rules warn against is the other thing. -->
            <div v-if="highlighted" class="min-w-0 flex-1 overflow-x-auto py-2 pr-3" v-html="highlighted" />
            <pre v-else class="min-w-0 flex-1 overflow-x-auto py-2 pr-3 whitespace-pre">{{ blob.content }}</pre>
          </div>
        </div>
      </div>
    </section>

    <p v-if="error" class="text-destructive absolute bottom-2 left-2 text-[11px]">{{ error }}</p>

    <!-- A selection inside the file offers to explain just that -- appears
         only once something is actually highlighted, positioned at it
         rather than fixed in a corner, so it reads as attached to the
         selection rather than as a persistent piece of chrome. -->
    <Button
      v-if="selectionPos"
      size="xs"
      class="fixed z-50 h-7 gap-1 px-2 text-[11px] shadow-md"
      :style="{ top: `${selectionPos.top}px`, left: `${selectionPos.left}px` }"
      @click="explainSelection"
    >
      <MessageCircleQuestion :size="12" aria-hidden="true" />
      Explain selection
    </Button>

    <Dialog :open="explainOpen" @update:open="(v) => !v && closeExplain()">
      <DialogContent variant="confirm" class="sm:max-w-md">
        <DialogHeader>
          <DialogTitle class="truncate">{{ explainTitle }}</DialogTitle>
          <DialogDescription class="text-[11px]">
            Explained by this project's own agent.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <p v-if="explainStatus === 'reading'" class="text-muted-foreground flex items-center gap-1.5 text-[11px]">
            <LoaderCircle :size="12" aria-hidden="true" class="spin" />
            Reading…
          </p>
          <p v-else-if="explainStatus === 'error'" class="text-destructive text-[11px]">
            {{ explainErrorMsg }}
          </p>
          <p v-else class="text-[11px] leading-relaxed whitespace-pre-wrap">{{ explainAnswer }}</p>
        </DialogBody>
      </DialogContent>
    </Dialog>
  </div>
</template>

<script setup lang="ts">
/**
 * Browse any branch, tag or commit of a project's repository.
 *
 * Independent of any task, approval or feature -- see
 * docs/design/code-explorer.md. No AI explain yet (phase 3), deliberately
 * out of scope here.
 */
import { computed, ref, watch } from 'vue'
import { api, type RepoBlob, type RepoRef, type TreeEntry } from '@/lib/api'
import { highlight } from '@/lib/highlight'
import { latest } from '@/lib/latest'
import { ChevronLeft, File, Folder, GitBranch, LoaderCircle, Tag } from '@lucide/vue'

const props = defineProps<{ projectId: string }>()

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
// Opening one file and then another leaves the first file's highlight still
// running; without this it can land after the second file's plain text is
// already showing and repaint it as the wrong file, coloured.
const newestFile = latest()

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
  try {
    refs.value = await api.repoRefs(props.projectId)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loadingRefs.value = false
  }
}

async function loadTree() {
  if (!selectedRef.value) return
  loadingTree.value = true
  error.value = ''
  try {
    const res = await api.repoTree(props.projectId, selectedRef.value, dir.value)
    resolvedSha.value = res.resolvedSha
    entries.value = res.entries
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    entries.value = []
  } finally {
    loadingTree.value = false
  }
}

function openRef(name: string) {
  selectedRef.value = name
  dir.value = ''
  selectedFile.value = ''
  blob.value = null
  loadTree()
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
  const current = newestFile()
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
  selectedFile.value = ''
  blob.value = null
  highlighted.value = null
  highlightNote.value = ''
}

function backToRefs() {
  selectedRef.value = ''
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

watch(
  () => props.projectId,
  () => {
    selectedRef.value = ''
    dir.value = ''
    entries.value = []
    selectedFile.value = ''
    blob.value = null
    loadRefs()
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
              'hover:bg-muted focus-visible:outline-ring flex w-full items-center gap-1.5 px-2 py-1.5 text-left text-xs transition-colors focus-visible:outline-2',
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
              'hover:bg-muted focus-visible:outline-ring flex w-full items-center gap-1.5 px-2 py-1.5 text-left text-xs transition-colors focus-visible:outline-2',
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
            class="hover:bg-muted focus-visible:outline-ring flex w-full items-center gap-1.5 px-2 py-1.5 text-left text-xs text-muted-foreground transition-colors focus-visible:outline-2"
            @click="openParentDir"
          >
            <Folder :size="12" class="shrink-0" aria-hidden="true" />
            ..
          </button>
          <button
            v-for="e in entries"
            :key="e.path"
            type="button"
            :class="[
              'hover:bg-muted focus-visible:outline-ring flex w-full items-center gap-1.5 px-2 py-1.5 text-left text-xs transition-colors focus-visible:outline-2',
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
          <div class="flex font-mono text-[11px] leading-snug">
            <div class="tabular text-muted-foreground shrink-0 select-none px-2 py-2 text-right">
              <div v-for="(_, i) in fileLines" :key="i">{{ i + 1 }}</div>
            </div>
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
  </div>
</template>

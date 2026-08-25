<script setup lang="ts">
import { ref, watch } from 'vue'
import type { Attention } from '@/lib/api'
import { ChevronRight } from '@lucide/vue'
import { api, type ChangedFile } from '@/lib/api'
import { renderMarkdown } from '@/lib/markdown'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

const props = defineProps<{ attention: Attention | null; compact?: boolean }>()
const emit = defineEmits<{
  approve: [id: string]
  reject: [id: string, note: string]
  answer: [id: string, answer: string]
}>()

const notesOpen = ref<Record<string, boolean>>({})
const diffs = ref<Record<string, { open: boolean; files: ChangedFile[]; error?: string }>>({})

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
watch(
  () => props.attention?.approvals?.map((a) => a.id).join(',') ?? '',
  () => {
    for (const a of props.attention?.approvals ?? []) {
      if (a.commit && !diffs.value[a.id]) void loadFiles(a.id)
    }
  },
  { immediate: true },
)

async function loadFiles(id: string) {
  diffs.value = { ...diffs.value, [id]: { open: true, files: [] } }
  try {
    const r = await api.approvalDiff(id)
    diffs.value = { ...diffs.value, [id]: { open: true, files: r.files ?? [] } }
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
  <div v-if="props.attention" class="flex flex-col gap-3">
    <!-- Nothing waiting should feel like calm, not like a broken panel. -->
    <div
      v-if="empty(props.attention)"
      class="text-muted-foreground/70 flex flex-col items-center gap-1 py-12 text-center"
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
          class="md text-xs leading-relaxed"
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

      <!-- And what it actually wrote. Deciding from a description of a change
           rather than the change is approving blind, and for a planner's spec
           the committed file *is* the deliverable. Loaded on demand: most
           approvals are read, not all are expanded, and a diff is far larger
           than anything else here. -->
      <div v-if="a.commit" class="mb-2.5">
        <button
          v-if="diffs[a.id]?.files.length"
          type="button"
          class="text-muted-foreground hover:text-foreground mb-1.5 flex items-center gap-1 text-[11px]"
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

        <div v-if="diffs[a.id]?.open" class="mt-2">
          <p v-if="diffs[a.id]?.error" class="text-destructive text-[11px]">
            {{ diffs[a.id]?.error }}
          </p>
          <p v-else-if="!diffs[a.id]?.files.length" class="text-muted-foreground text-[11px]">
            Loading…
          </p>

          <div
            v-for="f in diffs[a.id]?.files ?? []"
            :key="f.path"
            class="mb-3 border last:mb-0"
          >
            <div class="hairline-b flex items-center gap-2 px-2 py-1">
              <Badge variant="outline">{{ f.status }}</Badge>
              <code class="min-w-0 truncate text-[11px]">{{ f.path }}</code>
            </div>

            <!-- A document, rendered. This is the thing being approved; showing
                 it as a diff makes the reader reconstruct it line by line. -->
            <div
              v-if="isDoc(f)"
              class="md max-h-[26rem] overflow-y-auto px-3 py-2 text-xs leading-relaxed"
              v-html="renderMarkdown(f.content ?? '')"
            />
            <pre
              v-else
              class="bg-muted max-h-80 overflow-auto p-2 font-mono text-[10px] leading-relaxed"
            >{{ f.diff || '(no diff)' }}</pre>
          </div>
        </div>
      </div>

      <div class="flex flex-wrap gap-2">
        <Input
          v-model="notes[a.id]"
          placeholder="reason, if rejecting"
          class="min-w-0 flex-1"
        />
        <Button size="sm" @click="emit('approve', a.id)">Approve</Button>
        <Button size="sm" variant="destructive" @click="emit('reject', a.id, notes[a.id] ?? '')">
          Reject
        </Button>
      </div>
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
      <p class="mb-2.5 text-xs leading-relaxed break-words">{{ c.question }}</p>
      <div class="flex gap-2">
        <Input
          v-model="answers[c.id]"
          placeholder="your answer"
          class="min-w-0 flex-1"
          @keyup.enter="emit('answer', c.id, answers[c.id] ?? '')"
        />
        <Button size="sm" @click="emit('answer', c.id, answers[c.id] ?? '')">Answer</Button>
      </div>
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

<script setup lang="ts">
/**
 * What a task produced, where the person deciding about it is looking.
 *
 * A finished pipeline leaves a commit, which answers the question for a
 * library and answers nothing for anything with a screen: the next thing
 * anybody asks is what it looks like, and today that means checking out the
 * branch and running it. These are the answers an agent left behind -- a
 * screenshot, a report, or a dev server it started and registered.
 */
import { ref, watch } from 'vue'
import { Box, ExternalLink, FileText, Image as ImageIcon, Monitor, Pin } from '@lucide/vue'
import { api, artifactBytes, type Artifact } from '@/lib/api'
import { Button } from '@/components/ui/button'

const props = defineProps<{
  taskId: string | undefined
  /** Compact drops the service viewer, for the places that are already dense. */
  compact?: boolean
}>()

const items = ref<Artifact[]>([])
const error = ref('')

/** Which service is open in the viewer. One at a time: each is a whole
 *  application, and two of them side by side in a card is nobody's idea of a
 *  review. */
const open = ref<string | null>(null)

async function load(taskId: string | undefined) {
  items.value = []
  open.value = null
  if (!taskId) return
  try {
    items.value = await api.taskArtifacts(taskId)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

watch(() => props.taskId, load, { immediate: true })

async function pin(a: Artifact) {
  try {
    const updated = await api.pinArtifact(a.id, !a.pinned)
    items.value = items.value.map((it) => (it.id === a.id ? updated : it))
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

/** Bytes, in the unit a person would say. */
function size(n: number | undefined): string {
  if (!n) return ''
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

const icon = (a: Artifact) =>
  a.kind === 'service' ? Monitor : a.kind === 'image' ? ImageIcon : FileText

defineExpose({ load })
</script>

<template>
  <div v-if="items.length" class="flex flex-col gap-2">
    <p class="text-muted-foreground flex items-center gap-1.5 text-[11px]">
      <Box :size="12" aria-hidden="true" class="shrink-0" />
      what this produced
    </p>

    <div v-for="a in items" :key="a.id" class="border">
      <div class="flex items-center gap-2 px-2 py-1.5">
        <component :is="icon(a)" :size="12" aria-hidden="true" class="text-primary shrink-0" />
        <span class="min-w-0 flex-1 truncate text-[11px] font-medium" :title="a.name || a.label">
          {{ a.label || a.name }}
        </span>
        <span class="text-muted-foreground shrink-0 text-[10px] tabular-nums">
          {{ a.role }}<template v-if="a.bytes"> · {{ size(a.bytes) }}</template>
        </span>

        <!-- A stopped service is said rather than linked: the process is gone
             and its port belongs to whatever took it next. -->
        <span
          v-if="a.kind === 'service' && !a.url"
          class="text-muted-foreground shrink-0 text-[10px]"
        >
          stopped
        </span>
        <Button
          v-else-if="a.kind === 'service'"
          size="xs"
          variant="ghost"
          class="h-6 shrink-0 gap-1 px-1.5 text-[10px]"
          @click="open = open === a.id ? null : a.id"
        >
          {{ open === a.id ? 'hide' : 'open' }}
        </Button>
        <a
          v-if="a.kind === 'service' && a.url"
          :href="a.url"
          target="_blank"
          rel="noopener noreferrer"
          class="text-muted-foreground hover:text-foreground focus-visible:outline-ring shrink-0 focus-visible:outline-2"
          title="Open in a tab of its own"
        >
          <ExternalLink :size="12" aria-hidden="true" />
        </a>
        <a
          v-else-if="a.kind !== 'service'"
          :href="artifactBytes(a.id)"
          target="_blank"
          rel="noopener noreferrer"
          class="text-muted-foreground hover:text-foreground focus-visible:outline-ring shrink-0 text-[10px] underline-offset-2 hover:underline focus-visible:outline-2"
        >
          open
        </a>

        <!-- Pinned keeps the bytes after the transcript ages out. -->
        <button
          type="button"
          class="focus-visible:outline-ring shrink-0 focus-visible:outline-2"
          :class="a.pinned ? 'text-[var(--primary)]' : 'text-muted-foreground hover:text-foreground'"
          :title="a.pinned ? 'Kept when this task ages out' : 'Keep this after the task ages out'"
          :aria-label="a.pinned ? 'Stop keeping this' : 'Keep this'"
          @click="pin(a)"
        >
          <Pin :size="12" aria-hidden="true" />
        </button>
      </div>

      <!-- An image is the one thing worth showing without being asked. -->
      <img
        v-if="a.kind === 'image'"
        :src="artifactBytes(a.id)"
        :alt="a.label || a.name || 'screenshot'"
        class="hairline-t max-h-80 w-full bg-black/20 object-contain"
        loading="lazy"
      />

      <!-- A service, in a frame of its own.
           The src is a different origin from the cockpit (see §13.4), which is
           what keeps an agent's dev server away from the command API. The
           sandbox is the second layer: scripts and forms run, because a dev
           server that cannot run scripts is not worth looking at, and
           allow-same-origin grants it only its own origin, which holds nothing
           but itself. Top-level navigation is not granted, so the page cannot
           replace the cockpit around it. -->
      <iframe
        v-if="!compact && a.kind === 'service' && a.url && open === a.id"
        :src="a.url"
        :title="a.label || 'service'"
        sandbox="allow-scripts allow-forms allow-same-origin allow-popups"
        referrerpolicy="no-referrer"
        class="hairline-t h-96 w-full bg-white"
      />
    </div>

    <p v-if="error" class="text-destructive text-[11px]">{{ error }}</p>
  </div>
</template>

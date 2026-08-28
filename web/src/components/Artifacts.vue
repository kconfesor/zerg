<script setup lang="ts">
/**
 * What a task produced, where the person deciding about it is looking.
 *
 * A finished pipeline leaves a commit, which answers the question for a
 * library and answers nothing for anything with a screen: the next thing
 * anybody asks is what it looks like, and today that means checking out the
 * branch and running it. These are the answers an agent left behind -- a
 * screenshot, a report, or a dev server it started and registered -- and a
 * running server is a link, opened in a tab like any other local server.
 */
import { ref, watch } from 'vue'
import { Box, ExternalLink, FileText, Image as ImageIcon, Monitor, Pin } from '@lucide/vue'
import { api, artifactBytes, type Artifact } from '@/lib/api'

const props = defineProps<{
  taskId: string | undefined
}>()

const items = ref<Artifact[]>([])
const error = ref('')

async function load(taskId: string | undefined) {
  items.value = []
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
    <p v-if="items.length" class="text-muted-foreground flex items-center gap-1.5 text-[11px]">
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
        <!-- A running service is a link and nothing more. It was a frame
             embedded here, which is the wrong shape for it: what an agent
             started is a whole application, and half a card is not where
             anybody clicks around one. It opens in a tab, like every other
             local server a person runs.

             The href is the proxy's origin, not localhost:port. A dev server
             binds 127.0.0.1, so the direct address is reachable only from the
             machine the daemon is on -- and the reason to look at a preview on
             a phone is exactly the case that would break. -->
        <a
          v-else-if="a.kind === 'service' && a.url"
          :href="a.url"
          target="_blank"
          rel="noopener noreferrer"
          class="text-primary focus-visible:outline-ring shrink-0 text-[10px] underline-offset-2 hover:underline focus-visible:outline-2"
        >
          open
          <ExternalLink :size="10" aria-hidden="true" class="ml-0.5 inline-block align-[-1px]" />
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

    </div>

    <p v-if="error" class="text-destructive text-[11px]">{{ error }}</p>
  </div>
</template>

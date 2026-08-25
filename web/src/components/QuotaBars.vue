<script setup lang="ts">
/**
 * What a subscription has left, per rolling window.
 *
 * A bar rather than a number because the question is never "what is the
 * figure" — it is "is there room for this run", and a length answers that at a
 * glance where 68% has to be read and compared.
 *
 * Only the tightest window gets colour. Two amber bars would say the same
 * thing twice, and the one that stops work is the one nearest full.
 */
import { computed } from 'vue'
import type { QuotaReport } from '@/lib/api'

const props = defineProps<{ quota: QuotaReport; compact?: boolean }>()

/** The window nearest its limit — the one that will actually stop work. */
const tightest = computed(() =>
  props.quota.windows.reduce((a, b) => (b.used > a.used ? b : a), props.quota.windows[0]),
)

function tone(used: number): string {
  if (used >= 0.9) return 'bg-destructive'
  if (used >= 0.75) return 'bg-[var(--status-warning)]'
  return 'bg-[var(--primary)]'
}

/** "in 2h 10m", "in 3d" — how long until this window rolls over. */
function resetsIn(iso?: string): string {
  if (!iso) return ''
  const mins = Math.round((new Date(iso).getTime() - Date.now()) / 60000)
  if (!Number.isFinite(mins) || mins <= 0) return 'any moment'
  if (mins < 60) return `${mins}m`
  const hours = Math.floor(mins / 60)
  if (hours < 24) return `${hours}h ${mins % 60}m`
  return `${Math.floor(hours / 24)}d ${hours % 24}h`
}
</script>

<template>
  <div class="flex flex-col gap-1">
    <div v-for="w in quota.windows" :key="w.label" class="flex items-center gap-1.5">
      <span class="text-muted-foreground w-5 shrink-0 text-[10px]">{{ w.label }}</span>
      <!-- The track is always full width, so a short bar reads as room left
           rather than as a bar that failed to draw. -->
      <span class="bg-muted h-1 min-w-8 flex-1 overflow-hidden rounded-full">
        <span
          :class="['block h-full rounded-full transition-[width]',
                   w === tightest ? tone(w.used) : 'bg-muted-foreground/40']"
          :style="{ width: `${Math.min(100, Math.max(2, w.used * 100))}%` }"
        />
      </span>
      <span class="tabular text-muted-foreground w-8 shrink-0 text-right text-[10px]">
        {{ Math.round(w.used * 100) }}%
      </span>
      <span
        v-if="!compact && w.resetsAt"
        class="text-muted-foreground/70 w-16 shrink-0 text-[10px]"
      >
        {{ resetsIn(w.resetsAt) }}
      </span>
    </div>
  </div>
</template>

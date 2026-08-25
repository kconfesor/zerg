<script setup lang="ts">
/**
 * What actually happened to a task.
 *
 * A lane called Done says a task finished and nothing else. Everything here was
 * already recorded — each role writes a note when it hands work on, every
 * handoff points at a commit, and usage is totalled per task — it simply had
 * nowhere to be read.
 */
import { ref, watch } from 'vue'
import { api, type Task, type TaskDetail } from '@/lib/api'
import { renderMarkdown } from '@/lib/markdown'
import { duration } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'

const props = defineProps<{ task: Task | null }>()
const emit = defineEmits<{ close: [] }>()

const detail = ref<TaskDetail | null>(null)
const failed = ref('')

watch(
  () => props.task,
  async (t) => {
    detail.value = null
    failed.value = ''
    if (!t) return
    try {
      detail.value = await api.taskDetail(t.id)
    } catch (e) {
      failed.value = e instanceof Error ? e.message : String(e)
    }
  },
  { immediate: true },
)

function money(n: number): string {
  return n >= 1 ? `$${n.toFixed(2)}` : `$${n.toFixed(4)}`
}
const num = new Intl.NumberFormat()

function tokensOf(u: TaskDetail['usage']): number {
  return u.inputTokens + u.cacheReadTokens + u.cacheWriteTokens + u.outputTokens
}
</script>

<template>
  <Dialog :open="!!task" @update:open="(v) => !v && emit('close')">
    <DialogContent class="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
      <DialogHeader>
        <DialogTitle>{{ task?.name }}</DialogTitle>
        <DialogDescription class="flex flex-wrap items-center gap-2 text-[11px]">
          <Badge variant="outline">{{ task?.state }}</Badge>
          <Badge v-if="(task?.reworkCount ?? 0) > 0" variant="secondary">
            ↩ {{ task?.reworkCount }} rework
          </Badge>
          <span v-if="task?.activeMs" class="text-muted-foreground">
            {{ duration(task.activeMs) }} of agent time
          </span>
          <span v-if="detail?.usage.turns" class="text-muted-foreground">
            · {{ detail.usage.turns }} turns · {{ num.format(tokensOf(detail.usage)) }} tokens ·
            {{ money(detail.usage.costUsd) }}
            <span v-if="detail.usage.subscriptionTurns === detail.usage.turns">(plan)</span>
          </span>
        </DialogDescription>
      </DialogHeader>

      <p v-if="failed" class="text-destructive text-xs">{{ failed }}</p>
      <p v-else-if="!detail" class="text-muted-foreground text-xs">Reading the history…</p>

      <!-- One step per handoff, in order. The note each role wrote is the
           substance; the commit subject says what landed. -->
      <ol v-else class="flex flex-col gap-3">
        <li v-for="(h, i) in detail.history" :key="i" class="hairline-b pb-3 last:border-0">
          <div class="mb-1 flex flex-wrap items-center gap-1.5 text-[11px]">
            <span class="font-semibold">{{ h.from }}</span>
            <span class="text-muted-foreground">→</span>
            <span class="font-semibold">{{ h.final ? 'merged' : h.to || '—' }}</span>
            <code v-if="h.commit" class="text-muted-foreground">{{ h.commit.slice(0, 8) }}</code>
            <span v-if="h.subject" class="text-muted-foreground truncate">{{ h.subject }}</span>
          </div>
          <div v-if="h.body" class="md text-xs leading-relaxed" v-html="renderMarkdown(h.body)" />
          <p v-else class="text-muted-foreground text-[11px] italic">
            No note — this handoff predates notes being required.
          </p>
        </li>
      </ol>
    </DialogContent>
  </Dialog>
</template>

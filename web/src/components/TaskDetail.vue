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
import { latest } from '@/lib/latest'
import { renderMarkdown } from '@/lib/markdown'
import { duration } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import TaskFlow from '@/components/TaskFlow.vue'
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

const props = defineProps<{
  task: Task | null
  /** The team's roles in pipeline order, so the diagram can show the columns a
   *  card has not reached yet as well as the ones it has. */
  roles?: string[]
  baseBranch?: string
}>()
const emit = defineEmits<{ close: [] }>()

const detail = ref<TaskDetail | null>(null)
const failed = ref('')

// Opening one card and then another leaves two requests in flight, and the
// first can answer last — putting task A's history and spend under task B's
// name, which reads as a real record of the wrong card.
const newest = latest()

watch(
  () => props.task,
  async (t) => {
    const current = newest()
    detail.value = null
    failed.value = ''
    if (!t) return
    try {
      const d = await api.taskDetail(t.id)
      if (!current()) return
      detail.value = d
    } catch (e) {
      if (!current()) return
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
    <!-- gap-0 and p-0 hand every edge to the header and the body below, which
         is what keeps the padding still while the content moves. pr-12 on the
         header reserves the corner the close button sits in, so a long task
         name truncates before it reaches it rather than running underneath. -->
    <DialogContent class="gap-0 overflow-hidden p-0 sm:max-w-3xl">
      <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
        <DialogTitle class="truncate">{{ task?.name }}</DialogTitle>
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

      <DialogBody>
        <p v-if="failed" class="text-destructive text-xs">{{ failed }}</p>
        <p v-else-if="!detail" class="text-muted-foreground text-xs">Reading the history…</p>

        <!-- One step per handoff, drawn in the order they happened. The note
             each role wrote is the substance; the diagram is what says which
             way the work went. -->
        <TaskFlow
          v-else
          :steps="detail.history"
          :roles="roles ?? []"
          :base-branch="baseBranch"
          :current="task?.lane"
        >
          <template #note="{ step }">
            <div
              v-if="step.body"
              class="md mt-1 text-xs leading-relaxed"
              v-html="renderMarkdown(step.body)"
            />
            <p v-else class="text-muted-foreground text-[11px] italic">
              No note — this handoff predates notes being required.
            </p>
          </template>
        </TaskFlow>
      </DialogBody>
    </DialogContent>
  </Dialog>
</template>

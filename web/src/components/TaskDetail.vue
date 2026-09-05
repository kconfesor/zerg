<script setup lang="ts">
/**
 * What actually happened to a task.
 *
 * A lane called Done says a task finished and nothing else. Everything here was
 * already recorded — each role writes a note when it hands work on, every
 * handoff points at a commit, and usage is totalled per task — it simply had
 * nowhere to be read.
 */
import { computed, ref, watch } from 'vue'
import { api, type Project, type Task, type TaskDetail } from '@/lib/api'
import { latest } from '@/lib/latest'
import { renderMarkdown } from '@/lib/markdown'
import { duration, taskState } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import TaskFlow from '@/components/TaskFlow.vue'
import Artifacts from '@/components/Artifacts.vue'
import RunPanel from '@/components/RunPanel.vue'
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
  /** Read for where finished work lands; see lib/utils landing(). */
  project?: Project | null
  /** Roles this card was told to skip, by name. Resolved by the caller, which
   *  is the one holding the team: the card stores template ids so that
   *  renaming a role cannot silently un-skip it. */
  skipped?: string[]
  /** Grouping rows this card can belong to. */
  features?: Task[]
}>()
const emit = defineEmits<{ close: []; updated: [task: Task] }>()

const detail = ref<TaskDetail | null>(null)
const failed = ref('')

/** The artifact list, so a preview that starts can make it look again. */
const artifacts = ref<{ load: (taskId: string | undefined) => void } | null>(null)

/**
 * The commit this card left behind: the last handoff that carried one.
 *
 * The last rather than the first, because that is the state the work ended in
 * and the one worth looking at.
 */
const landed = computed(() =>
  [...(detail.value?.history ?? [])].reverse().find((s) => s.commit)?.commit,
)

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

async function setSupervised(on: boolean) {
  const t = props.task
  if (!t) return
  failed.value = ''
  try {
    emit('updated', await api.setTaskSupervised(t.id, on))
  } catch (e) {
    failed.value = e instanceof Error ? e.message : String(e)
  }
}

async function setParent(parentId: string) {
  const t = props.task
  if (!t) return
  failed.value = ''
  try {
    emit('updated', await api.setTaskParent(t.id, parentId === 'none' ? null : parentId))
  } catch (e) {
    failed.value = e instanceof Error ? e.message : String(e)
  }
}

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
    <!-- Wider than the dialogs that ask a question, and matching the two that
         also carry a transcript: this one holds a diagram whose columns are one
         per role, the note each role wrote, and a step's events. At 3xl a team
         of five put its lifelines on top of each other and every transcript
         line truncated. -->
    <DialogContent class="min-w-0 gap-0 overflow-hidden p-0 sm:max-w-5xl">
      <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
        <DialogTitle class="truncate">{{ task?.name }}</DialogTitle>
        <DialogDescription class="flex flex-wrap items-center gap-2 text-[11px]">
          <Badge :variant="task?.stoppedAt ? 'secondary' : 'outline'">
            {{ task ? taskState(task) : '' }}
          </Badge>
          <span v-if="task?.stoppedAt" class="text-muted-foreground">
            parked by a person, not rejected by a role
          </span>
          <Badge v-if="(task?.reworkCount ?? 0) > 0" variant="secondary">
            ↩ {{ task?.reworkCount }} rework
          </Badge>
          <!-- Why this card's route is not the pipeline. Without it the
               diagram below reads as a card that lost a role somewhere. -->
          <Badge v-if="skipped?.length" variant="secondary">
            skipped {{ skipped.join(', ') }}
          </Badge>
          <label
            v-if="task && task.state !== 'done' && task.state !== 'rejected'"
            class="text-muted-foreground flex items-center gap-1.5"
          >
            <Switch
              :model-value="!!task.supervised"
              aria-label="Architect supervises this card"
              class="scale-90"
              @update:model-value="setSupervised"
            />
            Architect supervises
          </label>
          <Badge v-else-if="task?.supervised" variant="secondary">
            architect supervised
          </Badge>
          <label
            v-if="task && features?.length && task.kind !== 'feature'"
            class="text-muted-foreground flex items-center gap-1.5"
          >
            Part of
            <Select
              :model-value="task.parentId || 'none'"
              @update:model-value="(v) => typeof v === 'string' && setParent(v)"
            >
              <SelectTrigger size="sm" class="h-6 w-36 text-[11px]">
                <SelectValue placeholder="None" />
              </SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  <SelectItem value="none">None</SelectItem>
                  <SelectItem v-for="f in features" :key="f.id" :value="f.id">
                    {{ f.name }}
                  </SelectItem>
                </SelectGroup>
              </SelectContent>
            </Select>
          </label>
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
        <!-- Running what this card produced, after the fact.
             "What did this actually look like" is a question asked of a
             finished card at least as often as of one at a gate, and the
             commit is right here in the trail. -->
        <RunPanel
          :project-id="project?.id"
          :commit="landed"
          :task-id="task?.id"
          class="mb-4"
          @served="artifacts?.load(task?.id)"
        />

        <!-- What the work produced, above the account of how it happened: on a
             finished card this is the answer to the question people actually
             open it with. -->
        <Artifacts ref="artifacts" :task-id="task?.id" class="mb-4" />

        <p v-if="failed" class="text-destructive text-xs">{{ failed }}</p>
        <p v-else-if="!detail" class="text-muted-foreground text-xs">Reading the history…</p>

        <!-- One step per handoff, drawn in the order they happened. The note
             each role wrote is the substance; the diagram is what says which
             way the work went. -->
        <TaskFlow
          v-else
          :task-id="task?.id"
          :steps="detail.history"
          :roles="roles ?? []"
          :project="project"
          :outcome="task?.outcome"
          :outcome-ref="task?.outcomeRef"
          :current="task?.lane"
          :skipped="skipped"
        >
          <template #note="{ step }">
            <div
              v-if="step.body"
              class="md mt-1 text-xs leading-relaxed"
              v-html="renderMarkdown(step.body)"
            />
            <p v-else class="text-muted-foreground text-[11px] italic">
              No note. This handoff predates notes being required.
            </p>
          </template>
        </TaskFlow>
      </DialogBody>
    </DialogContent>
  </Dialog>
</template>

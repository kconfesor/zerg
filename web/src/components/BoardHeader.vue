<script setup lang="ts">
/**
 * What is true about this board, in one line.
 *
 * These facts arrived one at a time — the repository path, then the branch,
 * then spend, then a hidden count — and each was put wherever there was room.
 * The result read as leftovers rather than a summary: a subtitle, a meta row
 * and a button in the corner, none of them related to the others.
 *
 * One strip of labelled figures instead. Every one answers a question you would
 * otherwise open something to ask: how much is queued, how much is put away,
 * what the checkouts cost.
 *
 * Spend is deliberately not among them any more. It is the one figure that
 * matters whatever you are looking at, so it moved to the bar that is on every
 * screen; here it was visible only while already reading the board.
 */
import { computed } from 'vue'
import { FolderGit2, GitBranch, HardDrive } from '@lucide/vue'
import type { Project, Task, Workspace } from '@/lib/api'

const props = defineProps<{
  project: Project
  tasks: Task[]
  workspace: Workspace | null
}>()

const stats = computed(() => {
  const total = props.tasks.length
  const working = props.tasks.filter((t) => t.state === 'working').length
  const hidden = props.tasks.filter((t) => t.hidden).length
  const done = props.tasks.filter((t) => t.state === 'done').length
  return { total, working, hidden, done, open: total - done }
})

/** Bytes as something a person reads, not a number they convert. */
function size(bytes: number): string {
  if (bytes >= 1 << 30) return `${(bytes / (1 << 30)).toFixed(1)} GB`
  if (bytes >= 1 << 20) return `${Math.round(bytes / (1 << 20))} MB`
  return `${Math.round(bytes / 1024)} KB`
}
</script>

<template>
  <div class="flex flex-col gap-3">
    <!-- Identity: which repository, on which branch. -->
    <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
      <h1 class="text-[17px] leading-none font-semibold tracking-tight">Board</h1>
      <span class="text-muted-foreground/40">/</span>
      <span
        class="text-muted-foreground inline-flex min-w-0 items-center gap-1.5 text-[11px]"
        :title="project.path"
      >
        <FolderGit2 :size="12" class="shrink-0" aria-hidden="true" />
        <span class="truncate font-mono">{{ project.path }}</span>
      </span>
      <span class="text-muted-foreground inline-flex items-center gap-1.5 text-[11px]">
        <GitBranch :size="12" aria-hidden="true" />
        {{ project.baseBranch }}
      </span>
      <div class="ml-auto flex items-center gap-2"><slot name="actions" /></div>
    </div>

    <!-- The figures. Value above label, so the numbers line up and the row
         scans as a row rather than as sentences of different lengths. -->
    <dl class="hairline-b flex flex-wrap items-end gap-x-6 gap-y-2 pb-3">
      <div>
        <dd class="tabular text-sm leading-none font-semibold">{{ stats.open }}</dd>
        <dt class="text-muted-foreground mt-1 text-[10px] tracking-wide uppercase">open</dt>
      </div>
      <div>
        <dd
          class="tabular text-sm leading-none font-semibold"
          :class="stats.working ? 'text-[var(--primary)]' : 'text-muted-foreground'"
        >
          {{ stats.working }}
        </dd>
        <dt class="text-muted-foreground mt-1 text-[10px] tracking-wide uppercase">working</dt>
      </div>
      <div>
        <dd class="tabular text-sm leading-none font-semibold">{{ stats.done }}</dd>
        <dt class="text-muted-foreground mt-1 text-[10px] tracking-wide uppercase">done</dt>
      </div>
      <!-- Only when there are any: a zero here is a control that does nothing
           dressed as information. -->
      <div v-if="stats.hidden">
        <dd class="tabular text-muted-foreground text-sm leading-none font-semibold">
          {{ stats.hidden }}
        </dd>
        <dt class="text-muted-foreground mt-1 text-[10px] tracking-wide uppercase">hidden</dt>
      </div>

      <div v-if="workspace?.worktrees?.length" class="border-l pl-6">
        <dd
          class="tabular flex items-baseline gap-1.5 text-sm leading-none font-semibold"
          :title="workspace.worktrees.map((w) => `${w.role}: ${size(w.bytes)}`).join('\n')"
        >
          {{ workspace.worktrees.length }}
          <HardDrive :size="12" class="text-muted-foreground self-center" aria-hidden="true" />
          <span class="text-muted-foreground text-xs font-normal">{{ size(workspace.bytes) }}</span>
        </dd>
        <dt class="text-muted-foreground mt-1 text-[10px] tracking-wide uppercase">worktrees</dt>
      </div>
    </dl>
  </div>
</template>

<script setup lang="ts">
/**
 * What is true about this board, in the same shape as every other view.
 *
 * These facts arrived one at a time — the repository path, then the branch,
 * then a hidden count, then what the checkouts cost — and each was put wherever
 * there was room. The answer to that was a strip of figures with the value
 * above an uppercase label, which fixed the disorder and introduced a worse
 * problem: it is a metrics widget, and nothing else in the cockpit is one. Every
 * other screen opens with a title, a sentence, and a row of small facts, so the
 * board was the one page that looked like it came from another product.
 *
 * The facts have not changed. They are written the way this app writes figures
 * everywhere else — number, then a lowercase word, separated by middots, the
 * way a task's turns and tokens and cost are written — and they sit in the meta
 * row that every other header already has.
 *
 * Spend is deliberately not among them. It is the one figure that matters
 * whatever you are looking at, so it lives in the bar that is on every screen;
 * here it was visible only while already reading the board.
 */
import { computed } from 'vue'
import { FolderGit2, GitBranch, HardDrive } from '@lucide/vue'
import PageHeader from '@/components/layout/PageHeader.vue'
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
  <PageHeader title="Board" subtitle="One column per role. A card moves right as each role finishes with it.">
    <template #meta>
      <!-- Which repository, and which branch the work lands on. The path is
           desktop-only: it is the longest thing in the row and answers a
           question you ask once, which is the same call Chat's header makes. -->
      <span
        class="text-muted-foreground hidden min-w-0 items-center gap-1.5 font-mono text-[11px] sm:flex"
        :title="project.path"
      >
        <FolderGit2 :size="11" class="shrink-0" aria-hidden="true" />
        <span class="truncate">{{ project.path }}</span>
      </span>
      <span class="text-muted-foreground flex items-center gap-1.5 font-mono text-[11px]">
        <GitBranch :size="11" class="shrink-0" aria-hidden="true" />
        {{ project.baseBranch }}
      </span>

      <!-- The counts, as one fact rather than four tiles. Middot-separated
           figures with the word after the number is how a task writes its
           turns, its tokens and its cost, and this is the same kind of
           sentence about a different subject.

           Working takes the accent when it is not zero: it is the only one of
           these that means something is happening right now, and the only one
           worth catching an eye that is looking at the columns instead. -->
      <span class="text-muted-foreground tabular flex items-center gap-1.5 text-[11px]">
        {{ stats.open }} open
        <span aria-hidden="true">·</span>
        <span :class="stats.working ? 'text-[var(--primary)] font-medium' : ''">
          {{ stats.working }} working
        </span>
        <span aria-hidden="true">·</span>
        {{ stats.done }} done
        <!-- Only when there are any: a zero here describes a control that is
             not on screen, since the toggle that reveals them is hidden too. -->
        <template v-if="stats.hidden">
          <span aria-hidden="true">·</span>
          {{ stats.hidden }} hidden
        </template>
      </span>

      <span
        v-if="workspace?.worktrees?.length"
        class="text-muted-foreground tabular flex items-center gap-1.5 text-[11px]"
        :title="workspace.worktrees.map((w) => `${w.role}: ${size(w.bytes)}`).join('\n')"
      >
        <HardDrive :size="11" class="shrink-0" aria-hidden="true" />
        {{ workspace.worktrees.length }} worktrees
        <span aria-hidden="true">·</span>
        {{ size(workspace.bytes) }}
      </span>
    </template>

    <template #actions><slot name="actions" /></template>
  </PageHeader>
</template>

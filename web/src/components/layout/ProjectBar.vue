<script setup lang="ts">
import type { Project, SwarmStatus } from '@/lib/api'
import { Button } from '@/components/ui/button'
import UsageSummary from '@/components/layout/UsageSummary.vue'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

const props = defineProps<{
  projects: Project[]
  current: Project | null
  status: SwarmStatus
  // Bumped when work completes, so the figure is not stale the moment it
  // matters most — right after a run that cost something.
  usageKey?: number
}>()
const emit = defineEmits<{
  openProject: [project: Project]
  addProject: []
  start: []
  stop: []
}>()

/** How many agents are actually up, as opposed to configured. */
function liveCount(s: SwarmStatus): number {
  return s.roles.filter((r) => r.state === 'ready' || r.state === 'working').length
}
</script>

<template>
  <!-- Project context and the action that acts on it, together. Starting the
       agents belongs beside the repository they will work in, not filed under
       navigation where "Start" has no visible object. -->
  <div class="hairline-b bg-[var(--surface-sunken)]/60 flex flex-wrap items-center gap-x-3 gap-y-2 px-[var(--gutter)] py-2.5">
    <Select
      v-if="projects.length"
      :model-value="current?.id"
      @update:model-value="(v) => emit('openProject', projects.find((p) => p.id === v)!)"
    >
      <SelectTrigger size="sm" class="w-52"><SelectValue placeholder="choose a project" /></SelectTrigger>
      <SelectContent>
        <SelectItem v-for="p in projects" :key="p.id" :value="p.id">{{ p.name }}</SelectItem>
      </SelectContent>
    </Select>

    <Button size="icon-sm" variant="outline" title="Add a project" @click="emit('addProject')">
      +
    </Button>

    <span v-if="current" class="text-muted-foreground truncate text-[11px]" :title="current.path">
      {{ current.path }} · {{ current.baseBranch }}
    </span>

    <div class="ml-auto flex items-center gap-3">
      <!-- Spend sits beside the agents that incur it, so it is noticed rather
           than gone looking for. -->
      <UsageSummary :project-id="current?.id ?? null" :refresh-key="usageKey" />

      <!-- State in words, with colour as reinforcement rather than the message. -->
      <span
        v-if="status.running"
        class="flex items-center gap-1.5 text-[11px] font-medium text-[var(--status-good)]"
      >
        <span class="pulse-dot size-1.5 rounded-full bg-current" />
        {{ liveCount(status) }} of {{ status.roles.length }} agents live
      </span>
      <span v-else class="text-muted-foreground text-[11px]">no agents running</span>

      <Button v-if="!status.running" size="sm" :disabled="!current" @click="emit('start')">
        Start agents
      </Button>
      <Button v-else size="sm" variant="destructive" @click="emit('stop')">Stop agents</Button>
    </div>
  </div>
</template>

<script setup lang="ts">
import type { Project, SwarmStatus } from '@/lib/api'
import { Bell, Play, Square } from '@lucide/vue'
import { Button } from '@/components/ui/button'
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
  attentionCount: number
}>()
const emit = defineEmits<{
  menu: []
  attention: []
  openProject: [project: Project]
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
  <!-- min-h rather than h: this bar wraps to two rows on a narrow screen, and
       a fixed height would clip the second one. It matches the brand block
       beside it whenever there is room, and grows when there is not. -->
  <div
    class="hairline-b bg-[var(--surface-sunken)]/60 flex min-h-[var(--topbar)] flex-wrap items-center gap-x-3 gap-y-2 px-[var(--gutter)] py-2"
  >
    <!-- Only below md, where the rail is a drawer rather than always present. -->
    <Button
      size="icon-sm"
      variant="ghost"
      class="md:hidden"
      aria-label="Open navigation"
      @click="emit('menu')"
    >
      ☰
    </Button>

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


    <div class="ml-auto flex items-center gap-3">
      <!-- Something is waiting on a person. It sits in the bar rather than
           behind a nav item, because it interrupts whatever you are reading
           instead of being somewhere you have to remember to go. -->
      <Button
        v-if="attentionCount"
        size="icon-sm"
        variant="ghost"
        class="relative text-[var(--status-warning)]"
        :title="`${attentionCount} waiting on you`"
        :aria-label="`${attentionCount} waiting on you`"
        @click="emit('attention')"
      >
        <Bell :size="15" aria-hidden="true" />
        <span
          class="bg-[var(--status-warning)] text-background absolute -top-0.5 -right-0.5 grid size-3.5 place-items-center rounded-full text-[9px] font-bold"
        >
          {{ attentionCount }}
        </span>
      </Button>

      <!-- State in words, with colour as reinforcement rather than the message. -->
      <span
        v-if="status.running"
        class="flex items-center gap-1.5 text-[11px] font-medium text-[var(--status-good)]"
      >
        <span class="pulse-dot size-1.5 rounded-full bg-current" />
        {{ liveCount(status) }}/{{ status.roles.length }}<span class="hidden sm:inline"> agents live</span>
      </span>
      <span v-else class="text-muted-foreground hidden text-[11px] sm:inline">no agents running</span>

      <!-- An icon, with the words in the tooltip and the aria label. The pair
           reads as one control at a glance, and on a phone it stops the bar
           wrapping for a verb. -->
      <Button
        v-if="!status.running"
        size="icon-sm"
        :disabled="!current"
        title="Start agents"
        aria-label="Start agents"
        @click="emit('start')"
      >
        <Play :size="14" aria-hidden="true" />
      </Button>
      <Button
        v-else
        size="icon-sm"
        variant="destructive"
        title="Stop agents"
        aria-label="Stop agents"
        @click="emit('stop')"
      >
        <Square :size="13" aria-hidden="true" />
      </Button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { RoleStatus, SwarmStatus } from '@/lib/api'

export type View = 'board' | 'team' | 'attention' | 'readiness'

const props = defineProps<{
  view: View
  status: SwarmStatus
  attentionCount: number
  taskCount: number
}>()
const emit = defineEmits<{ navigate: [view: View] }>()

const nav = computed(() => [
  { key: 'board' as const, label: 'Board', count: props.taskCount },
  { key: 'team' as const, label: 'Team', count: 0 },
  { key: 'attention' as const, label: 'Attention', count: props.attentionCount },
  { key: 'readiness' as const, label: 'Readiness', count: 0 },
])

/** Role state drives a colour and a word. Never colour alone. */
function tone(state: string): string {
  if (state === 'working') return 'text-[var(--primary)]'
  if (state === 'ready') return 'text-[var(--status-good)]'
  if (state === 'blocked' || state === 'failed') return 'text-destructive'
  return 'text-muted-foreground'
}
function live(r: RoleStatus): boolean {
  return r.state === 'working'
}
</script>

<template>
  <aside
    class="bg-[var(--surface-sunken)] hairline-r flex w-[var(--rail)] shrink-0 flex-col"
  >
    <!-- Identity, and whether anything is running at all. -->
    <div class="hairline-b flex items-center gap-2 px-3 py-3">
      <span
        class="bg-primary text-primary-foreground grid size-5 shrink-0 place-items-center text-[11px] font-bold"
        >z</span
      >
      <span class="text-sm font-bold tracking-tight">zerg</span>
      <span
        v-if="status.running"
        class="ml-auto flex items-center gap-1.5 text-[10px] font-semibold tracking-wider text-[var(--status-good)] uppercase"
      >
        <span class="pulse-dot size-1.5 rounded-full bg-current" />
        live
      </span>
    </div>

    <!-- Where to go. Counts sit right-aligned so the column scans vertically. -->
    <nav class="flex flex-col gap-px p-2">
      <button
        v-for="item in nav"
        :key="item.key"
        :class="[
          'group flex items-center gap-2 px-2 py-1.5 text-left text-xs transition-colors',
          'focus-visible:outline-ring focus-visible:outline-2 focus-visible:-outline-offset-2',
          view === item.key
            ? 'bg-primary/12 text-foreground font-semibold'
            : 'text-muted-foreground hover:bg-muted hover:text-foreground',
        ]"
        @click="emit('navigate', item.key)"
      >
        <span
          :class="[
            'h-3.5 w-px shrink-0',
            view === item.key ? 'bg-primary' : 'bg-transparent',
          ]"
        />
        {{ item.label }}
        <span
          v-if="item.count"
          :class="[
            'tabular ml-auto px-1 text-[10px] font-semibold',
            item.key === 'attention'
              ? 'bg-[var(--status-warning)]/15 text-[var(--status-warning)]'
              : 'text-muted-foreground',
          ]"
          >{{ item.count }}</span
        >
      </button>
    </nav>

    <!-- The live readout. This is the panel worth glancing at: which agents
         exist, and what each is doing right now. -->
    <div v-if="status.roles.length" class="hairline-t mt-1 flex min-h-0 flex-1 flex-col">
      <div
        class="text-muted-foreground px-3 pt-3 pb-1.5 text-[10px] font-bold tracking-[0.14em] uppercase"
      >
        Roles
      </div>
      <ul class="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
        <li v-for="r in status.roles" :key="r.role" class="px-1 py-1.5">
          <div class="flex items-center gap-2">
            <span
              :class="['size-1.5 shrink-0 rounded-full bg-current', tone(r.state), live(r) && 'pulse-dot']"
            />
            <span class="truncate text-xs font-medium">{{ r.role }}</span>
            <span :class="['tabular ml-auto text-[10px]', tone(r.state)]">{{ r.state }}</span>
          </div>
          <!-- A failed role says why, here, where it is already being looked at. -->
          <p v-if="r.lastError" class="text-destructive mt-1 pl-3.5 text-[10px] break-words">
            {{ r.lastError }}
          </p>
        </li>
      </ul>
    </div>
    <div v-else class="flex-1" />

  </aside>
</template>

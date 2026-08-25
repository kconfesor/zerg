<script setup lang="ts">
import { computed } from 'vue'
import {
  Activity as ActivityIcon,
  Columns3,
  FolderGit2,
  MessageSquare,
  Settings as SettingsIcon,
  ShieldCheck,
  Users,
} from '@lucide/vue'
import type { RoleStatus, SwarmStatus } from '@/lib/api'
import QuotaBars from '@/components/QuotaBars.vue'

import type { View } from '@/router'

const props = defineProps<{
  view: View
  status: SwarmStatus
  taskCount: number
  projectCount: number
  /** Whether the drawer is showing. Ignored at md and above, where the rail
   *  is always part of the layout. */
  open: boolean
}>()
const emit = defineEmits<{ navigate: [view: View]; close: [] }>()

// Icons are picked for what the view does, not for decoration: lanes for the
// board, a pulse for the live stream, a bell for the one screen that is
// waiting on a person.
const nav = computed(() => [
  { key: 'board' as const, label: 'Board', icon: Columns3, count: props.taskCount },
  { key: 'projects' as const, label: 'Projects', icon: FolderGit2, count: props.projectCount },
  { key: 'activity' as const, label: 'Activity', icon: ActivityIcon, count: 0 },
  { key: 'chat' as const, label: 'Chat', icon: MessageSquare, count: 0 },
  { key: 'team' as const, label: 'Team', icon: Users, count: 0 },
  { key: 'readiness' as const, label: 'Readiness', icon: ShieldCheck, count: 0 },
  { key: 'settings' as const, label: 'Settings', icon: SettingsIcon, count: 0 },
])

/** Role state drives a colour and a word. Never colour alone. */
function tone(state: string): string {
  if (state === 'working') return 'text-[var(--primary)]'
  if (state === 'ready') return 'text-[var(--status-good)]'
  if (state === 'blocked' || state === 'failed') return 'text-destructive'
  // Waiting on a quota window is not a fault. Amber, not red: nobody has to
  // do anything, and red would send someone looking for a problem to fix.
  if (state === 'throttled') return 'text-[var(--status-warning)]'
  return 'text-muted-foreground'
}

/** "in 47m", "in 2h 10m" — how long until the role resumes by itself. */
function resumesIn(iso?: string): string {
  if (!iso) return ''
  const ms = new Date(iso).getTime() - Date.now()
  if (!Number.isFinite(ms) || ms <= 0) return 'any moment'
  const mins = Math.round(ms / 60000)
  if (mins < 60) return `in ${mins}m`
  return `in ${Math.floor(mins / 60)}h ${mins % 60}m`
}
/** One entry per provider, ordered so the list is stable across polls. */
const quotas = computed(() =>
  Object.entries(props.status.quotas ?? {})
    .map(([provider, report]) => ({ provider, report }))
    .sort((a, b) => a.provider.localeCompare(b.provider)),
)

function live(r: RoleStatus): boolean {
  return r.state === 'working'
}
</script>

<template>
  <aside
    :class="[
      'bg-[var(--surface-sunken)] hairline-r flex w-[var(--rail)] shrink-0 flex-col',
      // Below md the rail would take most of a phone screen, so it slides in
      // over the content instead of pushing it into a column too narrow to
      // read. Above md it is an ordinary part of the layout and never moves.
      'max-md:fixed max-md:inset-y-0 max-md:left-0 max-md:z-50 max-md:transition-transform',
      open ? 'max-md:translate-x-0' : 'max-md:-translate-x-full',
    ]"
  >
    <!-- Identity, and whether anything is running at all. -->
    <div class="hairline-b flex min-h-[var(--topbar)] shrink-0 items-center gap-2.5 px-3">
      <!-- The raster, not the SVG. The mark's glow is two feGaussianBlur
           filters, and a filter is rasterised into an offscreen buffer whose
           resolution mobile browsers cap — a 512-unit viewBox drawn at 36px
           came out visibly pixelated on a phone. This PNG has the same glow
           baked in at 192px, which is five times what 36 CSS pixels need on a
           3x display, and no filter to resolve at render time. -->
      <img
        src="/android-chrome-192x192.png"
        alt=""
        width="36"
        height="36"
        class="size-9 shrink-0"
      />
      <span class="text-base font-bold tracking-tight">zerg</span>
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
          'group flex items-center gap-2 px-2 py-2 text-left text-xs transition-colors',
          'focus-visible:outline-ring focus-visible:outline-2 focus-visible:-outline-offset-2',
          view === item.key
            ? 'bg-primary/12 text-foreground font-semibold'
            : 'text-muted-foreground hover:bg-muted hover:text-foreground',
        ]"
        @click="emit('navigate', item.key), emit('close')"
      >
        <!-- The active rail stays. An icon changing colour is a weaker signal
             than a mark at the edge, and it is the one cue that survives at a
             glance down the column. -->
        <span
          :class="['h-4 w-px shrink-0', view === item.key ? 'bg-primary' : 'bg-transparent']"
        />
        <component
          :is="item.icon"
          :size="15"
          :stroke-width="view === item.key ? 2.25 : 1.75"
          class="shrink-0"
          aria-hidden="true"
        />
        {{ item.label }}
        <span
          v-if="item.count"
          :class="[
            'tabular ml-auto px-1 text-[10px] font-semibold',
            'text-muted-foreground',
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
          <!-- A throttled role says when it comes back, because that is the
               only question it raises. It is not an error and is not styled
               as one. -->
          <p
            v-if="r.state === 'throttled'"
            class="mt-1 pl-3.5 text-[10px] break-words text-[var(--status-warning)]"
          >
            provider limit · resumes {{ resumesIn(r.throttledUntil) || 'when the window rolls over' }}
            <span v-if="r.lastError" class="text-muted-foreground block">{{ r.lastError }}</span>
          </p>
          <!-- A failed role says why, here, where it is already being looked at. -->
          <p v-else-if="r.lastError" class="text-destructive mt-1 pl-3.5 text-[10px] break-words">
            {{ r.lastError }}
          </p>
        </li>
      </ul>

      <!-- What each account has left, under the roles that spend it. Here
           rather than the top bar: on a phone the bar is already carrying the
           project, the alerts and the run control, and a gauge is something you
           go and look at rather than something that has to be in your eye. -->
      <div v-if="quotas.length" class="hairline-t px-3 py-2.5">
        <p class="text-muted-foreground mb-1.5 text-[10px] font-semibold tracking-wide uppercase">
          Plan usage
        </p>
        <div v-for="q in quotas" :key="q.provider" class="mb-2 last:mb-0">
          <p class="text-muted-foreground/80 mb-1 truncate text-[10px]">
            {{ q.report.provider || q.provider
            }}<span v-if="q.report.plan"> · {{ q.report.plan }}</span>
          </p>
          <QuotaBars :quota="q.report" />
        </div>
      </div>
    </div>
    <div v-else class="flex-1" />

  </aside>
</template>

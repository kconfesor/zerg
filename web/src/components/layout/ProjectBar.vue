<script setup lang="ts">
import type { Project, SwarmStatus } from '@/lib/api'
import { computed } from 'vue'
import { Bell, Hourglass, Monitor, Moon, Play, Square, Sun } from '@lucide/vue'
import { Button } from '@/components/ui/button'
import UsageSummary from '@/components/layout/UsageSummary.vue'
import { isDark, theme, type Theme } from '@/lib/theme'

const props = defineProps<{
  current: Project | null
  status: SwarmStatus
  attentionCount: number
  /** A start or stop is in flight, so the control is not pressable again. */
  busy?: boolean
  /** Bumped when the board poll decides spend is worth re-reading. */
  usageKey?: number
}>()
const emit = defineEmits<{
  menu: []
  attention: []
  start: []
  stop: []
}>()

/** system -> light -> dark -> system. Named so the control can say what is
 *  next rather than leaving somebody to press it and find out. */
const order: Theme[] = ['system', 'light', 'dark']
const nextTheme = computed(() => order[(order.indexOf(theme.value) + 1) % order.length])
function cycleTheme() {
  theme.value = nextTheme.value
}

/** How many agents are actually up, as opposed to configured. */
function liveCount(s: SwarmStatus): number {
  return s.roles.filter((r) => r.state === 'ready' || r.state === 'working').length
}

/**
 * Roles waiting on a provider quota.
 *
 * Counted separately because "2/3 agents live" reads the same whether the
 * third crashed or is waiting out a limit, and those call for opposite
 * responses: one is yours to fix, the other is yours to ignore.
 */
const throttled = computed(() => props.status.roles.filter((r) => r.state === 'throttled'))

/** The soonest any of them comes back — the only number worth a top bar. */
const resumesIn = computed(() => {
  const times = throttled.value
    .map((r) => (r.throttledUntil ? new Date(r.throttledUntil).getTime() : NaN))
    .filter((t) => Number.isFinite(t))
  if (!times.length) return ''
  const mins = Math.round((Math.min(...times) - Date.now()) / 60000)
  if (mins <= 0) return 'any moment'
  return mins < 60 ? `${mins}m` : `${Math.floor(mins / 60)}h ${mins % 60}m`
})
</script>

<template>
  <!-- What is happening, and the control that changes it. The project itself
       moved to the rail, where it scopes every destination rather than sitting
       in a bar that scrolls past; what belongs here is the run state, because
       Start and Stop act on whatever the rail currently says. -->
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

    <!-- Run state and the control that changes it, together and first.
         Reading order is the argument: the button was on the far right and its
         subject — "no agents running" — was beside it, so the bar opened with
         nothing and ended with everything. What the project is doing is the
         first thing to know about it, so it is the first thing in the bar. -->
    <Button
      v-if="!status.running"
      size="icon-lg"
      :disabled="!current || busy"
      :title="busy ? 'Starting agents…' : 'Start agents'"
      :aria-label="busy ? 'Starting agents' : 'Start agents'"
      :aria-busy="busy || undefined"
      @click="emit('start')"
    >
      <Play :size="17" aria-hidden="true" />
    </Button>
    <!-- Stop beside Start, for a project that is down and still wanted.
         Stop used to appear only while something was running, which hid it in
         the one state it is needed for: a resume that fails preflight leaves
         the project stopped with the operator's standing intent intact, so
         every daemon start tries it again, and the button that withdraws that
         was behind the state it could not reach. Also how a start refused
         because a previous run's agents are unaccounted for is cleared, since
         that refusal is waiting for a person to say the worktree is free. -->
    <Button
      v-if="!status.running && status.wanted"
      size="icon-lg"
      variant="outline"
      :disabled="busy"
      title="This project is set to start itself. Stop asking for it."
      aria-label="Stop asking for this project to run"
      :aria-busy="busy || undefined"
      @click="emit('stop')"
    >
      <Square :size="15" aria-hidden="true" />
    </Button>
    <Button
      v-else-if="status.running"
      size="icon-lg"
      variant="destructive"
      :disabled="busy"
      :title="busy ? 'Stopping agents…' : 'Stop agents'"
      :aria-label="busy ? 'Stopping agents' : 'Stop agents'"
      :aria-busy="busy || undefined"
      @click="emit('stop')"
    >
      <Square :size="15" aria-hidden="true" />
    </Button>

    <span class="flex min-w-0 items-center gap-2">
      <!-- Below md the rail is a drawer, so the project is named nowhere on
           screen — and "3/3" is meaningless without knowing three of what. It
           rides with the run state rather than sitting on its own, because the
           pair is one fact: this project, this many agents. Hidden from md up,
           where the rail says it permanently. -->
      <span
        v-if="current"
        class="max-w-[9rem] truncate text-[11px] font-semibold md:hidden"
      >
        {{ current.name }}
      </span>

      <!-- State in words, with colour as reinforcement rather than the message. -->
      <span
        v-if="status.running"
        class="flex items-center gap-1.5 text-[11px] font-medium text-[var(--status-good)]"
      >
        <span class="pulse-dot size-1.5 rounded-full bg-current" />
        {{ liveCount(status) }}/{{ status.roles.length }}<span class="hidden sm:inline"> agents live</span>
      </span>
      <span v-else class="text-muted-foreground hidden text-[11px] sm:inline">no agents running</span>
    </span>

    <!-- A quota limit is not a fault and is not styled as one, but it does
         explain a number that would otherwise look like a crash. -->
    <span
      v-if="throttled.length"
      class="flex items-center gap-1.5 text-[11px] font-medium text-[var(--status-warning)]"
      :title="throttled.map((r) => `${r.role}: ${r.lastError || 'provider limit'}`).join('\n')"
    >
      <Hourglass :size="12" aria-hidden="true" />
      {{ throttled.length }} waiting on a limit<span v-if="resumesIn" class="hidden sm:inline">
        · back in {{ resumesIn }}</span
      >
    </span>

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

      <!-- What this is costing, on every screen rather than only on the board.
           It was a figure in the board's own stats row, which is the one place
           you are already looking at what the work is doing; spend is the thing
           you want to notice while looking at something else. -->
      <UsageSummary v-if="current" :project-id="current.id" :refresh-key="usageKey" />

      <!-- Light or dark. In the bar because it applies to the whole app rather
           than to a project, and because a preference you cannot find is one
           people ask for again.

           Three states, not a switch: "follow the system" is what most people
           want and a two-way toggle cannot say it. The icon shows what is on
           screen now; the title says what pressing it will do next. -->
      <Button
        size="icon-sm"
        variant="ghost"
        class="text-muted-foreground hover:text-foreground"
        :title="`Theme: ${theme} — press for ${nextTheme}`"
        :aria-label="`Theme: ${theme}. Press for ${nextTheme}.`"
        @click="cycleTheme"
      >
        <component :is="theme === 'system' ? Monitor : isDark() ? Moon : Sun" :size="15" aria-hidden="true" />
      </Button>
    </div>
  </div>
</template>

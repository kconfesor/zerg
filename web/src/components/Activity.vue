<script setup lang="ts">
/**
 * What the agents are actually doing.
 *
 * Styled as a terminal because that is the mental model — a scrolling record of
 * work — but it is not a terminal. Nothing here is scraped: every line is a
 * typed event, so it can be filtered by role, coloured by kind, and read back
 * after a reload. A pane of captured text can do none of that.
 */
import { computed, onBeforeUnmount, ref, watch, nextTick } from 'vue'
import {
  streamActivity,
  type ActivityEvent,
  type ActivityStream,
  type StreamState,
} from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'

const props = defineProps<{ projectId: string; roles: string[] }>()

const events = ref<ActivityEvent[]>([])
const roleFilter = ref<string>('')
const state = ref<StreamState>('connecting')
const follow = ref(true)
const viewport = ref<HTMLElement | null>(null)

let stream: ActivityStream | null = null

/**
 * The transcript is capped in the browser as well as on the server. A long run
 * would otherwise grow the DOM without bound, and the tab gets slow long before
 * anyone scrolls back that far.
 */
const MAX_LINES = 2000

function connect() {
  stream?.close()
  events.value = []
  state.value = 'connecting'
  if (!props.projectId) return

  stream = streamActivity(
    props.projectId,
    {
      onEvent: (e) => {
        events.value.push(e)
        if (events.value.length > MAX_LINES) {
          events.value.splice(0, events.value.length - MAX_LINES)
        }
        if (follow.value) scrollToEnd()
      },
      onCaughtUp: () => scrollToEnd(),
      onState: (s) => (state.value = s),
    },
    { role: roleFilter.value || undefined },
  )
}

function scrollToEnd() {
  nextTick(() => {
    const el = viewport.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

/** Scrolling up stops the follow, the way `tail -f` stops being useful when you
 * are trying to read something further back. */
function onScroll() {
  const el = viewport.value
  if (!el) return
  follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

watch(() => [props.projectId, roleFilter.value], connect, { immediate: true })
onBeforeUnmount(() => stream?.close())

/** Role identity is categorical: a fixed hue per role, assigned by position and
 * never recycled, so a role keeps its colour as the filter changes. */
function roleHue(role: string): string {
  const i = props.roles.indexOf(role)
  return i < 0 ? 'var(--chart-5)' : `var(--chart-${(i % 4) + 1})`
}

function time(at: string): string {
  return new Date(at).toLocaleTimeString(undefined, { hour12: false })
}

function usageOf(e: ActivityEvent) {
  const d = (e.data ?? {}) as Record<string, number | string>
  return {
    in: Number(d.in ?? 0),
    cacheRead: Number(d.cacheRead ?? 0),
    cacheWrite: Number(d.cacheWrite ?? 0),
    out: Number(d.out ?? 0),
    cost: Number(d.costUsd ?? 0),
    billing: String(d.billing ?? ''),
  }
}

function command(e: ActivityEvent): string {
  const d = (e.data ?? {}) as Record<string, unknown>
  const raw = d.command ?? d.file_path ?? d.path ?? d.pattern ?? ''
  return typeof raw === 'string' ? raw : JSON.stringify(d)
}

const num = new Intl.NumberFormat()
const roleCounts = computed(() => {
  const counts = new Map<string, number>()
  for (const e of events.value) counts.set(e.role, (counts.get(e.role) ?? 0) + 1)
  return counts
})
</script>

<template>
  <div class="flex flex-col gap-3">
    <!-- Filters in one row above the stream, so the reading area starts at a
         predictable place regardless of how many roles a project has. -->
    <div class="flex flex-wrap items-center gap-1.5">
      <Button
        :variant="roleFilter === '' ? 'default' : 'outline'"
        size="sm"
        @click="roleFilter = ''"
      >
        All roles
      </Button>
      <Button
        v-for="role in roles"
        :key="role"
        :variant="roleFilter === role ? 'default' : 'outline'"
        size="sm"
        @click="roleFilter = role"
      >
        <span
          class="mr-1.5 inline-block h-2 w-2 shrink-0"
          :style="{ background: roleHue(role) }"
          aria-hidden="true"
        />
        {{ role }}
        <span v-if="roleCounts.get(role)" class="ml-1.5 opacity-60 tabular-nums">
          {{ roleCounts.get(role) }}
        </span>
      </Button>

      <div class="ml-auto flex items-center gap-2">
        <!-- Connection state is a word, never a colour alone. "reconnecting"
             is distinct from "connecting" on purpose: the first means the feed
             dropped and is being resumed from its cursor, which is worth
             seeing, and the second is just startup. -->
        <Badge :variant="state === 'live' ? 'default' : 'outline'">
          {{ state }}
        </Badge>
        <Button v-if="!follow" variant="outline" size="sm" @click="follow = true; scrollToEnd()">
          Follow ↓
        </Button>
      </div>
    </div>

    <div
      ref="viewport"
      class="bg-card h-[62vh] overflow-y-auto border font-mono text-xs leading-relaxed"
      @scroll="onScroll"
    >
      <p v-if="!events.length" class="text-muted-foreground p-4">
        Nothing recorded yet. Start the agents and give them a task — this fills in as they work.
      </p>

      <ol>
        <li
          v-for="e in events"
          :key="e.id"
          class="hover:bg-muted/40 flex gap-2 px-3 py-0.5"
          :class="e.kind === 'error' ? 'bg-destructive/10' : ''"
        >
          <span class="text-muted-foreground shrink-0 tabular-nums">{{ time(e.at) }}</span>
          <span class="shrink-0 font-semibold" :style="{ color: roleHue(e.role) }">
            {{ e.role }}
          </span>

          <!-- One line per kind. Tool calls show what ran; usage shows the
               token split, never a single blended figure. -->
          <span v-if="e.kind === 'tool_call'" class="min-w-0 flex-1 break-all">
            <span class="text-muted-foreground">$</span>
            <span class="ml-1">{{ e.tool }}</span>
            <span class="text-muted-foreground ml-1.5">{{ command(e) }}</span>
          </span>

          <span v-else-if="e.kind === 'usage'" class="text-muted-foreground min-w-0 flex-1">
            <template v-for="(v, k) in usageOf(e)" :key="k">
              <template v-if="k !== 'cost' && k !== 'billing'">
                <span class="mr-2 tabular-nums">{{ k }} {{ num.format(v as number) }}</span>
              </template>
            </template>
            <span class="text-foreground tabular-nums">
              ${{ usageOf(e).cost.toFixed(4) }}
            </span>
            <span v-if="usageOf(e).billing === 'subscription'" class="ml-1.5 opacity-70">
              (plan — no marginal cost)
            </span>
          </span>

          <span v-else-if="e.kind === 'error'" class="text-destructive min-w-0 flex-1">
            {{ e.fatal ? 'fatal: ' : 'error: ' }}{{ e.text }}
          </span>

          <span v-else-if="e.kind === 'message'" class="min-w-0 flex-1 whitespace-pre-wrap">
            {{ e.text }}
          </span>

          <span v-else class="text-muted-foreground min-w-0 flex-1">{{ e.kind }}</span>
        </li>
      </ol>
    </div>
  </div>
</template>

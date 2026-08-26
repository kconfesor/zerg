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
import { renderMarkdown } from '@/lib/markdown'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'

const props = defineProps<{
  projectId: string
  roles: string[]
  /** Show only this task's events. The stream has always supported it; there
   *  was no way to ask for it from the board. */
  task?: string
  /** Inside a dialog: no role filter of its own — it is already filtered to one
   *  card — and it sizes to the space rather than to the viewport. */
  embedded?: boolean
  /** Whether the card is still being worked on. Only used to tell a quiet feed
   *  from a finished one: an agent thinking for two minutes and a stream that
   *  died look identical otherwise, and only one is worth acting on. */
  active?: boolean
  /** What the card is blocked on, if anything. A feed goes quiet for two very
   *  different reasons — the agent is thinking, or it has asked you something
   *  and stopped — and the second one is quiet forever until you act. */
  blocked?: 'approval' | 'question'
}>()

const events = ref<ActivityEvent[]>([])
const roleFilter = ref<string>('')
const state = ref<StreamState>('connecting')
/** What the server said went wrong, when it stayed up long enough to say. */
const streamError = ref('')
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
  if (frame) {
    cancelAnimationFrame(frame)
    frame = 0
  }
  pending = []
  events.value = []
  state.value = 'connecting'
  if (!props.projectId) return

  stream = streamActivity(
    props.projectId,
    {
      onEvent: (e) => queue(e),
      onCaughtUp: () => scrollToEnd(),
      onState: (s) => {
        state.value = s
        if (s === 'live') streamError.value = ''
      },
      onError: (m) => (streamError.value = m),
    },
    { role: roleFilter.value || undefined, task: props.task || undefined },
  )
}

/**
 * Events arrive faster than a screen can usefully change.
 *
 * A busy replay delivers hundreds in a burst, and pushing each one onto a
 * reactive array re-rendered the list and scheduled a scroll every time — the
 * work grows with the number of events while the visible result is identical.
 * They are collected and applied once per frame instead, which is as often as
 * anything can actually be seen.
 */
let pending: ActivityEvent[] = []
let frame = 0

function queue(e: ActivityEvent) {
  pending.push(e)
  if (frame) return
  frame = requestAnimationFrame(() => {
    frame = 0
    const batch = pending
    pending = []
    events.value.push(...batch)
    if (events.value.length > MAX_LINES) {
      events.value.splice(0, events.value.length - MAX_LINES)
    }
    if (follow.value) scrollToEnd()
  })
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

watch(() => [props.projectId, roleFilter.value, props.task], connect, { immediate: true })
onBeforeUnmount(() => {
  stream?.close()
  if (frame) cancelAnimationFrame(frame)
})

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
         predictable place regardless of how many roles a project has. Hidden
         when embedded: filtered to one card already, the roles that touched it
         are few and naming them is what the transcript does anyway. -->
    <div v-if="!embedded" class="flex flex-wrap items-center gap-1.5">
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
        <Badge
          :variant="state === 'live' ? 'default' : state === 'error' ? 'destructive' : 'outline'"
          :title="streamError || undefined"
        >
          {{ state }}
        </Badge>
        <Button v-if="!follow" variant="outline" size="sm" @click="follow = true; scrollToEnd()">
          Follow ↓
        </Button>
      </div>
    </div>

    <!-- Embedded has no filter bar, and lost with it every sign that this feed
         is live: the socket is open and replaying, but a dialog that shows a
         static list and says nothing reads as a snapshot. This is the same two
         controls, at the size a dialog can afford. -->
    <div v-if="embedded" class="flex items-center gap-2 text-[11px]">
      <span
        :class="[
          'size-1.5 shrink-0 rounded-full',
          state === 'live'
            ? 'bg-[var(--status-good)]'
            : state === 'error'
              ? 'bg-destructive'
              : 'bg-[var(--status-warning)]',
          state === 'live' && active && 'pulse-dot',
        ]"
        aria-hidden="true"
      />
      <span class="text-muted-foreground" role="status">
        <template v-if="state !== 'live'">{{ state }}</template>
        <template v-else-if="blocked">live · waiting on you</template>
        <template v-else-if="active">live</template>
        <template v-else>live · card is not working</template>
      </span>
      <span v-if="streamError" class="text-destructive truncate">{{ streamError }}</span>
      <Button
        v-if="!follow"
        variant="outline"
        size="xs"
        class="ml-auto"
        @click="follow = true; scrollToEnd()"
      >
        Follow ↓
      </Button>
    </div>

    <div
      ref="viewport"
      :class="[
        'bg-card overflow-y-auto border font-mono text-[11px] leading-relaxed md:text-xs',
        embedded ? 'h-[55vh]' : 'h-[70vh] md:h-[62vh]',
      ]"
      @scroll="onScroll"
    >
      <!-- A feed that failed must not read as a project with nothing in it.
           Those look identical, and only one of them is worth acting on. -->
      <p v-if="streamError && !events.length" class="text-destructive p-4 text-xs">
        {{ streamError }} — retrying.
      </p>
      <p v-else-if="!events.length" class="text-muted-foreground p-4">
        Nothing recorded yet. Start the agents and give them a task — this fills in as they work.
      </p>

      <ol>
        <!-- v-memo with no dependencies: an event is immutable once recorded,
             so a row that has been rendered never has to be patched again. With
             two thousand of them mounted, every new event otherwise walked the
             whole list to discover that nothing above had changed. -->
        <li
          v-for="e in events"
          :key="e.id"
          v-memo="[]"
          class="hover:bg-muted/40 flex flex-col px-2 py-0.5 md:flex-row md:gap-2 md:px-3"
          :class="e.kind === 'error' ? 'bg-destructive/10' : ''"
        >
          <!-- Time and role sit above the content on a phone and beside it on a
               desktop. Kept in the row, they leave a dead left column and squeeze
               a wrapped message into a third of the width. -->
          <span class="flex shrink-0 gap-2">
            <span class="text-muted-foreground tabular-nums">{{ time(e.at) }}</span>
            <span class="font-semibold" :style="{ color: roleHue(e.role) }">{{ e.role }}</span>
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

          <span v-else-if="e.kind === 'message'" class="md min-w-0 flex-1" v-html="renderMarkdown(e.text ?? '')" />

          <span v-else class="text-muted-foreground min-w-0 flex-1">{{ e.kind }}</span>
        </li>

        <!-- The feed is caught up and the agent has not spoken yet. Without
             this the last line of a five-minute-old turn is the whole of what
             the dialog says, and looks like the end of the story. -->
        <li
          v-if="(active || blocked) && state === 'live' && events.length"
          class="flex items-center gap-2 px-2 py-1 md:px-3"
          :class="blocked ? 'text-[var(--status-warning)]' : 'text-muted-foreground/70'"
        >
          <span
            class="pulse-dot size-1.5 rounded-full"
            :class="blocked ? 'bg-[var(--status-warning)]' : 'bg-muted-foreground/60'"
            aria-hidden="true"
          />
          <template v-if="blocked === 'approval'">
            waiting for your approval — nothing downstream moves until it is decided
          </template>
          <template v-else-if="blocked === 'question'">
            waiting for your answer — the role asked something and stopped
          </template>
          <template v-else>waiting for the next turn…</template>
        </li>
      </ol>
    </div>
  </div>
</template>

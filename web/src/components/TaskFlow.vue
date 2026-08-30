<script setup lang="ts">
/**
 * One card's history as a sequence diagram.
 *
 * The same steps read as a list before — `reviewer → coder`, one line each —
 * and a list cannot show the thing that matters most about them: direction.
 * Work going *back* up the pipeline looks identical to work going forward when
 * both are a line of text with an arrow glyph in the middle. Here a rejection
 * points left, and the shape of a card that bounced is visible from across the
 * room.
 *
 * Lifelines are the team's roles in pipeline order, with `operator` at the head
 * (every card starts as an instruction from a person) and the base branch at
 * the tail, since merging is the last step of the same route and the only one
 * that is not a role.
 *
 * Positions are percentages of the participant count, so the diagram is
 * resolution-independent and the whole thing scrolls sideways inside its own
 * box on a narrow screen rather than squeezing the arrows into nothing.
 */
import { computed, ref } from 'vue'
import { ChevronLeft, ChevronDown, ChevronRight, ChevronUp } from '@lucide/vue'
import { api, type ActivityEvent, type Project, type TaskStep } from '@/lib/api'
import { duration, landing, money } from '@/lib/utils'

const props = defineProps<{
  /** The task these steps belong to, so a step can fetch its own transcript. */
  taskId?: string
  steps: TaskStep[]
  /** The team's roles, in pipeline order. */
  roles: string[]
  /** Read for where finished work lands, for a card that has not landed yet:
   *  merge, pull request, or nowhere. A card that has ended says so itself. */
  project?: Project | null
  /** What happened to this card's work, and where it went, as recorded when it
   *  happened. A project's integration setting is a live value and answers a
   *  different question tomorrow, which is why this was recorded at all. */
  outcome?: string
  outcomeRef?: string
  /** Which role is holding the card now, if any — the lane it sits in. */
  current?: string
  /** Roles this card was told to skip, by name. Drawn rather than left out:
   *  the column is why the work jumped from one role to another, and a gap
   *  where a role should be reads as something having gone wrong. */
  skipped?: string[]
}>()

const MERGED = '(merged)'

/**
 * What a role actually did during one step, read on demand.
 *
 * The trail says a step took two minutes and cost seventy cents. What it did in
 * that time is in the events, which are the expensive tier and the one that
 * ages out (ARCHITECTURE §12.1), so they are fetched per step when asked for
 * rather than shipped with every card.
 *
 * The window is the one the daemon summed this step's cost over, handed out on
 * the step itself. It used to be the handoff plus a guessed half minute, which
 * disagreed with the cost in both directions: a closing turn that ran longer
 * was missing from what the step "did" while being charged to it, and a quick
 * second lap by the same role leaked into the step before it.
 */
const open = ref<Set<number>>(new Set())
const slices = ref<Record<number, ActivityEvent[]>>({})
const cut = ref<Record<number, boolean>>({})
const loading = ref<Set<number>>(new Set())
const failed = ref<Record<number, string>>({})

async function toggle(index: number, step: TaskStep) {
  const showing = new Set(open.value)
  if (showing.has(index)) {
    showing.delete(index)
    open.value = showing
    return
  }
  showing.add(index)
  open.value = showing
  if (slices.value[index] || loading.value.has(index) || !props.taskId) return

  const from = step.windowStart ?? step.startedAt ?? props.steps[index - 1]?.at
  if (!from) return
  loading.value = new Set(loading.value).add(index)
  try {
    const slice = await api.taskEvents(props.taskId, {
      role: step.from,
      from,
      // Absent on a role's last step, which runs to the end of the card: an
      // open window is the honest one there.
      until: step.windowEnd,
    })
    slices.value = { ...slices.value, [index]: slice.events }
    cut.value = { ...cut.value, [index]: slice.truncated }
  } catch (e) {
    failed.value = { ...failed.value, [index]: e instanceof Error ? e.message : String(e) }
  } finally {
    const running = new Set(loading.value)
    running.delete(index)
    loading.value = running
  }
}

/** A step can only be opened where there is a window to read it over. */
function readable(index: number, step: TaskStep): boolean {
  return !!props.taskId && !!(step.windowStart ?? step.startedAt ?? props.steps[index - 1]?.at)
}

function short(event: ActivityEvent): string {
  if (event.kind === 'tool_call') return event.tool || 'tool'
  return (event.text ?? '').replace(/\s+/g, ' ').trim()
}

/**
 * What a person reads out of a step: the calls it made, what it said, and what
 * broke.
 *
 * The rest of a transcript is mechanics. Sixty-seven rows for one step came
 * back as "ok" from every tool_done, blanks from thinking, and a turn_end and a
 * usage line per turn, which is a wall of nothing between the lines that say
 * what happened. They are still recorded and still count towards the step's
 * cost; they are not what this list is for.
 */
const READABLE = new Set(['tool_call', 'message', 'error'])

function readableSlice(index: number): ActivityEvent[] {
  return (slices.value[index] ?? []).filter((e) => READABLE.has(e.kind) && short(e) !== '')
}

/** How much of the step is not shown, so the list does not pretend to be all
 *  of it. */
function hiddenCount(index: number): number {
  return (slices.value[index]?.length ?? 0) - readableSlice(index).length
}

/**
 * Every lifeline the card touched, in pipeline order.
 *
 * Built from the team rather than from the steps, so a role that has not been
 * reached yet still has a column: the gap between what has happened and what is
 * left is half of what this diagram is for. Roles the card visited that the
 * team no longer has are appended, because a history must survive the pipeline
 * being edited under it.
 */
const participants = computed<string[]>(() => {
  const out: string[] = []
  if (props.steps.some((s) => s.from === 'operator')) out.push('operator')
  for (const r of props.roles) if (!out.includes(r)) out.push(r)
  for (const s of props.steps) {
    if (s.from && !out.includes(s.from)) out.push(s.from)
    if (s.to && !out.includes(s.to)) out.push(s.to)
  }
  if (props.project || props.steps.some((s) => s.final)) out.push(MERGED)
  return out
})

/** Centre of a lifeline, as a percentage of the diagram's width. */
function centre(name: string): number {
  const n = participants.value.length
  const i = participants.value.indexOf(name)
  return i < 0 ? 0 : ((i + 0.5) / n) * 100
}

type Arrow = {
  step: TaskStep
  from: number
  to: number
  /** True when the work went back up the pipeline. */
  back: boolean
  left: number
  width: number
}

const arrows = computed<Arrow[]>(() =>
  props.steps.map((s) => {
    const from = centre(s.from)
    const to = centre(s.final ? MERGED : (s.to ?? s.from))
    return {
      step: s,
      from,
      to,
      back: to < from,
      left: Math.min(from, to),
      width: Math.abs(to - from),
    }
  }),
)

function time(at: string): string {
  return new Date(at).toLocaleTimeString(undefined, { hour12: false })
}

/**
 * A participant's label. The last one is a destination rather than a role, and
 * which destination is a project setting: a branch to merge into, a pull
 * request to open, or nothing at all.
 */
function label(name: string): string {
  if (name !== MERGED) return name
  // What this card actually did, when it is known. The project setting is the
  // fallback for a card still in flight, and it was the only source until
  // outcomes were recorded: a card merged last week under a project since
  // switched to pull requests was labelled as having opened one.
  switch (props.outcome) {
    case 'merged':
      return 'merged'
    case 'pr':
      return 'pull request'
    case 'branch':
      return 'its branch'
  }
  return props.project ? landing(props.project).head : 'merged'
}

/** The pull request a card opened, for the one outcome that has somewhere to
 *  go. A commit is shown on the step that made it. */
const pullRequest = computed(() =>
  props.outcome === 'pr' && props.outcomeRef?.startsWith('http') ? props.outcomeRef : '',
)
</script>

<template>
  <!-- On a phone the columns compress rather than the box scrolling: the notes
       live in this same column, and prose you have to drag sideways to read is
       a worse failure than a truncated participant chip. Past sm there is room
       for both, and a floor keeps the arrows from collapsing into dots. -->
  <div class="overflow-x-auto">
    <div class="relative sm:min-w-[30rem]">
      <!-- Heads. Sticky, so the names stay readable while a long history
           scrolls past them. -->
      <div class="bg-popover sticky top-0 z-10 flex pb-2">
        <span
          v-for="p in participants"
          :key="p"
          class="min-w-0 flex-1 px-1 text-center"
          :style="{ flexBasis: `${100 / participants.length}%` }"
        >
          <span
            :class="[
              'inline-block max-w-full truncate border px-1.5 py-0.5 text-[10px] font-semibold',
              p === current
                ? 'border-primary/50 bg-primary/[0.12] text-foreground'
                : p === MERGED
                  ? 'text-muted-foreground border-dashed'
                  : 'text-muted-foreground',
              skipped?.includes(p) ? 'border-dashed opacity-45' : '',
            ]"
          >
            {{ label(p) }}
          </span>
          <span
            v-if="skipped?.includes(p)"
            class="text-muted-foreground mt-0.5 block text-[9px] tracking-wide uppercase"
          >
            skipped
          </span>
        </span>
      </div>

      <!-- The lifelines themselves, behind every row. -->
      <div class="pointer-events-none absolute inset-x-0 top-7 bottom-0" aria-hidden="true">
        <span
          v-for="p in participants"
          :key="p"
          class="border-muted-foreground/25 absolute top-0 bottom-0 border-l border-dashed"
          :style="{ left: `${centre(p)}%` }"
        />
      </div>

      <ol class="relative">
        <li v-for="(a, i) in arrows" :key="i" class="pt-1 pb-3">
          <!-- The arrow. Label above the line rather than beside it: beside it
               there is only ever room for one of the two. -->
          <div class="relative h-9">
            <span
              class="text-muted-foreground absolute top-0 -translate-x-1/2 px-1 text-center text-[10px] whitespace-nowrap"
              :style="{ left: `${a.left + a.width / 2}%` }"
            >
              <code v-if="a.step.commit" class="text-foreground/80">
                {{ a.step.commit.slice(0, 8) }}
              </code>
              <span v-else>{{ a.step.kind }}</span>
            </span>

            <span
              :class="[
                'absolute top-6 h-px',
                a.back ? 'bg-[var(--status-warning)]' : 'bg-primary/60',
                a.width === 0 && 'hidden',
              ]"
              :style="{ left: `${a.left}%`, width: `${a.width}%` }"
            />
            <component
              :is="a.back ? ChevronLeft : ChevronRight"
              :size="12"
              :class="[
                'absolute top-[18px]',
                a.back ? 'text-[var(--status-warning)]' : 'text-primary',
              ]"
              :style="{ left: `calc(${a.to}% - ${a.back ? 2 : 10}px)` }"
              aria-hidden="true"
            />
          </div>

          <!-- What the role said it did. The substance of the step, in prose,
               under the arrow that carried it. -->
          <div class="flex gap-3 px-1">
            <span class="text-muted-foreground tabular shrink-0 text-[10px] leading-5">
              {{ time(a.step.at) }}
            </span>
            <div class="min-w-0 flex-1">
              <p class="text-[11px] leading-5">
                <span class="font-semibold">{{ a.step.from }}</span>
                <span class="text-muted-foreground mx-1">{{ a.back ? '↩' : '→' }}</span>
                <span class="font-semibold">
                  {{ a.step.final ? label(MERGED) : (a.step.to ?? 'nobody') }}
                </span>
                <span v-if="a.step.subject" class="text-muted-foreground ml-1.5">
                  {{ a.step.subject }}
                </span>
                <!-- Where the work went, on the step that sent it there. It was
                     recorded so it could be read; without this it was in the
                     database and nowhere on screen. -->
                <a
                  v-if="a.step.final && pullRequest"
                  :href="pullRequest"
                  target="_blank"
                  rel="noreferrer"
                  class="text-[var(--primary)] ml-1.5 underline underline-offset-2"
                >
                  pull request
                </a>
              </p>
              <!-- What this step cost and how long it took, which is where a
                   card's hours go. Per step rather than per card: a total says
                   the pipeline cost four dollars and not which role spent it,
                   nor which lap. -->
              <p
                v-if="a.step.durationMs || a.step.costUsd || a.step.gate || a.step.clarifications?.length"
                class="text-muted-foreground mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px]"
              >
                <span v-if="a.step.durationMs" class="tabular">{{ duration(a.step.durationMs) }}</span>
                <span v-if="a.step.costUsd" class="tabular">{{ money(a.step.costUsd) }}</span>
                <!-- A gate is not a delay in the pipeline, it is a wait on a
                     person, and it is invisible in any per-card total. -->
                <span
                  v-if="a.step.gate"
                  :class="a.step.gate.state === 'pending' ? 'text-[var(--status-warning)]' : ''"
                >
                  {{ a.step.gate.state === 'pending' ? 'waiting for a decision' : a.step.gate.state }}
                  <template v-if="a.step.gate.waitedMs >= 60_000">
                    after {{ duration(a.step.gate.waitedMs) }}
                  </template>
                </span>
                <span v-if="a.step.clarifications?.length">
                  {{ a.step.clarifications.length }}
                  question{{ a.step.clarifications.length === 1 ? '' : 's' }} asked
                </span>
              </p>
              <slot name="note" :step="a.step" />

              <!-- What the role did in that time. The trail says two minutes
                   and seventy cents; this is the two minutes. -->
              <button
                v-if="readable(i, a.step)"
                type="button"
                class="text-muted-foreground hover:text-foreground focus-visible:outline-ring mt-1 flex items-center gap-1 text-[10px] focus-visible:outline-2"
                @click="toggle(i, a.step)"
              >
                <component :is="open.has(i) ? ChevronUp : ChevronDown" :size="10" aria-hidden="true" />
                {{ open.has(i) ? 'hide what it did' : 'what it did' }}
              </button>
              <div v-if="open.has(i)" class="mt-1">
                <p v-if="loading.has(i)" class="text-muted-foreground text-[10px]">Reading…</p>
                <p v-else-if="failed[i]" class="text-destructive text-[10px]">{{ failed[i] }}</p>
                <ol v-else-if="readableSlice(i).length" class="border-border/70 ml-0.5 border-l pl-2">
                  <li
                    v-for="e in readableSlice(i)"
                    :key="e.id"
                    :class="[
                      'flex gap-2 py-0.5 text-[10px] leading-4',
                      e.kind === 'error' ? 'text-destructive' : 'text-muted-foreground',
                    ]"
                  >
                    <span class="tabular shrink-0 opacity-60">{{ time(e.at) }}</span>
                    <span v-if="e.kind === 'tool_call'" class="text-foreground/70 shrink-0">
                      {{ e.tool || 'tool' }}
                    </span>
                    <span v-else class="min-w-0 flex-1 truncate" :title="short(e)">{{ short(e) }}</span>
                  </li>
                </ol>
                <!-- Not an empty box: events are the tier that ages out, and a
                     step whose transcript is gone should say so. -->
                <p v-else class="text-muted-foreground text-[10px]">
                  No transcript kept for this step.
                </p>
                <p v-if="open.has(i) && hiddenCount(i) > 0" class="text-muted-foreground mt-0.5 text-[10px]">
                  and {{ hiddenCount(i) }} more of the machinery: tool results, thinking, turn
                  accounting.
                </p>
                <!-- Said rather than cut silently: a transcript that stops at
                     the page boundary reads like a step that stopped there. -->
                <p v-if="cut[i]" class="text-muted-foreground mt-0.5 text-[10px]">
                  This step is longer than one page; the rest is not shown.
                </p>
              </div>
            </div>
          </div>
        </li>
      </ol>
    </div>
  </div>
</template>

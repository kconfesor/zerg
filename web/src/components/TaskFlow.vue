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
import { computed } from 'vue'
import { ChevronLeft, ChevronRight } from '@lucide/vue'
import type { Project, TaskStep } from '@/lib/api'
import { duration, landing, money } from '@/lib/utils'

const props = defineProps<{
  steps: TaskStep[]
  /** The team's roles, in pipeline order. */
  roles: string[]
  /** Read for where finished work lands: merge, pull request, or nowhere. */
  project?: Project | null
  /** Which role is holding the card now, if any — the lane it sits in. */
  current?: string
}>()

const MERGED = '(merged)'

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
  return props.project ? landing(props.project).head : 'merged'
}
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
            ]"
          >
            {{ label(p) }}
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
            <span class="text-muted-foreground/70 tabular shrink-0 text-[10px] leading-5">
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
              </p>
              <!-- What this step cost and how long it took, which is where a
                   card's hours go. Per step rather than per card: a total says
                   the pipeline cost four dollars and not which role spent it,
                   nor which lap. -->
              <p
                v-if="a.step.durationMs || a.step.costUsd || a.step.gate || a.step.clarifications?.length"
                class="text-muted-foreground/80 mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px]"
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
            </div>
          </div>
        </li>
      </ol>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { ResolvedRole, Task } from '@/lib/api'
import { duration, taskState } from '@/lib/utils'

/**
 * "3m ago" rather than a timestamp. On a board the useful question is how long
 * something has been sitting, and a clock time makes you do that subtraction
 * yourself.
 */
function ago(iso?: string): string {
  if (!iso) return ''
  const secs = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (secs < 60) return 'just now'
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`
  return `${Math.floor(secs / 86400)}d ago`
}

function compactTokens(n: number): string {
  if (!n) return ''
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${Math.round(n / 1000)}k`
  return String(n)
}

function money(n: number): string {
  return n >= 1 ? `$${n.toFixed(2)}` : `$${n.toFixed(3)}`
}
import { Bell, Eye, EyeOff, MessageCircleQuestion, ScrollText, Square, Trash2 } from '@lucide/vue'
import { Badge } from '@/components/ui/badge'

const props = defineProps<{
  team: ResolvedRole[]
  tasks: Task[]
  /** Ids of tasks with something waiting on a person. */
  needsAttention?: string[]
  /** What each is waiting for, so the card can say which kind. */
  blockedOn?: Map<string, 'question' | 'approval'>
  /** Whether cards a person has put away are shown. */
  showHidden?: boolean
}>()
const emit = defineEmits<{
  open: [task: Task]
  review: [task: Task]
  hide: [task: Task]
  unhide: [task: Task]
  stop: [task: Task]
  activity: [task: Task]
  remove: [task: Task]
}>()

/** Lanes are the enabled roles in pipeline order, then the Done well. */
const lanes = computed(() => {
  const roles = props.team.filter((r) => r.enabled).map((r) => r.name)
  return [...roles, 'done']
})

const byLane = computed(() => {
  const map = new Map<string, Task[]>()
  for (const lane of lanes.value) map.set(lane, [])
  for (const task of props.tasks) {
    // A hidden card is skipped rather than dimmed: the lane count has to agree
    // with the cards under it, or the header is lying about the column.
    if (task.hidden && !props.showHidden) continue
    map.get(task.lane)?.push(task)
  }
  return map
})
</script>

<template>
  <!-- Lanes share the width when there are few and scroll sideways when there
       are many. The page itself never scrolls horizontally. -->
  <div class="-mx-[var(--gutter)] overflow-x-auto px-[var(--gutter)] pb-2">
    <!-- Lanes stack on a phone and sit in one row above it. basis-full below
         sm rather than a width: flex-1 overrides width but respects basis, so
         without it lanes would silently share a phone row — tolerable at three
         roles, 45px each at eight.
         The width is measured, not chosen: a card's tightest row — "done 6h
         ago · 3.3M · $1.61" — needs 147px, and the card adds ~20px of padding.
         192 leaves that a little slack; much below 176 and the line wraps and
         every card grows a row.
         nowrap past sm is what makes the scroll box above do its job. Wrapping
         there instead meant a team of eight broke into two and three rows of
         lanes, so the pipeline no longer read left to right and a card's lane
         had to be found rather than seen. A board scrolls sideways; that is
         what a board is. -->
    <div class="flex flex-wrap items-start gap-3 sm:flex-nowrap">
      <section
        v-for="(lane, i) in lanes"
        :key="lane"
        class="rise flex min-w-0 flex-1 basis-full flex-col sm:min-w-44 sm:max-w-96 sm:basis-48 sm:shrink-0"
        :style="{ animationDelay: `${i * 40}ms` }"
      >
        <!-- A lane header that reads as a column, not floating text. -->
        <div
          :class="[
            'flex items-baseline gap-2 border-b-2 px-1 pb-2',
            lane === 'done' ? 'border-[var(--status-good)]/50' : 'border-primary/45',
          ]"
        >
          <span class="text-xs font-semibold tracking-wide">{{ lane }}</span>
          <span class="tabular text-muted-foreground ml-auto text-[11px]">
            {{ byLane.get(lane)?.length ?? 0 }}
          </span>
        </div>

        <div class="flex flex-col gap-2 pt-2">
          <!-- A wrapper, not a button, because a card that is waiting carries
               its own action and a button cannot nest inside a button. The body
               is still the whole clickable surface. -->
          <div
            v-for="task in byLane.get(lane)"
            :key="task.id"
            :class="[
              'bg-card hover:border-primary/40 border transition-colors',
              task.state === 'working' && 'border-primary/50 bg-primary/[0.06]',
              needsAttention?.includes(task.id) &&
                'border-[var(--status-warning)]/60 bg-[var(--status-warning)]/[0.06]',
              // Visible only because the switch is on. Dimmed so the board
              // still reads as the work you did not put away.
              task.hidden && 'opacity-55',
            ]"
          >
          <button
            type="button"
            class="focus-visible:outline-ring w-full p-2.5 text-left focus-visible:outline-2 focus-visible:-outline-offset-2"
            @click="emit('open', task)"
          >
            <div class="mb-1.5 text-xs leading-snug font-medium break-words">{{ task.name }}</div>

            <!-- What is happening right now. "working" for four minutes is
                 indistinguishable from stuck; the tool it just ran is not. -->
            <!-- Three lines, not one. A tool call fits in one; the line an
                 agent writes about what it just decided rarely does, and a
                 single line ending in an ellipsis is the half of a sentence
                 that carries the least. Clamped rather than unbounded, so one
                 verbose card cannot push the rest of the lane off screen. -->
            <p
              v-if="task.doing"
              class="text-muted-foreground mb-1.5 line-clamp-3 font-mono text-[10px] leading-snug break-words"
              :title="task.doing"
            >
              {{ task.doing }}
            </p>
            <div class="flex flex-wrap items-center gap-1.5">
              <!-- lane says who holds the card, state says whether they are
                   actually working it. Showing only the lane makes a card read
                   as claimed the instant it is delivered. -->
              <Badge
                :variant="
                  task.state === 'working'
                    ? 'default'
                    : task.stoppedAt
                      ? 'secondary'
                      : 'outline'
                "
                class="gap-1"
              >
                <!-- The same pulse a live role wears in the rail, so the board
                     has one vocabulary for "this is moving" rather than a
                     second animation that has to be learned. A still badge and
                     a working one otherwise differ only in fill, which is a
                     colour difference and not something you catch in passing. -->
                <span v-if="task.state === 'working'" class="pulse-dot size-1.5 rounded-full bg-current" />
                {{ taskState(task) }}
              </Badge>
              <!-- The card that is holding everything up says so on itself.
                   A count in the header tells you something is waiting; this
                   tells you which one. -->
              <Badge
                v-if="needsAttention?.includes(task.id)"
                variant="secondary"
                class="gap-1 text-[var(--status-warning)]"
                :title="blockedOn?.get(task.id) === 'question'
                  ? 'an agent asked you something'
                  : 'waiting on your decision'"
              >
                <component
                  :is="blockedOn?.get(task.id) === 'question' ? MessageCircleQuestion : Bell"
                  :size="10"
                  aria-hidden="true"
                />
                {{ blockedOn?.get(task.id) === 'question' ? 'answer' : 'approve' }}
              </Badge>
              <Badge v-if="task.reworkCount > 0" variant="secondary" :title="`sent backward ${task.reworkCount} times`">
                ↩ {{ task.reworkCount }}
              </Badge>
              <span v-if="task.activeMs > 0" class="tabular text-muted-foreground text-[10px]">
                {{ duration(task.activeMs) }}
              </span>

              <!-- Card controls. On the badge row rather than a footer of their
                   own: a card is a dense object and a second row of chrome on
                   every one of them costs more than it gives.
                   Buttons rather than the wrapper's click, so they do not open
                   the card as well — and .stop on each, because the whole card
                   body is clickable. -->
              <span class="ml-auto flex items-center gap-0.5">
                <button
                  v-if="task.state === 'queued' || task.state === 'working'"
                  type="button"
                  title="Stop — no agent picks this up again"
                  aria-label="Stop this task"
                  class="text-muted-foreground hover:bg-muted hover:text-destructive focus-visible:outline-ring grid size-5 place-items-center transition-colors focus-visible:outline-2"
                  @click.stop="emit('stop', task)"
                >
                  <Square :size="11" aria-hidden="true" />
                </button>
                <button
                  type="button"
                  title="Activity for this task"
                  aria-label="Show this task's activity"
                  class="text-muted-foreground hover:bg-muted hover:text-foreground focus-visible:outline-ring grid size-5 place-items-center transition-colors focus-visible:outline-2"
                  @click.stop="emit('activity', task)"
                >
                  <ScrollText :size="11" aria-hidden="true" />
                </button>
                <button
                  type="button"
                  title="Delete this task and its transcript"
                  aria-label="Delete this task"
                  class="text-muted-foreground hover:bg-destructive/10 hover:text-destructive focus-visible:outline-ring grid size-5 place-items-center transition-colors focus-visible:outline-2"
                  @click.stop="emit('remove', task)"
                >
                  <Trash2 :size="11" aria-hidden="true" />
                </button>
              </span>
            </div>

            <!-- When, and what it cost. Both were only discoverable by opening
                 the card, which is the wrong place for the number that tells
                 you whether to open it. -->
            <div
              v-if="task.tokens || task.completedAt || task.firstClaimedAt"
              class="text-muted-foreground mt-1.5 flex flex-wrap items-center gap-x-2 text-[10px]"
            >
              <span v-if="task.state === 'done' && task.completedAt">
                done {{ ago(task.completedAt) }}
              </span>
              <span v-else-if="task.firstClaimedAt">started {{ ago(task.firstClaimedAt) }}</span>
              <span v-else>queued {{ ago(task.createdAt) }}</span>

              <span v-if="task.tokens" class="tabular ml-auto">
                {{ compactTokens(task.tokens) }} · {{ money(task.costUsd) }}
              </span>
            </div>
          </button>

            <!-- Put away, on the card itself. Finished work accumulates, and
                 the person reading a card is the one who knows whether they
                 will want it again — which no age cutoff can guess. -->
            <button
              v-if="task.state === 'done'"
              type="button"
              class="hairline-t text-muted-foreground hover:bg-muted hover:text-foreground focus-visible:outline-ring flex w-full items-center gap-1.5 px-2.5 py-1.5 text-left text-[11px] focus-visible:outline-2 focus-visible:-outline-offset-2"
              @click="task.hidden ? emit('unhide', task) : emit('hide', task)"
            >
              <component :is="task.hidden ? Eye : EyeOff" :size="12" aria-hidden="true" />
              {{ task.hidden ? 'Unhide' : 'Hide' }}
            </button>

            <!-- The action the card is waiting for, on the card. Reaching a
                 decision through a notification means finding the card again
                 afterwards; the card that is blocked is where the decision
                 belongs. -->
            <button
              v-if="needsAttention?.includes(task.id)"
              type="button"
              class="hairline-t hover:bg-[var(--status-warning)]/10 focus-visible:outline-ring flex w-full items-center gap-1.5 px-2.5 py-1.5 text-left text-[11px] font-medium text-[var(--status-warning)] focus-visible:outline-2 focus-visible:-outline-offset-2"
              @click="emit('review', task)"
            >
              <component
                :is="blockedOn?.get(task.id) === 'question' ? MessageCircleQuestion : Bell"
                :size="12"
                aria-hidden="true"
              />
              {{ blockedOn?.get(task.id) === 'question' ? 'Answer the question' : 'Review and decide' }}
            </button>
          </div>

          <!-- An empty lane is normal; it should read as quiet, not broken. -->
          <p
            v-if="!byLane.get(lane)?.length"
            class="text-muted-foreground/50 px-1 py-3 text-[11px]"
          >
            empty
          </p>
        </div>
      </section>
    </div>
  </div>
</template>

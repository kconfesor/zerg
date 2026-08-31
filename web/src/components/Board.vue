<script setup lang="ts">
import { computed } from 'vue'
import type { LiveService, ResolvedRole, SwarmStatus, Task } from '@/lib/api'
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
import {
  Bell,
  Eye,
  EyeOff,
  ExternalLink,
  Hourglass,
  LoaderCircle,
  MessageCircleQuestion,
  ScrollText,
  Square,
  Trash2,
} from '@lucide/vue'
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
  /** Apps running right now, so the card that asked for one can link to it. */
  services?: LiveService[]
  /** A deploy in flight, for the card that is waiting on it. */
  deploy?: SwarmStatus['deploy']
  /** Roles that are mid-turn and have gone quiet, keyed by role name. */
  quiet?: Map<string, number>
}>()
const emit = defineEmits<{
  open: [task: Task]
  review: [task: Task]
  hide: [task: Task]
  unhide: [task: Task]
  stop: [task: Task]
  activity: [task: Task]
  remove: [task: Task]
  stopDeploy: [task: Task]
}>()

/**
 * What a card's deployment is doing, if it has one.
 *
 * On the card that asked for it. It was in the top bar, which is the wrong
 * place twice over: it says a thing is running without saying what produced
 * it, and it is nowhere near the card whose change you want to look at.
 */
const deployFor = computed(() => (task: Task) => {
  const live = (props.services ?? []).find((s) => s.taskId === task.id)
  if (live) return { state: 'serving' as const, url: live.url, label: live.label }
  const d = props.deploy
  if (d && d.taskId === task.id && d.state !== 'idle') {
    return { state: d.state, url: '', label: '', message: d.message }
  }
  return null
})

/**
 * The models that did the work, short enough to sit on a card.
 *
 * "claude-sonnet-5" and "gpt-5.6-sol" are the identifiers, and the vendor
 * prefix is the least interesting part of them on a board where every card
 * carries one. The full names are in the title, since the short form is
 * ambiguous the moment two vendors ship a "5".
 */
function shortModel(model: string): string {
  return model.replace(/^(claude|openai|anthropic|google)-/, '')
}

/**
 * The model a role is configured with, for its column heading.
 *
 * A different question from the models on a card, which say what actually did
 * that work and are recorded with it. This one is live: change the team and
 * every heading changes, because it describes what the next card through this
 * lane will be worked by.
 *
 * The last path segment, since a harness that namespaces its models
 * ("openai-codex/gpt-5.6-sol") puts the interesting half at the end and a
 * column is 192 pixels wide. The full value is in the title.
 */
function laneModel(lane: string): string {
  const role = props.team.find((r) => r.enabled && r.name === lane)
  if (!role?.model) return ''
  return shortModel(role.model.split('/').pop() ?? role.model)
}

/**
 * How long the role working this card has been silent, in words.
 *
 * A card that says "working" says the same thing whether the agent is running
 * a long test suite or sitting in a command that will never return. One of
 * those wants patience and the other wants a person, and until this there was
 * nothing on screen that told them apart -- a wedged agent was found because
 * somebody happened to be watching.
 */
function quietFor(task: Task): string {
  const seconds = props.quiet?.get(task.lane)
  if (!seconds || task.state !== 'working') return ''
  const mins = Math.round(seconds / 60)
  return `quiet ${mins}m`
}

/** What the strip says, which is different in each of the three states. */
function deploySays(state: string): string {
  if (state === 'serving') return 'Deployment done'
  if (state === 'gave up') return 'Deployment failed'
  if (state === 'asking') return 'Deployment needs an answer'
  return 'Deploying'
}

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
  <!-- One box, both axes. The lanes scroll inside it rather than the page
       scrolling under them, which is what lets a lane heading stay put: sticky
       positions against the nearest scrolling ancestor, and this is it.
       It also gives the horizontal bar somewhere fixed to live — with every
       card hidden the board used to collapse to a strip 83px tall, and the
       scrollbar with it. -->
  <div
    class="-mx-[var(--gutter)] min-h-0 flex-1 overflow-x-auto overflow-y-auto px-[var(--gutter)] pb-2 sm:pr-0"
  >
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
    <!-- items-stretch, so every lane is as tall as the tallest and its heading
         stays put for the whole scroll. Sized to their own content, the short
         lanes' headings scrolled away while the busy lane's stayed, which reads
         as the headings being broken rather than as the lanes being empty. -->
    <div class="flex flex-wrap items-stretch gap-3 sm:flex-nowrap">
      <section
        v-for="(lane, i) in lanes"
        :key="lane"
        class="rise flex min-w-0 flex-1 basis-full flex-col sm:min-w-44 sm:max-w-96 sm:basis-48 sm:shrink-0"
        :style="{ animationDelay: `${i * 40}ms` }"
      >
        <!-- A lane header that reads as a column, not floating text.
             Sticky, because a board tall enough to scroll is a board where the
             names have left the screen: a card halfway down belongs to a role
             you can no longer see. The background is opaque for the same
             reason a sticky header always is — cards pass underneath it. -->
        <div
          :class="[
            'bg-background sticky top-0 z-10 flex items-baseline gap-2 border-b-2 px-1 pt-1 pb-2',
            lane === 'done' ? 'border-[var(--status-good)]/50' : 'border-primary/45',
          ]"
        >
          <span class="shrink-0 text-xs font-semibold tracking-wide">{{ lane }}</span>
          <!-- What will work the next card through here. It was a trip to the
               team editor to answer, which is a screen away from the board it
               is about. Allowed to truncate rather than to push the count off
               the end: the count is the fact you scan for, and the full model
               is in the title. -->
          <span
            v-if="laneModel(lane)"
            class="text-muted-foreground min-w-0 truncate text-[10px]"
            :title="`${lane} runs ${team.find((r) => r.name === lane)?.model}`"
          >
            · {{ laneModel(lane) }}
          </span>
          <span class="tabular text-muted-foreground ml-auto shrink-0 pl-1 text-[11px]">
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
                  title="Stop, and no agent picks this up again"
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

              <span
                v-if="quietFor(task)"
                class="flex items-center gap-1 text-[var(--status-warning)]"
                title="This agent is mid-turn and has produced nothing for a while. A long build looks like this; so does a command that will never return."
              >
                <Hourglass :size="10" aria-hidden="true" />
                {{ quietFor(task) }}
              </span>

              <!-- Which models produced this. On the card because it is the
                   card you are judging: "this came out well" and "this came
                   out badly" are both worth attaching to what made it, and a
                   role's configured model is a live value that will not
                   remember. -->
              <span
                v-if="task.models?.length"
                class="truncate"
                :title="`Worked by ${task.models.join(', ')}`"
              >
                {{ task.models.map(shortModel).join(' · ') }}
              </span>
            </div>
          </button>

            <!-- The card's foot: putting it away, and what it cost.
                 Together on one row because both are about the card as a whole
                 rather than about the work in it, and the spend was taking a
                 third of the line above while the row underneath held one
                 word. Put away is on the card itself because finished work
                 accumulates, and the person reading a card is the one who
                 knows whether they will want it again -- which no age cutoff
                 can guess. -->
            <div
              v-if="task.state === 'done' || task.tokens"
              class="hairline-t text-muted-foreground flex items-center text-[11px]"
            >
              <button
                v-if="task.state === 'done'"
                type="button"
                class="hover:bg-muted hover:text-foreground focus-visible:outline-ring flex items-center gap-1.5 px-2.5 py-1.5 text-left transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2"
                @click="task.hidden ? emit('unhide', task) : emit('hide', task)"
              >
                <component :is="task.hidden ? Eye : EyeOff" :size="12" aria-hidden="true" />
                {{ task.hidden ? 'Unhide' : 'Hide' }}
              </button>

              <span
                v-if="task.tokens"
                class="tabular ml-auto px-2.5 py-1.5 text-[10px]"
                :title="`${task.tokens.toLocaleString()} tokens across every role and every lap`"
              >
                {{ compactTokens(task.tokens) }} · {{ money(task.costUsd) }}
              </span>
            </div>

            <!-- What this card's change is doing when it is running somewhere.
                 On the card rather than in the top bar: an app running is only
                 meaningful next to the change that produced it, and this is
                 where somebody is already looking when they want to see it. -->
            <div
              v-if="deployFor(task)"
              class="hairline-t flex items-center gap-1.5 px-2.5 py-1.5 text-[11px]"
              :class="
                deployFor(task)!.state === 'gave up'
                  ? 'text-[var(--status-warning)]'
                  : 'text-muted-foreground'
              "
            >
              <span
                v-if="deployFor(task)!.state === 'serving'"
                class="pulse-dot size-1.5 shrink-0 rounded-full bg-[var(--status-good)]"
              />
              <LoaderCircle
                v-else-if="deployFor(task)!.state === 'working'"
                :size="11"
                aria-hidden="true"
                class="spin shrink-0"
              />
              <span class="truncate">{{ deploySays(deployFor(task)!.state) }}</span>

              <a
                v-if="deployFor(task)!.state === 'serving'"
                :href="deployFor(task)!.url"
                target="_blank"
                rel="noopener noreferrer"
                class="text-primary hover:bg-muted focus-visible:outline-ring ml-auto grid size-5 shrink-0 place-items-center transition-colors focus-visible:outline-2"
                :title="`Open ${deployFor(task)!.label || 'it'} in a new tab`"
                :aria-label="`Open ${deployFor(task)!.label || 'this deployment'} in a new tab`"
                @click.stop
              >
                <ExternalLink :size="12" aria-hidden="true" />
              </a>

              <button
                type="button"
                class="text-muted-foreground hover:bg-muted hover:text-foreground focus-visible:outline-ring grid size-5 shrink-0 place-items-center transition-colors focus-visible:outline-2"
                :class="deployFor(task)!.state === 'serving' ? '' : 'ml-auto'"
                title="Stop this deployment"
                aria-label="Stop this deployment"
                @click.stop="emit('stopDeploy', task)"
              >
                <Square :size="11" aria-hidden="true" />
              </button>
            </div>

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
            class="text-muted-foreground px-1 py-3 text-[11px]"
          >
            empty
          </p>
        </div>
      </section>
      <!-- The right-hand gutter, as a real element.
           Engines do not agree on what a scroll container's overflow area
           contains. At full scroll the last lane sat flush against the window,
           and the reason differs per browser: Chrome counts the box's
           padding-inline-end, Firefox does not; neither counts a margin on the
           overflowing lane, and Firefox ignores one made with a negative
           margin too. An element is the one thing both count, measured in both.

           Sized to the gutter minus the row's gap, because the gap before it
           is already part of the space you see, and stretched to the row's
           height: an 8x0 box is counted by neither engine. Its twin below sm is the box's
           own padding, which is why that is dropped only past sm. -->
      <span
        class="hidden shrink-0 basis-[calc(var(--gutter)_-_0.75rem)] self-stretch sm:block"
        aria-hidden="true"
      />
    </div>
  </div>
</template>

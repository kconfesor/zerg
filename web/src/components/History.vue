<script setup lang="ts">
/**
 * What this project has worked on, newest first.
 *
 * The board answers "what is happening"; nothing answered "what happened".
 * A finished card went to Done, was put away, and took its account with it:
 * what it cost, how long it actually ran against how long it sat waiting, how
 * many times it went round, and where the work ended up.
 *
 * It reads its own pages rather than taking them from the shell. The board
 * polls every two seconds and history does not move underneath you; sharing a
 * refresh would give one of them the wrong behaviour.
 */
import { computed, ref, watch } from 'vue'
import { GitBranch, GitMerge, GitPullRequest, Pin, RotateCcw, Search } from '@lucide/vue'
import { api, type HistoryEntry, type Task } from '@/lib/api'
import { latest } from '@/lib/latest'
import { duration, money, taskState } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

const props = defineProps<{
  projectId: string
  /** The roles on this project's team, for the filter. History can hold roles
   *  a team no longer has, so the list is what is offered, not what exists. */
  roles: string[]
}>()
const emit = defineEmits<{ open: [task: Task] }>()

const entries = ref<HistoryEntry[]>([])
const next = ref('')
const loading = ref(false)
const error = ref('')
const outcome = ref('all')
const role = ref('all')
const query = ref('')

const ANY = 'all'

/**
 * Only the newest request may write.
 *
 * A filter change, a project switch and a page of older work are three requests
 * that can be in flight together, and nothing cancels the ones already gone.
 * A slow first answer landing last put an unfiltered list under a filter, or
 * appended a page of one project's work to another's.
 */
const newest = latest()

async function load(append = false) {
  if (!props.projectId) return
  const current = newest()
  loading.value = true
  error.value = ''
  try {
    const page = await api.history(props.projectId, {
      before: append ? next.value : '',
      outcome: outcome.value === ANY ? '' : outcome.value,
      role: role.value === ANY ? '' : role.value,
      q: query.value.trim(),
    })
    if (!current()) return // a newer request has been asked for
    entries.value = append ? [...entries.value, ...page.entries] : page.entries
    next.value = page.next
  } catch (e) {
    if (!current()) return
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (current()) loading.value = false
  }
}

// A filter change starts a new list rather than appending to the old one, and
// typing waits for a pause: a request per keystroke against a table this size
// is a request per keystroke nobody reads the answer to.
let typing: ReturnType<typeof setTimeout> | undefined
watch([() => props.projectId, outcome, role], () => load())
watch(query, () => {
  clearTimeout(typing)
  typing = setTimeout(() => load(), 250)
})
load()

/**
 * When a card stopped being a card: finished, or parked by a person.
 *
 * A stopped card has no completedAt, so measuring to "now" made its wall time
 * grow for as long as the database held it, and the date beside it read as when
 * it was created rather than when it was stopped.
 */
function ended(task: HistoryEntry): string | undefined {
  return task.completedAt ?? task.stoppedAt
}

/** Wall time is what a person waited; active is what the agents ran for. */
function wall(task: HistoryEntry): number {
  const end = ended(task)
  const at = end ? new Date(end) : new Date()
  return Math.max(0, at.getTime() - new Date(task.createdAt).getTime())
}

/**
 * How a card ended, in a word.
 *
 * Not the same question as its state. A card can be done and have landed
 * nothing, because the project leaves work on its branch; and a card that
 * ended before the daemon recorded outcomes has none to show, which is said
 * rather than guessed.
 */
function ending(task: HistoryEntry): { label: string; icon: unknown; tone: string } | null {
  if (task.outcome === 'merged') return { label: 'merged', icon: GitMerge, tone: 'text-[var(--status-good)]' }
  if (task.outcome === 'pr') return { label: 'pull request', icon: GitPullRequest, tone: 'text-[var(--primary)]' }
  if (task.outcome === 'branch') return { label: 'on its branch', icon: GitBranch, tone: 'text-muted-foreground' }
  return null
}

/**
 * Keeping a card's transcript, or handing it back to the sweep.
 *
 * Events are the expensive tier and are swept past the retention window, so the
 * card somebody will want to read in six months has to be marked before then.
 * The row updates in place rather than reloading the page: a list that jumps
 * back to the top when you pin something is a list you cannot pin twice.
 */
async function pin(task: HistoryEntry) {
  try {
    const updated = await api.setTaskPinned(task.id, !task.pinned)
    entries.value = entries.value.map((e) => (e.id === task.id ? { ...e, pinned: updated.pinned } : e))
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

const empty = computed(() => !loading.value && !entries.value.length)
const filtered = computed(() => outcome.value !== ANY || role.value !== ANY || !!query.value.trim())
</script>

<template>
  <div class="flex flex-col gap-3">
    <div class="flex flex-wrap items-center gap-2">
      <div class="relative min-w-40 flex-1">
        <Search
          :size="13"
          class="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2"
          aria-hidden="true"
        />
        <Input v-model="query" placeholder="Search by name" class="h-8 pl-7 text-xs" aria-label="Search history" />
      </div>
      <Select v-model="outcome">
        <SelectTrigger class="h-8 w-40 text-xs" aria-label="Filter by outcome">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="all">Any outcome</SelectItem>
          <SelectItem value="merged">Merged</SelectItem>
          <SelectItem value="pr">Pull request</SelectItem>
          <SelectItem value="branch">Left on a branch</SelectItem>
          <SelectItem value="none">Nothing recorded</SelectItem>
        </SelectContent>
      </Select>
      <Select v-if="roles.length" v-model="role">
        <SelectTrigger class="h-8 w-36 text-xs" aria-label="Filter by role">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="all">Any role</SelectItem>
          <SelectItem v-for="r in roles" :key="r" :value="r">{{ r }}</SelectItem>
        </SelectContent>
      </Select>
    </div>

    <p v-if="error" class="text-destructive text-xs">{{ error }}</p>

    <div class="border bg-card">
      <ul class="divide-y">
        <li v-for="task in entries" :key="task.id" class="relative">
          <button
            type="button"
            class="hover:bg-muted/50 focus-visible:outline-ring flex w-full flex-col gap-1.5 py-3 pr-24 pl-4 text-left focus-visible:outline-2 focus-visible:-outline-offset-2"
            @click="emit('open', task)"
          >
            <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
              <span class="truncate text-sm font-medium">{{ task.name }}</span>
              <!-- What became of the work, which is the question a lane called
                   Done cannot answer. -->
              <span
                v-if="ending(task)"
                :class="['flex items-center gap-1 text-[11px]', ending(task)!.tone]"
              >
                <component :is="ending(task)!.icon" :size="11" aria-hidden="true" />
                {{ ending(task)!.label }}
              </span>
              <Badge v-else-if="task.state !== 'done'" variant="outline" class="text-[10px]">
                {{ taskState(task) }}
              </Badge>
              <!-- Laps are legitimate; a lot of them is a loop nobody watched. -->
              <span
                v-if="task.reworkCount"
                class="text-muted-foreground flex items-center gap-1 text-[11px]"
                :title="`Went back through the pipeline ${task.reworkCount} time${task.reworkCount === 1 ? '' : 's'}`"
              >
                <RotateCcw :size="10" aria-hidden="true" />
                {{ task.reworkCount }}
              </span>
              <span v-if="task.hidden" class="text-muted-foreground/70 text-[10px]">put away</span>
              <!-- Said rather than discovered on opening it: the transcript is
                   the tier that ages out, and a card that has lost it still has
                   its outcome, its cost and its trail. -->
              <span
                v-if="!task.hasTranscript"
                class="text-muted-foreground/60 text-[10px]"
                title="Its events have been swept. The trail, the cost and the outcome are still here."
              >
                transcript aged out
              </span>
            </div>

            <div class="text-muted-foreground flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px]">
              <span :title="ended(task) ?? task.createdAt">
                {{ new Date(ended(task) ?? task.createdAt).toLocaleString() }}
              </span>
              <!-- The gap between these two is the reading: six hours of wall
                   time against twelve minutes of work is a card that was
                   waiting, not a task that was hard. -->
              <span class="tabular">
                {{ duration(wall(task)) }} wall
                <span class="text-muted-foreground/60">/</span>
                {{ duration(task.activeMs) }} working
              </span>
              <span class="tabular">{{ money(task.costUsd) }}</span>
              <span v-if="task.roles.length" class="truncate">{{ task.roles.join(' → ') }}</span>
            </div>
          </button>
          <!-- Over the row rather than inside it: a button inside a button is
               not a thing a browser will render, and the row is the click
               target for reading the card. The row's right padding is what
               keeps a long name from running underneath. -->
          <div class="absolute top-2 right-2">
            <Button
              size="xs"
              variant="ghost"
              class="h-6 gap-1 px-1.5 text-[10px]"
              :class="task.pinned ? 'text-[var(--primary)]' : 'text-muted-foreground'"
              :aria-label="task.pinned ? `Stop keeping ${task.name}'s transcript` : `Keep ${task.name}'s transcript`"
              :title="
                task.pinned
                  ? 'Kept past the retention window. Press to let the sweep have it.'
                  : 'Keep this transcript past the retention window.'
              "
              @click="pin(task)"
            >
              <!-- One icon, coloured. A crossed-out pin on the row you have
                   not pinned reads as "cannot be pinned". -->
              <Pin :size="11" aria-hidden="true" />
              {{ task.pinned ? 'kept' : 'keep' }}
            </Button>
          </div>
        </li>
      </ul>

      <p v-if="empty" class="text-muted-foreground px-4 py-10 text-center text-xs">
        {{ filtered ? 'No task matches that.' : 'Nothing has been worked on in this project yet.' }}
      </p>

      <div v-if="next" class="border-t p-2 text-center">
        <Button size="sm" variant="ghost" :disabled="loading" @click="load(true)">
          {{ loading ? 'Loading' : 'Older' }}
        </Button>
      </div>
    </div>
  </div>
</template>

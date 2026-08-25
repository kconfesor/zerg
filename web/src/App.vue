<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import {
  ApiError,
  api,
  type Attention as AttentionData,
  type Model,
  type Project,
  type ProjectRole,
  type Readiness,
  type ResolvedRole,
  type RoleTemplate,
  type SwarmStatus,
  type Task,
} from '@/lib/api'
import Attention from '@/components/Attention.vue'
import Activity from '@/components/Activity.vue'
import Board from '@/components/Board.vue'
import ReadinessPanel from '@/components/Readiness.vue'
import TeamEditor from '@/components/TeamEditor.vue'
import AppSidebar, { type View } from '@/components/layout/AppSidebar.vue'
import ProjectBar from '@/components/layout/ProjectBar.vue'
import PageHeader from '@/components/layout/PageHeader.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

const projects = ref<Project[]>([])
const current = ref<Project | null>(null)
const view = ref<View>('board')

/** The nav drawer, which only exists below md. */
const navOpen = ref(false)

/**
 * Board polling, with the same backoff discipline the event stream has.
 *
 * A fixed interval keeps firing into a daemon that is not there: restarting it
 * produced a dozen failed requests in ten seconds while the socket, which backs
 * off, made three. The queue is durable and the stream carries anything urgent,
 * so a slow retry costs a slightly stale screen and nothing else.
 *
 * self-scheduling rather than setInterval, because an interval cannot change
 * its own period and would also stack requests if one ever outlived a tick.
 */
const POLL_BASE = 2000
const POLL_MAX = 30_000
let pollDelay = POLL_BASE

/** Board ticks between usage refreshes. At a 2s board poll this is ~10s. */
const USAGE_EVERY = 5
const usageKey = ref(0)
let usageTicks = 0

const library = ref<RoleTemplate[]>([])
const team = ref<ResolvedRole[]>([])
const tasks = ref<Task[]>([])
const attention = ref<AttentionData | null>(null)
const readiness = ref<Readiness | null>(null)
const status = ref<SwarmStatus>({ running: false, roles: [] })
const harnesses = ref<string[]>([])
const models = ref<Record<string, Model[]>>({})

/**
 * `transient` marks a message the background poller raised rather than one a
 * click produced. The two must clear differently: a failed Start has to stay
 * until it is read, while a connection blip must disappear the moment contact
 * is back. Without the distinction, restarting the daemon left "Failed to
 * fetch" on screen permanently — a fully recovered system that reads as broken,
 * which is the exact failure this project exists to avoid.
 */
const banner = ref<{ tone: 'bad' | 'ok'; text: string; transient?: boolean } | null>(null)
const newPath = ref('')
const taskName = ref('')
const taskBody = ref('')
const composing = ref(false)
const addingProject = ref(false)

let timer: number | undefined

const attentionCount = computed(() => {
  const a = attention.value
  if (!a) return 0
  return a.approvals.length + a.clarifications.length + a.rework.tasks.length
})

const working = computed(() => tasks.value.filter((t) => t.state === 'working').length)

const boardSubtitle = computed(() => {
  const n = tasks.value.length
  if (!n) return 'No cards yet. Open one to give the agents something to do.'
  return `${n} ${n === 1 ? 'card' : 'cards'} · ${working.value} being worked`
})

function schedulePoll(delay: number) {
  window.clearTimeout(timer)
  timer = window.setTimeout(async () => {
    await refresh()
    // Jitter so several open tabs do not land on the same tick.
    schedulePoll(pollDelay + Math.random() * pollDelay * 0.2)
  }, delay)
}

function fail(err: unknown) {
  banner.value = { tone: 'bad', text: err instanceof Error ? err.message : String(err) }
}

/**
 * The poller lost contact. "Failed to fetch" is the browser's words for it and
 * says nothing useful, so this says what happened and what is being done —
 * usually the daemon is restarting, and it will clear itself.
 */
function pollerLostContact() {
  if (banner.value && !banner.value.transient) return // do not bury a real error
  banner.value = {
    tone: 'bad',
    transient: true,
    text: 'Lost contact with the daemon. Retrying — this clears itself when it is back.',
  }
}

async function loadGlobals() {
  try {
    ;[projects.value, library.value, harnesses.value] = await Promise.all([
      api.projects(),
      api.roles(),
      api.harnesses(),
    ])
    for (const h of harnesses.value) models.value[h] = await api.models(h).catch(() => [])
  } catch (err) {
    fail(err)
  }
}

/** Refreshes everything scoped to the open project. */
async function refresh() {
  const project = current.value
  if (!project) return
  try {
    const [t, tk, at, st] = await Promise.all([
      api.team(project.id),
      api.tasks(project.id),
      api.attention(project.id),
      api.status(project.id),
    ])
    team.value = t
    tasks.value = tk
    attention.value = at
    status.value = st

    // Contact is back, so a message about losing it has to go. Only the
    // transient one: an error the user's own click produced is still theirs
    // to read.
    if (banner.value?.transient) banner.value = null
    pollDelay = POLL_BASE

    // Usage moves only when an agent finishes a turn, and it is a summary
    // rather than a live counter, so it is refreshed on a slower cadence than
    // the board — a totals query every board tick would be mostly wasted.
    if (++usageTicks % USAGE_EVERY === 0) usageKey.value++
  } catch {
    pollerLostContact()
    pollDelay = Math.min(pollDelay * 2, POLL_MAX)
  }
}

async function open(project: Project) {
  current.value = project
  view.value = 'board'
  readiness.value = null
  await refresh()
}

async function addProject() {
  if (!newPath.value.trim()) return
  try {
    const p = await api.createProject(newPath.value.trim(), 'main')
    newPath.value = ''
    addingProject.value = false
    await loadGlobals()
    await open(p)
  } catch (err) {
    fail(err)
  }
}

async function checkReadiness() {
  if (!current.value) return
  try {
    readiness.value = await api.readiness(current.value.id)
    view.value = 'readiness'
  } catch (err) {
    fail(err)
  }
}

async function start() {
  if (!current.value) return
  banner.value = null
  try {
    status.value = await api.start(current.value.id)
    banner.value = { tone: 'ok', text: 'Swarm started.' }
  } catch (err) {
    // A refused start carries the readiness report, so show which role failed
    // which check rather than only that something was wrong.
    if (err instanceof ApiError && err.status === 409) {
      const body = err.body as { readiness?: Readiness } | undefined
      if (body?.readiness) {
        readiness.value = body.readiness
        view.value = 'readiness'
      }
    }
    fail(err)
  }
  await refresh()
}

async function stop() {
  if (!current.value) return
  try {
    await api.stop(current.value.id)
    banner.value = { tone: 'ok', text: 'Swarm stopped.' }
  } catch (err) {
    fail(err)
  }
  await refresh()
}

async function createTask() {
  if (!current.value || !taskName.value.trim()) return
  try {
    await api.newTask(current.value.id, taskName.value.trim(), taskBody.value)
    taskName.value = ''
    taskBody.value = ''
    composing.value = false
    await refresh()
  } catch (err) {
    fail(err)
  }
}

async function setTeam(roles: ProjectRole[]) {
  if (!current.value) return
  try {
    team.value = await api.setTeam(current.value.id, roles)
  } catch (err) {
    fail(err)
  }
}

async function saveRole(role: RoleTemplate) {
  try {
    await api.updateRole(role)
    await loadGlobals()
    await refresh()
    banner.value = {
      tone: 'ok',
      text: `Saved ${role.name}. Restart the role for it to take effect.`,
    }
  } catch (err) {
    fail(err)
  }
}

const act = {
  approve: async (id: string) => {
    try {
      await api.approve(id)
    } catch (err) {
      fail(err)
    }
    await refresh()
  },
  reject: async (id: string, note: string) => {
    try {
      await api.reject(id, note)
    } catch (err) {
      fail(err)
    }
    await refresh()
  },
  answer: async (id: string, answer: string) => {
    if (!answer.trim()) return
    try {
      await api.answer(id, answer)
    } catch (err) {
      fail(err)
    }
    await refresh()
  },
}

onMounted(async () => {
  await loadGlobals()
  if (projects.value.length) await open(projects.value[0])
  schedulePoll(0)
})
onUnmounted(() => window.clearTimeout(timer))
watch(current, () => (banner.value = null))
</script>

<template>
  <div class="flex h-full">
    <!-- A tap outside the drawer closes it, which is what every phone user
         tries first. Inert above md, where the drawer does not exist. -->
    <div
      v-if="navOpen"
      class="fixed inset-0 z-40 bg-black/50 md:hidden"
      aria-hidden="true"
      @click="navOpen = false"
    />

    <AppSidebar
      :view="view"
      :status="status"
      :attention-count="attentionCount"
      :task-count="tasks.length"
      :open="navOpen"
      @close="navOpen = false"
      @navigate="(v) => (view = v)"
    />

    <div class="flex min-w-0 flex-1 flex-col overflow-hidden">
      <ProjectBar
        :projects="projects"
        :current="current"
        :status="status"
        :usage-key="usageKey"
        @menu="navOpen = true"
        @open-project="open"
        @add-project="addingProject = true"
        @start="start"
        @stop="stop"
      />

      <p
        v-if="banner"
        :class="[
          'hairline-b shrink-0 px-[var(--gutter)] py-2 text-xs',
          banner.tone === 'bad'
            ? 'bg-destructive/10 text-destructive'
            : 'bg-[var(--status-good)]/10 text-[var(--status-good)]',
        ]"
      >
        {{ banner.text }}
      </p>

      <!-- Something waiting on a human follows you between views. A count in
           the sidebar is easy to walk past; this is not. -->
      <button
        v-if="attentionCount && view !== 'attention'"
        class="hairline-b flex shrink-0 items-center gap-2 bg-[var(--status-warning)]/10 px-[var(--gutter)] py-2 text-left text-xs text-[var(--status-warning)] transition-colors hover:bg-[var(--status-warning)]/15"
        @click="view = 'attention'"
      >
        <span class="pulse-dot size-1.5 rounded-full bg-current" />
        <span class="tabular font-semibold">{{ attentionCount }}</span>
        {{ attentionCount === 1 ? 'item needs' : 'items need' }} you
        <span class="ml-auto opacity-70">Review →</span>
      </button>

      <!-- No project yet: one job on screen, nothing else. -->
      <main v-if="!current" class="grid flex-1 place-items-center overflow-y-auto p-8">
        <div class="w-full max-w-md text-center">
          <h1 class="text-lg font-semibold tracking-tight">No project yet</h1>
          <p class="text-muted-foreground mt-1.5 mb-4 text-xs leading-relaxed">
            Point zerg at a git repository. It starts with coder → reviewer selected; everything
            else is a checkbox in Team.
          </p>
          <Button @click="addingProject = true">Add a project</Button>
        </div>
      </main>

      <main v-else class="min-h-0 flex-1 overflow-y-auto">
        <div class="w-full p-[var(--gutter)]">
          <!-- Board -->
          <template v-if="view === 'board'">
            <PageHeader title="Board" :subtitle="boardSubtitle">
              <template #actions>
                <Button @click="composing = true">New card</Button>
              </template>
            </PageHeader>
            <div class="pt-4"><Board :team="team" :tasks="tasks" /></div>
          </template>

          <!-- Activity -->
          <template v-else-if="view === 'activity'">
            <PageHeader
              title="Activity"
              subtitle="Every tool call, message and turn, as the agents emit them."
            />
            <div class="pt-4">
              <Activity
                :project-id="current?.id ?? ''"
                :roles="team.filter((r) => r.enabled).map((r) => r.name)"
              />
            </div>
          </template>

          <!-- Team -->
          <template v-else-if="view === 'team'">
            <PageHeader
              title="Team"
              subtitle="The library is shared by every project. The pipeline is this one's."
            />
            <div class="pt-4">
              <TeamEditor
                :library="library"
                :team="team"
                :harnesses="harnesses"
                :models="models"
                :running="status.running"
                @set-team="setTeam"
                @save-role="saveRole"
              />
            </div>
          </template>

          <!-- Attention -->
          <template v-else-if="view === 'attention'">
            <PageHeader
              title="Attention"
              :subtitle="
                attentionCount
                  ? `${attentionCount} waiting on a decision`
                  : 'Everything waiting on a human appears here'
              "
            />
            <div class="max-w-3xl pt-4">
              <Attention
                :attention="attention"
                @approve="act.approve"
                @reject="act.reject"
                @answer="act.answer"
              />
            </div>
          </template>

          <!-- Readiness -->
          <template v-else>
            <PageHeader
              title="Readiness"
              subtitle="A team that cannot work must not reach a running board."
            >
              <template #actions>
                <Button size="sm" variant="outline" @click="checkReadiness">Re-check</Button>
              </template>
            </PageHeader>
            <div class="max-w-4xl pt-4">
              <ReadinessPanel :readiness="readiness" />
              <p v-if="!readiness" class="text-muted-foreground py-10 text-center text-xs">
                Not checked yet. Press Re-check to probe every enabled role.
              </p>
            </div>
          </template>
        </div>
      </main>
    </div>

    <Dialog v-model:open="addingProject">
      <DialogContent class="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a project</DialogTitle>
          <DialogDescription>
            An absolute path to a git repository. zerg initialises one if the directory is not a
            repository yet.
          </DialogDescription>
        </DialogHeader>
        <div class="flex flex-col gap-1.5">
          <Label>Path</Label>
          <Input
            v-model="newPath"
            placeholder="/Users/you/source/your-repo"
            autofocus
            @keyup.enter="addProject"
          />
        </div>
        <DialogFooter>
          <Button variant="outline" @click="addingProject = false">Cancel</Button>
          <Button :disabled="!newPath.trim()" @click="addProject">Add</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <!-- Opening a card. The name is the thing that follows this work through
         the whole pipeline, which is worth saying where it is being typed. -->
    <Dialog v-model:open="composing">
      <DialogContent class="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>New card</DialogTitle>
          <DialogDescription>
            Queued for {{ team.find((r) => r.enabled)?.name ?? 'the first role' }}, the first role
            in the pipeline.
          </DialogDescription>
        </DialogHeader>

        <div class="flex flex-col gap-3">
          <div class="flex flex-col gap-1.5">
            <Label>Name</Label>
            <Input v-model="taskName" placeholder="Expression parser" autofocus />
            <span class="text-muted-foreground text-[11px]">
              Short and stable — it follows the card through every role.
            </span>
          </div>
          <div class="flex flex-col gap-1.5">
            <Label>What to do</Label>
            <Textarea v-model="taskBody" rows="5" placeholder="Describe the work." />
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" @click="composing = false">Cancel</Button>
          <Button :disabled="!taskName.trim()" @click="createTask">Open card</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>

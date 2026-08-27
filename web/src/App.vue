<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, useId, watch } from 'vue'
import {
  ApiError,
  api,
  type Attention as AttentionData,
  type Model,
  type Project,
  type ProjectTeam,
  type ProjectTeamUpdate,
  type Readiness,
  type ResolvedRole,
  type RoleTemplate,
  type TeamPreset,
  type TeamPresetRole,
  type SwarmStatus,
  type Task,
  type Workspace,
} from '@/lib/api'
import { followPreset } from '@/lib/team'
import Attention from '@/components/Attention.vue'
import Activity from '@/components/Activity.vue'
import Spend from '@/components/Spend.vue'
import Board from '@/components/Board.vue'
import Chat from '@/components/Chat.vue'
import MarkdownEditor from '@/components/MarkdownEditor.vue'
import Projects from '@/components/Projects.vue'
import ProjectPathField from '@/components/ProjectPathField.vue'
import TaskDetail from '@/components/TaskDetail.vue'
import Settings from '@/components/Settings.vue'
import ReadinessPanel from '@/components/Readiness.vue'
import TeamEditor from '@/components/TeamEditor.vue'
import AppSidebar from '@/components/layout/AppSidebar.vue'
import { useRoute, useRouter } from 'vue-router'
import { viewOf, viewPath, type View } from '@/router'
import { latest } from '@/lib/latest'
import { usePending } from '@/lib/pending'
import ProjectBar from '@/components/layout/ProjectBar.vue'
import PageHeader from '@/components/layout/PageHeader.vue'
import BoardHeader from '@/components/BoardHeader.vue'
import { Switch } from '@/components/ui/switch'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

// Every field's label points at the control it names. Without the pairing a
// screen reader reads an unlabelled text box, and clicking the word does not
// focus the field it sits above.
const pathId = useId()
const taskNameId = useId()
const taskBodyId = useId()

const projects = ref<Project[]>([])
const current = ref<Project | null>(null)

/**
 * What is waiting on the daemon. Every mutation on this screen is a round trip
 * with no optimistic update, so without this a control looks untouched while it
 * works and invites the second press that duplicates the card, or races the
 * start it just asked for.
 */
const busy = usePending()
/**
 * The current view is the route, not a ref. Anything that needs to change it
 * navigates, so the address bar, the back button and a reload all agree.
 */
const route = useRoute()
const router = useRouter()
const view = computed<View>(() => viewOf(route.name))

/** Navigate within the current project, keeping it in the path. */
function go(v: View) {
  router.push(viewPath(current.value?.id, v))
}

/** The nav drawer, which only exists below md. */
const navOpen = ref(false)

/** The task whose history is open, if any. */
const openTask = ref<Task | null>(null)

/** Whatever is waiting on a person, shown over the page rather than instead of
 *  it. It arrives while you are reading something else, and going to another
 *  screen and back loses your place twice. */
const attentionOpen = ref(false)

/**
 * Whether cards a person has put away are shown. Off by default — the whole
 * point of hiding one is not to see it again.
 *
 * This is the view preference, so localStorage. Which cards are hidden lives in
 * the database instead: the same board is read from a laptop and a phone, and
 * a card put away on one should be away on the other.
 */
const showHidden = ref(localStorage.getItem('zerg.showHidden') === 'true')
watch(showHidden, (v) => localStorage.setItem('zerg.showHidden', String(v)))

/**
 * What the worktrees occupy. Fetched on its own slow cadence: walking several
 * checkouts is real filesystem work, and the answer moves in megabytes over
 * minutes rather than with every board tick.
 */
const workspace = ref<Workspace | null>(null)

const newestWorkspace = latest()

async function loadWorkspace() {
  if (!current.value) return
  const isCurrent = newestWorkspace()
  const w = await api.workspace(current.value.id).catch(() => null)
  // Walking several checkouts is slow enough that a project switch mid-flight
  // is ordinary rather than a race worth calling unlikely.
  if (!isCurrent()) return
  workspace.value = w
}

const hiddenCount = computed(() => tasks.value.filter((t) => t.hidden).length)

/** Put a finished card away, or bring it back. */
/** Which task Activity is filtered to, if any. */
const activityTask = ref<Task | null>(null)
const confirmDeleteTask = ref<Task | null>(null)

/**
 * One task's activity, in a dialog.
 *
 * Navigating to the Activity view took you off the board to answer a question
 * about one card on it — and then you had to find your way back and find the
 * card again. A dialog keeps the board underneath, which is where you were.
 */
function showTaskActivity(task: Task) {
  activityTask.value = task
}

/** Park a card. Nothing picks it up again; its history stays. */
async function stopTask(task: Task) {
  await busy.run(`task:${task.id}`, async () => {
    try {
      await api.stopTask(task.id)
      await refresh()
    } catch (err) {
      fail(err)
    }
  })
}

async function removeTask(task: Task) {
  await busy.run(`task:${task.id}`, async () => {
    try {
      dialogError.value = ''
      await api.deleteTask(task.id)
      confirmDeleteTask.value = null
      if (activityTask.value?.id === task.id) activityTask.value = null
      await refresh()
    } catch (err) {
      failIn(err)
    }
  })
}

async function setHidden(task: Task, hidden: boolean) {
  await busy.run(`task:${task.id}`, async () => {
    try {
      const updated = await api.setTaskHidden(task.id, hidden)
      const i = tasks.value.findIndex((t) => t.id === task.id)
      if (i !== -1) tasks.value[i] = updated
    } catch (err) {
      // This had no error handling at all: the switch moved, the request
      // failed, and the card stayed put with nothing said — so the same card
      // read as hidden on this device and visible on the next one.
      fail(err)
      await refresh()
    }
  })
}

/** Which cards are the ones waiting, so the board can mark them rather than
 *  leaving you to work out which of five is holding the pipeline. */
/**
 * What each blocked card is blocked on.
 *
 * A question and a merge approval both stopped the pipeline, and the card said
 * "waiting" for both — so on a board with several tasks you could see that
 * something needed you and not what, or which one. They read differently and
 * are answered differently, so the card says which.
 */
type Blocker = 'question' | 'approval'

const attentionByTask = computed(() => {
  const a = attention.value
  const map = new Map<string, Blocker>()
  if (!a) return map
  // Approvals first, then questions: a question is the more specific ask, and
  // a card carrying both should send you to the one that needs words.
  for (const x of a.approvals) if (x.taskId) map.set(x.taskId, 'approval')
  for (const x of a.clarifications) if (x.taskId) map.set(x.taskId, 'question')
  return map
})

const attentionTaskIds = computed(() => [...attentionByTask.value.keys()])

/**
 * True until the first load settles.
 *
 * Without it the empty state renders while the projects request is still in
 * flight, so every start said "No project yet" and then replaced it with the
 * project — telling you something false, briefly, on every single page load.
 * An empty state is a claim about the world and must not be made before the
 * answer is back.
 */
const loading = ref(true)

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
const presets = ref<TeamPreset[]>([])
const projectTeam = ref<ProjectTeam>({ presetId: null, topologyOverride: true, roles: [] })
const team = ref<ResolvedRole[]>([])
const tasks = ref<Task[]>([])
const attention = ref<AttentionData | null>(null)
const readiness = ref<Readiness | null>(null)

/**
 * The readiness report, over whatever you were looking at.
 *
 * A dialog rather than a screen or a tab: it is something you run and read,
 * not a place. It also means a refused Start can put the report in front of the
 * person who pressed the button, wherever they pressed it, instead of
 * navigating them away from the board mid-thought.
 */
const readinessOpen = ref(false)

function showReadiness() {
  readinessOpen.value = true
}
const status = ref<SwarmStatus>({ running: false, roles: [] })
const harnesses = ref<string[]>([])
const models = ref<Record<string, Model[]>>({})
/** The team this project follows, when it follows one. */
const currentPreset = computed(
  () => presets.value.find((preset) => preset.id === projectTeam.value.presetId) ?? null,
)
const currentTeamName = computed(() => {
  const id = projectTeam.value.presetId
  if (!id) return projectTeam.value.topologyOverride ? 'Custom team' : ''
  return currentPreset.value?.name ?? ''
})

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

/**
 * Close the queue when the last thing in it is decided.
 *
 * On the transition to empty, not on being empty: deciding the final approval
 * should dismiss the dialog, but a dialog opened deliberately over an empty
 * queue is a place someone chose to be and should stay put.
 */
watch(
  () => attentionCount.value,
  (now, before) => {
    if (attentionOpen.value && now === 0 && (before ?? 0) > 0) attentionOpen.value = false
  },
)



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
 * An error from a button inside a dialog belongs inside that dialog.
 *
 * The banner renders in the page behind the panel, so a failed Approve wrote
 * its explanation somewhere the person pressing Approve could not see. On a
 * phone the dialog is the whole screen and the banner is not merely behind it
 * but completely covered — which is how a message naming the problem *and* the
 * two ways to fix it arrived as nothing at all, six presses in a row.
 *
 * Kept in the banner too: the dialog closes, and the reason should not vanish
 * with it.
 */
const dialogError = ref('')

function failIn(err: unknown) {
  dialogError.value = err instanceof Error ? err.message : String(err)
  fail(err)
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
    text: 'Lost contact with the daemon. Retrying, and this clears itself when it is back.',
  }
}

async function loadGlobals() {
  try {
    // Teams are not global any more: one can belong to a project, so the list
    // is fetched with the project's own refresh rather than once at startup.
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

/**
 * Refreshes everything scoped to the open project.
 *
 * Four requests go out per refresh and each takes its own time. Switch project
 * while they are in flight and the older set can land after the newer one,
 * putting one project's board under another project's name — briefly, and
 * exactly when someone is looking to see whether the switch worked.
 *
 * Comparing the project id was the first attempt and is not enough: going A →
 * B → A leaves two refreshes for A outstanding, the id matches for both, and
 * the older one is free to land last. A sequence is what actually separates
 * them.
 */
const newestRefresh = latest()

async function refresh() {
  const project = current.value
  if (!project) return
  const current_ = newestRefresh()
  try {
    const [t, tk, at, st, ps] = await Promise.all([
      api.team(project.id),
      api.tasks(project.id),
      api.attention(project.id),
      api.status(project.id),
      api.teamPresets(project.id),
    ])
    // A newer refresh has been asked for; this data is already history.
    if (!current_()) return

    projectTeam.value = t
    team.value = t.roles
    presets.value = ps
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
    if (++usageTicks % USAGE_EVERY === 0) {
      usageKey.value++
      loadWorkspace()
    }
  } catch {
    if (!current_()) return
    pollerLostContact()
    pollDelay = Math.min(pollDelay * 2, POLL_MAX)
  }
}

async function open(project: Project) {
  const switching = current.value?.id !== project.id
  current.value = project
  // The project belongs in the URL, so a reload comes back to it. Replace
  // rather than push when only the id is being filled in: arriving at /board
  // and landing on /p/<id>/board is one destination, not two, and back should
  // not walk through it.
  if (route.params.projectId !== project.id) {
    const to = viewPath(project.id, view.value)
    // push when changing project, replace when only filling the id into the
    // URL: arriving at /board and landing on /p/<id>/board is one destination,
    // and back should not walk through it.
    if (switching) router.push(to)
    else router.replace(to)
  }
  // Record it, so the next start opens what you were last working on rather
  // than whichever project happened to be created most recently. The column
  // and the ordering both existed; nothing was writing it.
  api.openProject(project.id).catch(() => {})
  // Deliberately no navigation. Opening a project on startup used to force the
  // board, which silently discarded whatever route was asked for — so a link to
  // /settings, or a reload while on /chat, always landed on the board. Every
  // view is scoped to the current project anyway, so switching project while
  // reading one of them should keep you there.
  readiness.value = null
  await refresh()
  loadWorkspace()
}

/** Reload the list after one is added or removed, and pick a sensible current
 *  project when the one that was open has just gone. */
async function onProjectsChanged() {
  try {
    projects.value = await api.projects()
    if (!projects.value.some((p) => p.id === current.value?.id)) {
      current.value = null
      if (projects.value.length) await open(projects.value[0])
    }
  } catch (err) {
    fail(err)
  }
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

/**
 * Probing every enabled role runs real checks against real CLIs — a version, a
 * config parse, a model catalogue — so it takes seconds, not milliseconds, and
 * the button has to say it is working. It looked ignored until the answer
 * arrived.
 */
async function checkReadiness() {
  if (!current.value) return
  // Open first, run second. Waiting for the probe to answer before showing
  // anything is the same dead two seconds the button used to have, moved.
  readinessOpen.value = true
  await busy.run('readiness', async () => {
    try {
      readiness.value = await api.readiness(current.value!.id)
      showReadiness()
    } catch (err) {
      fail(err)
    }
  })
}

async function start() {
  const project = current.value
  if (!project) return
  // Starting a swarm shells out to preflight and git before it spawns
  // anything, so the button is unresponsive for seconds. A second press used
  // to reach the daemon and come back "this project is already starting" — an
  // error the operator caused by being told nothing was happening.
  await busy.run('swarm', async () => {
    banner.value = null
    try {
      // No banner. The bar already flips to "n/n agents live", which is the
      // same news said in the place you look for it and without a dismissal.
      status.value = await api.start(project.id)
    } catch (err) {
      // A refused start carries the readiness report, so show which role failed
      // which check rather than only that something was wrong.
      if (err instanceof ApiError && err.status === 409) {
        const body = err.body as { readiness?: Readiness } | undefined
        if (body?.readiness) {
          readiness.value = body.readiness
          showReadiness()
        }
      }
      fail(err)
    }
    await refresh()
  })
}

async function stop() {
  const project = current.value
  if (!project) return
  await busy.run('swarm', async () => {
    try {
      // No banner, for the same reason Start has none: the bar flips to "no
      // agents running", which is the same news said where you are already
      // looking and without something to dismiss afterwards.
      await api.stop(project.id)
    } catch (err) {
      fail(err)
    }
    await refresh()
  })
}

async function createTask() {
  const project = current.value
  if (!project || !taskName.value.trim()) return
  await busy.run('newTask', async () => {
    try {
      dialogError.value = ''
      await api.newTask(project.id, taskName.value.trim(), taskBody.value)
      taskName.value = ''
      taskBody.value = ''
      composing.value = false
      await refresh()
    } catch (err) {
      failIn(err)
    }
  })
}

async function setTeam(update: ProjectTeamUpdate) {
  if (!current.value) return
  try {
    projectTeam.value = await api.setTeam(current.value.id, update)
    team.value = projectTeam.value.roles
    const refreshed = await api.projects()
    projects.value = refreshed
    current.value = refreshed.find((p) => p.id === current.value?.id) ?? current.value
  } catch (err) {
    fail(err)
  }
}

async function savePreset(preset: TeamPreset) {
  try {
    const updated = await api.updateTeamPreset(preset)
    presets.value = presets.value.map((p) => (p.id === updated.id ? updated : p))
    // An edit to the team this project runs is invisible while the project also
    // carries a topology layer: the pipeline then comes from project_roles, not
    // from the team, so the roles just written are not the ones resolved. The
    // layer is a leftover from before a team could belong to a project, and
    // there is now nothing that writes one, so writing the team drops it. The
    // project's per-role overrides survive; only the frozen shape goes.
    if (current.value && projectTeam.value.topologyOverride && projectTeam.value.presetId === updated.id) {
      projectTeam.value = await api.setTeam(current.value.id, followPreset(updated, team.value))
      team.value = projectTeam.value.roles
    }
    await refresh()
  } catch (err) {
    fail(err)
  }
}

async function createPreset(name: string, roles: TeamPresetRole[], projectId: string | null) {
  try {
    const created = await api.createTeamPreset({ name, roles, projectId })
    presets.value = [...presets.value, created].sort((a, b) => a.name.localeCompare(b.name))
  } catch (err) {
    fail(err)
  }
}

/** Why the last team action was refused. Read by the dialog that asked. */
const teamError = ref('')

/** The same, for the rail's copy-this-team dialog. Separate because the two
 *  dialogs are on different screens and a stale message in one of them is a
 *  refusal attached to something nobody pressed. */
const forkError = ref('')

/**
 * A shared team edited from the rail becomes this project's own team.
 *
 * Created and adopted in that order, and the project keeps whatever per-role
 * overrides it had for roles the new team still has: the copy is meant to be
 * what was running a moment ago, plus the one change that prompted it.
 */
async function forkTeam(name: string, roles: TeamPresetRole[]) {
  if (!current.value) return
  forkError.value = ''
  try {
    const created = await api.createTeamPreset({ name, roles, projectId: current.value.id })
    presets.value = [...presets.value, created]
    projectTeam.value = await api.setTeam(current.value.id, followPreset(created, team.value))
    team.value = projectTeam.value.roles
    const refreshed = await api.projects()
    projects.value = refreshed
    current.value = refreshed.find((p) => p.id === current.value?.id) ?? current.value
  } catch (err) {
    // Stays in the dialog, which stays open: a duplicate name is fixed by
    // typing another one, and closing would take the edit with it.
    forkError.value = err instanceof Error ? err.message : String(err)
  }
}

async function deletePreset(id: string) {
  teamError.value = ''
  try {
    await api.deleteTeamPreset(id)
    presets.value = presets.value.filter((p) => p.id !== id)
  } catch (err) {
    teamError.value = err instanceof Error ? err.message : String(err)
    fail(err)
  }
}

// Deciding an approval runs a merge or opens a pull request, so a second press
// is not a wasted request: it races the first through git. Keyed by the
// approval, so two different cards can still be decided at once.
const act = {
  approve: (id: string) =>
    busy.run(`decide:${id}`, async () => {
      dialogError.value = ''
      try {
        await api.approve(id)
      } catch (err) {
        failIn(err)
      }
      await refresh()
    }),
  reject: (id: string, note: string) =>
    busy.run(`decide:${id}`, async () => {
      dialogError.value = ''
      try {
        await api.reject(id, note)
      } catch (err) {
        failIn(err)
      }
      await refresh()
    }),
  answer: (id: string, answer: string) => {
    if (!answer.trim()) return
    return busy.run(`answer:${id}`, async () => {
      dialogError.value = ''
      try {
        await api.answer(id, answer)
      } catch (err) {
        failIn(err)
      }
      await refresh()
    })
  },
}

onMounted(async () => {
  try {
    await loadGlobals()
    // The URL wins over recency: someone who asked for a project by link means
    // that one, whatever was opened last.
    const asked = String(route.params.projectId ?? '')
    const wanted = projects.value.find((p) => p.id === asked) ?? projects.value[0]
    if (wanted) await open(wanted)
  } finally {
    // In a finally: a failed load must still stop claiming to be loading, or
    // the spinner becomes its own kind of lie.
    loading.value = false
  }
  schedulePoll(0)
})
/**
 * The URL is the project selection, not a record of it.
 *
 * Only onMounted read the route, so browser back and forward between two
 * project URLs moved the address bar and left the board showing the project
 * from before — the two disagreed with no way to tell which was real.
 */
watch(
  () => route.params.projectId,
  async (id) => {
    const asked = String(id ?? '')
    if (!asked || asked === current.value?.id) return
    const wanted = projects.value.find((p) => p.id === asked)
    if (wanted) await open(wanted)
  },
)

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
      :project-count="projects.length"
      :projects="projects"
      :current="current"
      :team-name="currentTeamName"
      :team="team"
      :library="library"
      :project-team="projectTeam"
      :preset="currentPreset"
      :presets="presets"
      :fork-error="forkError"
      :open="navOpen"
      @close="navOpen = false"
      @navigate="go"
      @open-project="open"
      @save-preset="savePreset"
      @fork-team="forkTeam"
    />

    <div class="flex min-w-0 flex-1 flex-col overflow-hidden">
      <ProjectBar
        :current="current"
        :status="status"
        :attention-count="attentionCount"
        :busy="busy.is('swarm')"
        :usage-key="usageKey"
        @menu="navOpen = true"
        @attention="attentionOpen = true"
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


      <!-- No project yet: one job on screen, nothing else. -->
      <!-- Loading. Deliberately quiet: this resolves in well under a second on
           loopback, and a spinner that flashes is worse than a still frame. -->
      <main v-if="loading" class="grid flex-1 place-items-center p-8">
        <div class="flex items-center gap-2.5">
          <span class="loading-pulse size-2 rounded-full bg-[var(--primary)]" />
          <span class="text-muted-foreground text-xs">Loading…</span>
        </div>
      </main>

      <main v-else-if="!current" class="grid flex-1 place-items-center overflow-y-auto p-8">
        <div class="w-full max-w-md text-center">
          <h1 class="text-lg font-semibold tracking-tight">No project yet</h1>
          <p class="text-muted-foreground mt-1.5 mb-4 text-xs leading-relaxed">
            Point zerg at a git repository. It starts with the reusable Default team, coder then
            reviewer, and can be changed in Team.
          </p>
          <Button @click="addingProject = true">Add a project</Button>
        </div>
      </main>

      <!-- The board owns its own scrolling; every other view scrolls the page.
           Lanes have to scroll under a heading that stays put, and a heading
           can only stick to the box that scrolls it — so on the board the
           scrolling moves inside, and main must not scroll too or the two
           fight over the same gesture. -->
      <main
        v-else
        :class="[
          'min-h-0 flex-1',
          view === 'board' ? 'flex flex-col overflow-hidden' : 'overflow-y-auto',
        ]"
      >
        <div
          :class="[
            'w-full p-[var(--gutter)]',
            view === 'board' && 'flex min-h-0 flex-1 flex-col',
          ]"
        >
          <!-- Board -->
          <template v-if="view === 'board'">
            <BoardHeader
              :project="current"
              :tasks="tasks"
              :workspace="workspace"
            >
              <template #actions>
                <div v-if="hiddenCount" class="flex items-center gap-2">
                  <Switch id="show-hidden" v-model="showHidden" />
                  <Label for="show-hidden" class="cursor-pointer text-xs font-normal">
                    Show hidden
                  </Label>
                </div>
                <Button @click="composing = true">New task</Button>
              </template>
            </BoardHeader>
            <div class="flex min-h-0 flex-1 flex-col pt-4"><Board
                :team="team"
                :tasks="tasks"
                :show-hidden="showHidden"
                :needs-attention="attentionTaskIds"
                :blocked-on="attentionByTask"
                @open="(t) => (openTask = t)"
                @review="() => (attentionOpen = true)"
                @hide="(t: Task) => setHidden(t, true)"
                @unhide="(t: Task) => setHidden(t, false)"
                @stop="stopTask"
                @activity="showTaskActivity"
                @remove="(t: Task) => (confirmDeleteTask = t)"
              /></div>
          </template>

          <!-- Chat -->
          <template v-else-if="view === 'chat'">
            <PageHeader
              title="Chat"
              subtitle="Ask about the project. This agent reads it and answers; it takes no work."
            />
            <div class="pt-4">
              <Chat
                :project="current"
                :harnesses="harnesses"
                :models="models"
                @updated="(p: Project) => (current = p)"
              />
            </div>
          </template>

          <!-- Projects -->
          <template v-else-if="view === 'projects'">
            <PageHeader
              title="Projects"
              subtitle="The repositories zerg knows about, and what is configured about each."
            />
            <div class="pt-4">
              <Projects
                :projects="projects"
                :current="current"
                @open="open"
                @updated="(p) => (current = p)"
                @changed="onProjectsChanged"
              />
            </div>
          </template>

          <!-- Settings -->
          <template v-else-if="view === 'settings'">
            <PageHeader
              title="Settings"
              subtitle="Everything a project is set up with: whether its team can work, what each role is, how the daemon serves the cockpit, and what it keeps on disk."
            />
            <div class="pt-4">
              <Settings :checking="busy.is('readiness')" @readiness="checkReadiness" />
            </div>
          </template>

          <!-- Spend -->
          <template v-else-if="view === 'spend'">
            <PageHeader
              title="Spend"
              subtitle="What the run cost, and which of the four token classes it went on."
            />
            <div class="pt-4">
              <Spend :project-id="current?.id ?? null" />
            </div>
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
              subtitle="A team can belong to this project or be shared by every project. Choose one, configure its roles, order its pipeline, then use it here."
            />
            <div class="pt-4">
              <TeamEditor
                :library="library"
                :presets="presets"
                :project-id="current?.id ?? ''"
                :project-name="current?.name ?? ''"
                :project-team="projectTeam"
                :harnesses="harnesses"
                :models="models"
                :running="status.running"
                @set-team="setTeam"
                @save-preset="savePreset"
                @create-preset="createPreset"
                :action-error="teamError"
                @delete-preset="deletePreset"
              />
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
          <Label :for="pathId">Path</Label>
          <ProjectPathField v-model="newPath" :input-id="pathId" autofocus @submit="addProject" />
        </div>
        <DialogFooter>
          <Button variant="outline" @click="addingProject = false">Cancel</Button>
          <Button :disabled="!newPath.trim()" @click="addProject">Add</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <!-- Opening a card. The name is the thing that follows this work through
         the whole pipeline, which is worth saying where it is being typed. -->
    <!-- Deleting takes the transcript with it, so it says what goes and what
         stays. The spend stays: the money was real, and a cost total that fell
         when a card was tidied away would disagree with the bill. -->
    <Dialog :open="!!confirmDeleteTask" @update:open="(v) => !v && (confirmDeleteTask = null)">
      <DialogContent class="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Delete “{{ confirmDeleteTask?.name }}”?</DialogTitle>
          <DialogDescription>
            The card and everything recorded against it, meaning its messages, its approvals and
            its whole transcript, is deleted. What it cost stays in the project's usage, because it
            was spent. Commits the agents made stay on their branches; nothing touches the
            repository.
          </DialogDescription>
        </DialogHeader>
        <p
          v-if="dialogError"
          class="bg-destructive/10 text-destructive px-3 py-2 text-xs"
          role="alert"
        >
          {{ dialogError }}
        </p>
        <DialogFooter>
          <Button variant="outline" @click="confirmDeleteTask = null">Cancel</Button>
          <Button
            variant="destructive"
            :disabled="busy.is(`task:${confirmDeleteTask!.id}`)"
            @click="removeTask(confirmDeleteTask!)"
          >
            Delete
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <!-- One card's transcript, over the board rather than instead of it. -->
    <Dialog :open="!!activityTask" @update:open="(v) => !v && (activityTask = null)">
      <DialogContent class="min-w-0 gap-0 overflow-hidden p-0 sm:max-w-4xl">
        <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
          <DialogTitle class="truncate">{{ activityTask?.name }}</DialogTitle>
          <DialogDescription>
            Every tool call, message and turn recorded against this card.
          </DialogDescription>
        </DialogHeader>
        <!-- Activity scrolls its own viewport, so this is a plain flex child
             rather than a DialogBody: two nested scroll regions is one too
             many, and the inner one is the one that follows the tail. -->
        <div v-if="activityTask" class="min-h-0 flex-1 px-5 py-4">
          <Activity
            :project-id="current?.id ?? ''"
            :roles="team.filter((r) => r.enabled).map((r) => r.name)"
            :task="activityTask.id"
            :active="activityTask.state === 'working' || activityTask.state === 'queued'"
            :blocked="attentionByTask.get(activityTask.id)"
            embedded
          />
        </div>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="attentionOpen" @update:open="(v) => v && (dialogError = '')">
      <!-- Wider than the other dialogs: a spec is the widest thing this app
           shows, and its behaviour tables are the point of reading it.
           min-w-0 because DialogContent is a grid, and a grid child defaults to
           min-width:auto — it refuses to shrink below its content and overflows
           the panel instead of scrolling inside it, which is what cut the right
           side off a wide table. -->
      <DialogContent class="min-w-0 gap-0 overflow-hidden p-0 sm:max-w-4xl">
        <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
          <DialogTitle>Waiting on you</DialogTitle>
          <DialogDescription>
            Nothing downstream moves until these are decided.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <!-- Where the person who pressed the button is looking. -->
          <p
            v-if="dialogError"
            class="bg-destructive/10 text-destructive mb-3 px-3 py-2 text-xs"
            role="alert"
          >
            {{ dialogError }}
          </p>
          <Attention
            :attention="attention"
            :busy="busy.is"
            @approve="act.approve"
            @reject="act.reject"
            @answer="act.answer"
          />
        </DialogBody>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="readinessOpen">
      <DialogContent class="min-w-0 gap-0 overflow-hidden p-0 sm:max-w-3xl">
        <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
          <DialogTitle>Readiness</DialogTitle>
          <DialogDescription>
            Every check for every enabled role. A team that cannot work must not reach a running
            board, so Start refuses while any of them is blocked.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div :class="busy.is('readiness') && 'pointer-events-none opacity-50 transition-opacity'">
            <ReadinessPanel :readiness="readiness" />
          </div>
          <p
            v-if="busy.is('readiness') && !readiness"
            class="text-muted-foreground py-10 text-center text-xs"
            role="status"
          >
            Probing every enabled role: a version, a config parse, a model catalogue…
          </p>
          <p v-else-if="!readiness" class="text-muted-foreground py-10 text-center text-xs">
            Not checked yet.
          </p>
        </DialogBody>
        <DialogFooter class="hairline-t shrink-0 px-5 py-4">
          <span v-if="readiness" class="text-muted-foreground mr-auto text-[11px]">
            checked at {{ new Date(readiness.checkedAt).toLocaleTimeString(undefined, { hour12: false }) }}
          </span>
          <Button variant="outline" :disabled="busy.is('readiness')" @click="checkReadiness">
            {{ busy.is('readiness') ? 'Checking…' : 'Re-check' }}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <TaskDetail
      :task="openTask"
      :roles="team.filter((r) => r.enabled).map((r) => r.name)"
      :project="current"
      @close="openTask = null"
    />

    <Dialog v-model:open="composing" @update:open="(v) => v && (dialogError = '')">
      <!-- The widest and tallest dialog in the app, on purpose: the brief is
           the whole of what an agent is told, and a spec-shaped one written in
           a six-line box reads like a chat message. A fixed height rather than
           a grown one, so the editor does not resize under the caret as you
           type. -->
      <DialogContent
        class="gap-0 overflow-hidden p-0 sm:h-[min(46rem,88vh)] sm:max-w-2xl lg:max-w-3xl"
      >
        <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
          <DialogTitle>New task</DialogTitle>
          <DialogDescription>
            Goes to {{ team.find((r) => r.enabled)?.name ?? 'the first role' }} first, then down
            the pipeline. The last role merges it to {{ current?.baseBranch ?? 'the base branch' }}.
          </DialogDescription>
        </DialogHeader>

        <DialogBody class="flex flex-col gap-3">
          <p
            v-if="dialogError"
            class="bg-destructive/10 text-destructive px-3 py-2 text-xs"
            role="alert"
          >
            {{ dialogError }}
          </p>
          <div class="flex flex-col gap-1.5">
            <Label :for="taskNameId">Name</Label>
            <Input :id="taskNameId" v-model="taskName" autofocus />
            <span class="text-muted-foreground text-[11px]">
              Short and distinct, since every role refers to the task by this name.
            </span>
          </div>
          <div class="flex min-h-0 flex-1 flex-col gap-1.5">
            <Label :for="taskBodyId">What to do</Label>
            <!-- Rich text on screen, Markdown in the database: this goes to
                 an agent as text, and Markdown is what it reads. The Source tab
                 is what will actually be sent. -->
            <div class="flex min-h-0 flex-1 border">
              <MarkdownEditor :id="taskBodyId" v-model="taskBody" :rows="8" />
            </div>
            <span class="text-muted-foreground text-[11px]">
              This is the whole brief. The agent has the repository and nothing else. Concrete
              cases and what must not break are worth more than length.
            </span>
          </div>
        </DialogBody>

        <!-- Outside the body, so Queue it is reachable without scrolling to the
             end of a brief you are still writing. -->
        <DialogFooter class="hairline-t shrink-0 px-5 py-4">
          <Button variant="outline" @click="composing = false">Cancel</Button>
          <Button :disabled="!taskName.trim() || busy.is('newTask')" @click="createTask">
            {{ busy.is('newTask') ? 'Queueing…' : 'Queue it' }}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>

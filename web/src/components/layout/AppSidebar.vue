<script setup lang="ts">
import { computed, ref } from 'vue'
import {
  Activity as ActivityIcon,
  ChevronDown,
  ChevronUp,
  Check,
  Columns3,
  GitMerge,
  GitPullRequest,
  GitPullRequestDraft,
  FolderGit2,
  GitBranch,
  MessageSquare,
  Pencil,
  Plus,
  Receipt,
  Settings as SettingsIcon,
  Undo2,
  Users,
  X,
} from '@lucide/vue'
import type {
  Project,
  ProjectTeam,
  ProjectTeamUpdate,
  ResolvedRole,
  RoleStatus,
  RoleTemplate,
  SwarmStatus,
  TeamPreset,
} from '@/lib/api'
import { landing } from '@/lib/utils'
import { followPreset, ownPipeline, projectRoles } from '@/lib/team'
import QuotaBars from '@/components/QuotaBars.vue'
import ProjectAvatar from '@/components/ProjectAvatar.vue'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
} from '@/components/ui/select'

import type { View } from '@/router'

const props = defineProps<{
  view: View
  status: SwarmStatus
  taskCount: number
  projectCount: number
  /** Which project everything on screen is scoped to, and what there is to
   *  switch between. The switcher lives here rather than in the top bar
   *  because it is the context every other control in this rail inherits —
   *  the board, the roles below, the quota. */
  projects: Project[]
  current: Project | null
  /** The team this project runs, and the roles it is made of. Named here
   *  rather than on the board header: the roles are listed right below, and a
   *  list of roles whose team is named on another screen is a list you have to
   *  remember the heading for. */
  teamName?: string
  team: ResolvedRole[]
  /** The whole library, so a role can be added to the pipeline from here
   *  rather than by going to the Team screen and back. */
  library: RoleTemplate[]
  /** Which team the project follows and whether its pipeline has already
   *  diverged from it. Editing here is what makes it diverge. */
  projectTeam: ProjectTeam
  /** The followed team itself, when there is one: needed to say which roles
   *  came from it, and to go back to following it. */
  preset: TeamPreset | null
  /** Whether the drawer is showing. Ignored at md and above, where the rail
   *  is always part of the layout. */
  open: boolean
}>()

/**
 * What to list under the team: the live roles when a swarm is up, the team's
 * own enabled roles when it is not.
 *
 * Without the fallback this panel disappeared the moment the swarm stopped —
 * `status.roles` is empty then — so the one place that shows what a team is
 * made of was blank exactly when you were deciding whether to start it.
 */
const rolesShown = computed<{ role: string; live: RoleStatus | null }[]>(() => {
  if (props.status.roles.length) {
    return props.status.roles.map((r) => ({ role: r.role, live: r }))
  }
  return props.team.filter((r) => r.enabled).map((r) => ({ role: r.name, live: null }))
})
/**
 * Changing the pipeline from the rail, where it is being read.
 *
 * Dropping a role for one piece of work meant the Team screen, which edits the
 * *shared* team: turning off the planner there turns it off for every project
 * that adopted that team. So this edits the project instead. The team keeps its
 * shape and its per-role overrides still apply, and what changes is which roles
 * this project runs and in what order.
 *
 * The cost is stated rather than discovered: once a project has its own
 * pipeline, later changes to the team's shape stop reaching it, and the notice
 * below carries the way back.
 */
const editing = ref(false)
/** The add list, open only while something is being picked from it. */
const adding = ref(false)

/** The live status of each role by name, for rows that are showing both. */
const liveByRole = computed(() => new Map(props.status.roles.map((r) => [r.role, r])))

/** Editing lists the whole team. A role that is off is the thing you came to
 *  turn back on, so it cannot be the one entry that is filtered out. */
const editRows = computed(() =>
  props.team.map((role) => ({ role, live: liveByRole.value.get(role.name) ?? null })),
)

const enabledCount = computed(() => props.team.filter((r) => r.enabled).length)
const presetRoleIds = computed(() => new Set(props.preset?.roles.map((r) => r.templateId) ?? []))

/** Library roles this team does not have yet. */
const addable = computed(() => {
  const inTeam = new Set(props.team.map((r) => r.id))
  return props.library.filter((t) => !inTeam.has(t.id))
})

/** Following a team, but not its pipeline: reversible, and worth saying. */
const divergedFromPreset = computed(() => props.projectTeam.topologyOverride && !!props.preset)

function saveTeam(team: ResolvedRole[]) {
  emit('setTeam', ownPipeline(props.projectTeam.presetId, team))
}

function toggleRole(role: ResolvedRole) {
  // A team with nothing enabled cannot be started and has nowhere to send a
  // task, so the last one standing does not turn off. Better to refuse the
  // click than to leave a project that looks configured and takes no work.
  if (role.enabled && enabledCount.value === 1) return
  saveTeam(props.team.map((r) => (r.id === role.id ? { ...r, enabled: !r.enabled } : r)))
}

function moveRole(index: number, by: number) {
  const to = index + by
  if (to < 0 || to >= props.team.length) return
  const team = [...props.team]
  const [moved] = team.splice(index, 1)
  team.splice(to, 0, moved)
  saveTeam(team)
}

/**
 * Removing is for roles added here; a role the team itself has is turned off.
 *
 * Both are undone by following the team again, but only one of them survives
 * it: deleting a role that the team has would come straight back the moment
 * anything re-followed the preset, which reads as the click not working.
 */
function removable(role: ResolvedRole) {
  return !presetRoleIds.value.has(role.id)
}

function removeRole(role: ResolvedRole) {
  saveTeam(props.team.filter((r) => r.id !== role.id))
}

/**
 * Added at the end, which is where the work ends up: the last enabled role is
 * the terminal one, so the new role takes over integrating, and the landing
 * line under the list moves with it rather than reporting something else.
 */
function addRole(id: unknown) {
  const tpl = props.library.find((t) => t.id === id)
  if (!tpl) return
  adding.value = false
  emit('setTeam', {
    presetId: props.projectTeam.presetId,
    topologyOverride: true,
    roles: [...projectRoles(props.team), { templateId: tpl.id, enabled: true, argsOverride: null }],
  })
}

function followTeamAgain() {
  if (props.preset) emit('setTeam', followPreset(props.preset, props.team))
}

const emit = defineEmits<{
  navigate: [view: View]
  close: []
  openProject: [project: Project]
  setTeam: [team: ProjectTeamUpdate]
}>()

function pick(id: unknown) {
  const project = props.projects.find((p) => p.id === id)
  if (project && project.id !== props.current?.id) emit('openProject', project)
}

// Icons are picked for what the view does, not for decoration: lanes for the
// board, a pulse for the live stream, a bell for the one screen that is
// waiting on a person.
const nav = computed(() => [
  // Ordered by how a project is actually used: the board first, then what it
  // runs on, then what it costs, then how it is configured. Readiness is not
  // here — it is a setup step, and it lives in Settings with the rest of them.
  { key: 'board' as const, label: 'Board', icon: Columns3, count: props.taskCount },
  { key: 'projects' as const, label: 'Projects', icon: FolderGit2, count: props.projectCount },
  { key: 'team' as const, label: 'Team', icon: Users, count: 0 },
  { key: 'chat' as const, label: 'Chat', icon: MessageSquare, count: 0 },
  { key: 'spend' as const, label: 'Spend', icon: Receipt, count: 0 },
  { key: 'activity' as const, label: 'Activity', icon: ActivityIcon, count: 0 },
  { key: 'settings' as const, label: 'Settings', icon: SettingsIcon, count: 0 },
])

/** Role state drives a colour and a word. Never colour alone. */
function tone(state: string): string {
  if (state === 'working') return 'text-[var(--primary)]'
  if (state === 'ready') return 'text-[var(--status-good)]'
  if (state === 'blocked' || state === 'failed') return 'text-destructive'
  // Waiting on a quota window is not a fault. Amber, not red: nobody has to
  // do anything, and red would send someone looking for a problem to fix.
  if (state === 'throttled') return 'text-[var(--status-warning)]'
  return 'text-muted-foreground'
}

/** "in 47m", "in 2h 10m" — how long until the role resumes by itself. */
function resumesIn(iso?: string): string {
  if (!iso) return ''
  const ms = new Date(iso).getTime() - Date.now()
  if (!Number.isFinite(ms) || ms <= 0) return 'any moment'
  const mins = Math.round(ms / 60000)
  if (mins < 60) return `in ${mins}m`
  return `in ${Math.floor(mins / 60)}h ${mins % 60}m`
}
/** One entry per provider, ordered so the list is stable across polls. */
const quotas = computed(() =>
  Object.entries(props.status.quotas ?? {})
    .map(([provider, report]) => ({ provider, report }))
    .sort((a, b) => a.provider.localeCompare(b.provider)),
)

function live(r: RoleStatus): boolean {
  return r.state === 'working'
}
</script>

<template>
  <aside
    :class="[
      'bg-[var(--surface-sunken)] hairline-r flex w-[var(--rail)] shrink-0 flex-col',
      // Below md the rail would take most of a phone screen, so it slides in
      // over the content instead of pushing it into a column too narrow to
      // read. Above md it is an ordinary part of the layout and never moves.
      'max-md:fixed max-md:inset-y-0 max-md:left-0 max-md:z-50 max-md:transition-transform',
      open ? 'max-md:translate-x-0' : 'max-md:-translate-x-full',
    ]"
  >
    <!-- Identity, and whether anything is running at all. -->
    <div class="hairline-b flex min-h-[var(--topbar)] shrink-0 items-center gap-2.5 px-3">
      <!-- The raster, not the SVG. The mark's glow is two feGaussianBlur
           filters, and a filter is rasterised into an offscreen buffer whose
           resolution mobile browsers cap — a 512-unit viewBox drawn at 36px
           came out visibly pixelated on a phone. This PNG has the same glow
           baked in at 192px, which is five times what 36 CSS pixels need on a
           3x display, and no filter to resolve at render time. -->
      <img
        src="/android-chrome-192x192.png"
        alt=""
        width="36"
        height="36"
        class="size-9 shrink-0"
      />
      <!-- The mark and the name, and nothing else. This used to carry a
           pulsing "live" badge, which said the same thing as the run state in
           the bar and the roles listed below it — three places for one fact,
           and the one attached to the logo was the one furthest from anything
           you could do about it. -->
      <span class="text-base font-bold tracking-tight">zerg</span>
    </div>

    <!-- Which project. First thing in the rail, above the destinations,
         because every one of them means something different depending on this
         answer — and it stays on screen rather than being one more thing in a
         top bar that is already carrying the run control and the alerts. -->
    <div v-if="projects.length" class="hairline-b p-2">
      <Select :model-value="current?.id" @update:model-value="pick">
        <!-- Three overrides of SelectTrigger's own utilities, each for a
             reason the default shape cannot accommodate:
               h-auto  — the variant sets a fixed 32px, and this trigger is two
                         lines, so the branch overflowed the box it was in.
               justify-start — the default spreads its children apart, which put
                         57px of nothing between the mark and the name.
               border/bg — this sits in a rail, not in a form; a boxed control
                         here reads as a field waiting to be filled in. -->
        <SelectTrigger
          class="hover:bg-muted h-auto w-full justify-start gap-2.5 border-0 bg-transparent px-2 py-2 transition-colors data-[size=default]:h-auto"
          aria-label="Switch project"
        >
          <ProjectAvatar :project="current" />
          <!-- min-w-0 so a long repository name ellipses instead of pushing the
               chevron off the rail; flex-1 so the chevron is pushed to the far
               edge rather than sitting against the text. -->
          <span class="flex min-w-0 flex-1 flex-col items-start gap-0.5 text-left">
            <span class="w-full truncate text-xs leading-tight font-semibold">
              {{ current?.name ?? 'choose a project' }}
            </span>
            <!-- The base branch, because it is the answer to "what will this
                 land on" and it is not visible anywhere else on the board. -->
            <span
              v-if="current"
              class="text-muted-foreground flex w-full min-w-0 items-center gap-1 text-[10px] leading-tight"
            >
              <GitBranch :size="9" class="shrink-0" aria-hidden="true" />
              <span class="truncate">{{ current.baseBranch }}</span>
            </span>
          </span>
        </SelectTrigger>
        <!-- popper, not the default item-aligned. item-aligned is the native
             select behaviour — it lays the chosen row over the trigger — and it
             measures against a one-line control, so against this one it put the
             whole list at the bottom edge of the window, off screen. -->
        <SelectContent position="popper" align="start" class="min-w-(--reka-select-trigger-width)">
          <SelectItem v-for="p in projects" :key="p.id" :value="p.id" class="py-2 pr-8 pl-2">
            <span class="flex min-w-0 items-center gap-2.5">
              <ProjectAvatar :project="p" size="sm" />
              <span class="flex min-w-0 flex-col gap-0.5">
                <span class="truncate text-xs leading-tight font-medium">{{ p.name }}</span>
                <span class="text-muted-foreground truncate text-[10px] leading-tight">
                  {{ p.baseBranch }}
                </span>
              </span>
            </span>
          </SelectItem>
        </SelectContent>
      </Select>
    </div>

    <!-- Where to go. Counts sit right-aligned so the column scans vertically. -->
    <nav class="flex flex-col gap-px p-2">
      <button
        v-for="item in nav"
        :key="item.key"
        :class="[
          'group flex items-center gap-2 px-2 py-2 text-left text-xs transition-colors',
          'focus-visible:outline-ring focus-visible:outline-2 focus-visible:-outline-offset-2',
          view === item.key
            ? 'bg-primary/12 text-foreground font-semibold'
            : 'text-muted-foreground hover:bg-muted hover:text-foreground',
        ]"
        @click="emit('navigate', item.key), emit('close')"
      >
        <!-- The active rail stays. An icon changing colour is a weaker signal
             than a mark at the edge, and it is the one cue that survives at a
             glance down the column. -->
        <span
          :class="['h-4 w-px shrink-0', view === item.key ? 'bg-primary' : 'bg-transparent']"
        />
        <component
          :is="item.icon"
          :size="15"
          :stroke-width="view === item.key ? 2.25 : 1.75"
          class="shrink-0"
          aria-hidden="true"
        />
        {{ item.label }}
        <span
          v-if="item.count"
          :class="[
            'tabular ml-auto px-1 text-[10px] font-semibold',
            'text-muted-foreground',
          ]"
          >{{ item.count }}</span
        >
      </button>
    </nav>

    <!-- The live readout, under the name of the team it belongs to. This is
         the panel worth glancing at: which agents exist, and what each is
         doing right now. -->
    <div v-if="current" class="hairline-t mt-1 flex min-h-0 flex-1 flex-col">
      <div class="flex items-center gap-1.5 px-3 pt-3 pb-1.5">
        <Users :size="11" class="text-muted-foreground shrink-0" aria-hidden="true" />
        <span
          class="text-muted-foreground min-w-0 flex-1 truncate text-[10px] font-bold tracking-[0.14em] uppercase"
          :title="teamName || undefined"
        >
          {{ teamName || 'Roles' }}
        </span>
        <span v-if="!editing" class="text-muted-foreground/70 tabular shrink-0 text-[10px]">
          {{ rolesShown.length }}
        </span>
        <!-- The pipeline is edited where it is read. Everything this button
             reveals changes this project only. -->
        <button
          type="button"
          class="text-muted-foreground hover:text-foreground focus-visible:outline-ring shrink-0 p-0.5 focus-visible:outline-2"
          :title="editing ? 'Done' : 'Add or turn off roles for this project'"
          :aria-label="editing ? 'Done editing this pipeline' : 'Edit this pipeline'"
          @click="editing = !editing"
        >
          <component :is="editing ? Check : Pencil" :size="12" aria-hidden="true" />
        </button>
      </div>

      <!-- A pipeline of this project's own. Said in both modes: a reader who
           never opens the editor still needs to know that changes to the team
           no longer arrive here, and the way back is one press. -->
      <div
        v-if="divergedFromPreset"
        class="text-muted-foreground/80 flex items-center gap-1.5 px-3 pb-1.5 text-[10px]"
      >
        <span class="min-w-0 flex-1 truncate">own pipeline</span>
        <button
          type="button"
          class="hover:text-foreground focus-visible:outline-ring flex shrink-0 items-center gap-1 focus-visible:outline-2"
          :title="'Drop this project\'s own pipeline and follow ' + (teamName || 'the team') + ' again'"
          @click="followTeamAgain"
        >
          <Undo2 :size="10" aria-hidden="true" />
          follow {{ teamName }}
        </button>
      </div>

      <!-- Editing: the whole team, including the roles that are off, since one
           of those is what you came to turn back on. -->
      <div v-if="editing" class="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
        <p
          v-if="status.running"
          class="text-muted-foreground mb-1 px-1 text-[10px] leading-snug"
        >
          Agents are running. A role turned off stops within a second or so and whatever it was
          holding goes back to the queue.
        </p>
        <ul>
          <li v-for="(r, i) in editRows" :key="r.role.id" class="flex items-center gap-0.5 py-0.5">
            <button
              type="button"
              role="switch"
              :aria-checked="r.role.enabled"
              class="focus-visible:outline-ring flex min-w-0 flex-1 items-center gap-2 px-1 py-1 text-left focus-visible:outline-2 disabled:cursor-not-allowed"
              :disabled="r.role.enabled && enabledCount === 1"
              :title="
                r.role.enabled
                  ? enabledCount === 1
                    ? 'A team needs one role that is on'
                    : 'Turn ' + r.role.name + ' off for this project'
                  : 'Turn ' + r.role.name + ' on for this project'
              "
              @click="toggleRole(r.role)"
            >
              <span
                :class="[
                  'size-1.5 shrink-0 rounded-full',
                  r.role.enabled
                    ? 'bg-[var(--primary)]'
                    : 'border-muted-foreground/40 border bg-transparent',
                ]"
              />
              <span
                :class="[
                  'truncate text-xs',
                  r.role.enabled ? 'font-medium' : 'text-muted-foreground/60 line-through',
                ]"
              >
                {{ r.role.name }}
              </span>
            </button>
            <!-- Order is the route work takes, so it is editable here too:
                 a project with its own pipeline has nowhere else to reorder it,
                 since the Team screen edits the shared team. -->
            <button
              type="button"
              class="text-muted-foreground/70 hover:text-foreground shrink-0 p-0.5 disabled:opacity-25"
              :disabled="i === 0"
              :aria-label="'Move ' + r.role.name + ' earlier'"
              @click="moveRole(i, -1)"
            >
              <ChevronUp :size="11" aria-hidden="true" />
            </button>
            <button
              type="button"
              class="text-muted-foreground/70 hover:text-foreground shrink-0 p-0.5 disabled:opacity-25"
              :disabled="i === editRows.length - 1"
              :aria-label="'Move ' + r.role.name + ' later'"
              @click="moveRole(i, 1)"
            >
              <ChevronDown :size="11" aria-hidden="true" />
            </button>
            <button
              v-if="removable(r.role)"
              type="button"
              class="text-muted-foreground/70 hover:text-destructive shrink-0 p-0.5"
              :aria-label="'Remove ' + r.role.name + ' from this pipeline'"
              :title="'Remove ' + r.role.name + ' from this pipeline'"
              @click="removeRole(r.role)"
            >
              <X :size="11" aria-hidden="true" />
            </button>
          </li>
        </ul>

        <div v-if="addable.length" class="pt-1">
          <button
            type="button"
            class="text-muted-foreground hover:text-foreground focus-visible:outline-ring flex w-full items-center gap-2 px-1 py-1 text-left text-[11px] focus-visible:outline-2"
            @click="adding = !adding"
          >
            <Plus :size="11" class="shrink-0" aria-hidden="true" />
            Add a role
          </button>
          <ul v-if="adding" class="border-border ml-1 border-l pl-2">
            <li v-for="t in addable" :key="t.id">
              <button
                type="button"
                class="hover:bg-muted focus-visible:outline-ring w-full truncate px-1 py-1 text-left text-xs focus-visible:outline-2"
                :title="'Add ' + t.name + ' to the end of this pipeline'"
                @click="addRole(t.id)"
              >
                {{ t.name }}
              </button>
            </li>
          </ul>
        </div>
      </div>

      <ul v-else class="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
        <li v-for="(r, i) in rolesShown" :key="r.role" class="relative px-1 py-1.5">
          <!-- The pipeline is an order, and a list does not say so on its own:
               these three names read as a set of agents rather than as a route
               work takes. The rail runs from each role's dot to the next one's,
               which is the direction a handoff actually travels. -->
          <template v-if="i < rolesShown.length - 1 || current?.baseBranch">
            <span
              class="bg-border absolute top-[19px] bottom-[-7px] left-[6.5px] w-px"
              aria-hidden="true"
            />
            <ChevronDown
              :size="9"
              class="text-muted-foreground/50 absolute -bottom-[7px] left-[2.5px]"
              aria-hidden="true"
            />
          </template>
          <div class="flex items-center gap-2">
            <span
              v-if="r.live"
              :class="[
                'size-1.5 shrink-0 rounded-full bg-current',
                tone(r.live.state),
                live(r.live) && 'pulse-dot',
              ]"
            />
            <!-- Hollow, because a role that is configured and not running is
                 not a state the swarm is in. -->
            <span
              v-else
              class="border-muted-foreground/40 size-1.5 shrink-0 rounded-full border"
            />
            <span :class="['truncate text-xs font-medium', !r.live && 'text-muted-foreground']">
              {{ r.role }}
            </span>
            <span
              v-if="r.live"
              :class="['tabular ml-auto text-[10px]', tone(r.live.state)]"
            >
              {{ r.live.state }}
            </span>
            <span v-else class="text-muted-foreground/60 tabular ml-auto text-[10px]">
              not started
            </span>
          </div>
          <!-- A throttled role says when it comes back, because that is the
               only question it raises. It is not an error and is not styled
               as one. -->
          <p
            v-if="r.live?.state === 'throttled'"
            class="mt-1 pl-3.5 text-[10px] break-words text-[var(--status-warning)]"
          >
            provider limit · resumes
            {{ resumesIn(r.live.throttledUntil) || 'when the window rolls over' }}
            <span v-if="r.live.lastError" class="text-muted-foreground block">
              {{ r.live.lastError }}
            </span>
          </p>
          <!-- A failed role says why, here, where it is already being looked at. -->
          <p
            v-else-if="r.live?.lastError"
            class="text-destructive mt-1 pl-3.5 text-[10px] break-words"
          >
            {{ r.live.lastError }}
          </p>
        </li>

        <!-- Where the last role's work lands, which is the end of the same
             route and the one step of it that is not a role. Read from the
             project: two of the three settings do not merge anything. -->
        <li v-if="current?.baseBranch" class="relative px-1 pt-1.5">
          <div class="text-muted-foreground/60 flex items-center gap-2 text-[10px]">
            <component
              :is="
                current.integration === 'pr'
                  ? current.prDraft
                    ? GitPullRequestDraft
                    : GitPullRequest
                  : current.integration === 'branch'
                    ? GitBranch
                    : GitMerge
              "
              :size="9"
              class="ml-[-1px] shrink-0"
              aria-hidden="true"
            />
            <span class="truncate" :title="landing(current).line">{{ landing(current).line }}</span>
          </div>
        </li>
      </ul>

      <!-- What each account has left, under the roles that spend it. Here
           rather than the top bar: on a phone the bar is already carrying the
           project, the alerts and the run control, and a gauge is something you
           go and look at rather than something that has to be in your eye. -->
      <div v-if="quotas.length" class="hairline-t px-3 py-2.5">
        <p class="text-muted-foreground mb-1.5 text-[10px] font-semibold tracking-wide uppercase">
          Plan usage
        </p>
        <div v-for="q in quotas" :key="q.provider" class="mb-2 last:mb-0">
          <p class="text-muted-foreground/80 mb-1 truncate text-[10px]">
            {{ q.report.provider || q.provider
            }}<span v-if="q.report.plan"> · {{ q.report.plan }}</span>
          </p>
          <QuotaBars :quota="q.report" />
        </div>
      </div>
    </div>
    <div v-else class="flex-1" />

  </aside>
</template>

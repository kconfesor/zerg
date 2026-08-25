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
import Board from '@/components/Board.vue'
import ReadinessPanel from '@/components/Readiness.vue'
import TeamEditor from '@/components/TeamEditor.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

type Tab = 'board' | 'team' | 'readiness'

const projects = ref<Project[]>([])
const current = ref<Project | null>(null)
const tab = ref<Tab>('board')

const library = ref<RoleTemplate[]>([])
const team = ref<ResolvedRole[]>([])
const tasks = ref<Task[]>([])
const attention = ref<AttentionData | null>(null)
const readiness = ref<Readiness | null>(null)
const status = ref<SwarmStatus>({ running: false, roles: [] })
const harnesses = ref<string[]>([])
const models = ref<Record<string, Model[]>>({})

const banner = ref<{ tone: 'bad' | 'ok'; text: string } | null>(null)
const newPath = ref('')
const taskName = ref('')
const taskBody = ref('')

let timer: number | undefined

const attentionCount = computed(() => {
  const a = attention.value
  if (!a) return 0
  return a.approvals.length + a.clarifications.length + a.rework.tasks.length
})

function fail(err: unknown) {
  banner.value = { tone: 'bad', text: err instanceof Error ? err.message : String(err) }
}

async function loadGlobals() {
  try {
    ;[projects.value, library.value, harnesses.value] = await Promise.all([
      api.projects(),
      api.roles(),
      api.harnesses(),
    ])
    for (const h of harnesses.value) {
      models.value[h] = await api.models(h).catch(() => [])
    }
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
  } catch (err) {
    fail(err)
  }
}

async function open(project: Project) {
  current.value = project
  tab.value = 'board'
  readiness.value = null
  await refresh()
}

async function addProject() {
  if (!newPath.value.trim()) return
  try {
    const p = await api.createProject(newPath.value.trim(), 'main')
    newPath.value = ''
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
    tab.value = 'readiness'
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
        tab.value = 'readiness'
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
    banner.value = { tone: 'ok', text: `Saved ${role.name}. Restart the role for it to take effect.` }
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

const stateVariant = (s: string) =>
  s === 'ready' || s === 'working' ? 'default' : s === 'blocked' || s === 'failed' ? 'destructive' : 'outline'

onMounted(async () => {
  await loadGlobals()
  if (projects.value.length) await open(projects.value[0])
  // Polling until the event stream lands. The queue is durable, so a missed
  // tick costs nothing but a slightly stale screen.
  timer = window.setInterval(refresh, 2000)
})
onUnmounted(() => window.clearInterval(timer))
watch(current, () => (banner.value = null))
</script>

<template>
  <div class="flex min-h-dvh flex-col">
    <header class="bg-card flex flex-wrap items-center gap-3 border-b px-4 py-2.5">
      <div class="flex items-center gap-2">
        <span
          class="bg-primary text-primary-foreground grid size-5 place-items-center text-xs font-bold"
          >z</span
        >
        <span class="text-sm font-bold tracking-tight">zerg</span>
      </div>

      <Select
        v-if="projects.length"
        :model-value="current?.id"
        @update:model-value="(v) => open(projects.find((p) => p.id === v)!)"
      >
        <SelectTrigger size="sm" class="w-44"><SelectValue /></SelectTrigger>
        <SelectContent>
          <SelectItem v-for="p in projects" :key="p.id" :value="p.id">{{ p.name }}</SelectItem>
        </SelectContent>
      </Select>
      <span v-if="current" class="text-muted-foreground text-xs">
        {{ current.path }} · {{ current.baseBranch }}
      </span>

      <div class="ml-auto flex items-center gap-2">
        <Badge :variant="status.running ? 'default' : 'outline'">
          {{ status.running ? 'running' : 'stopped' }}
        </Badge>
        <Button v-if="!status.running" size="sm" variant="outline" @click="checkReadiness">
          Check
        </Button>
        <Button v-if="!status.running" size="sm" :disabled="!current" @click="start">Start</Button>
        <Button v-else size="sm" variant="destructive" @click="stop">Stop</Button>
      </div>
    </header>

    <p
      v-if="banner"
      :class="[
        'border-b px-4 py-2 text-xs',
        banner.tone === 'bad'
          ? 'border-destructive/40 bg-destructive/10 text-destructive'
          : 'border-[var(--status-good)]/40 bg-[var(--status-good)]/10 text-[var(--status-good)]',
      ]"
    >
      {{ banner.text }}
    </p>

    <!-- No project yet. -->
    <main v-if="!current" class="mx-auto w-full max-w-2xl p-6">
      <Card>
        <CardHeader>
          <CardTitle>Add a project</CardTitle>
        </CardHeader>
        <CardContent>
          <div class="flex gap-2">
            <Input
              v-model="newPath"
              placeholder="/Users/you/source/your-repo"
              class="min-w-0 flex-1"
              @keyup.enter="addProject"
            />
            <Button @click="addProject">Add</Button>
          </div>
          <p class="text-muted-foreground mt-2 text-xs">
            A new project starts with coder → reviewer selected. Everything else is a checkbox in
            Team.
          </p>
        </CardContent>
      </Card>
    </main>

    <main v-else class="flex-1 p-4">
      <div class="mb-4 flex flex-wrap items-center gap-3">
        <Tabs v-model="tab">
          <TabsList>
            <TabsTrigger value="board">Board</TabsTrigger>
            <TabsTrigger value="team">Team</TabsTrigger>
            <TabsTrigger value="readiness">Readiness</TabsTrigger>
          </TabsList>
        </Tabs>
        <Badge v-if="attentionCount" variant="secondary">{{ attentionCount }} needs you</Badge>
      </div>

      <div class="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
        <div class="flex min-w-0 flex-col gap-4">
          <template v-if="tab === 'board'">
            <Card>
              <CardHeader><CardTitle>New task</CardTitle></CardHeader>
              <CardContent>
                <div class="flex flex-wrap gap-2">
                  <Input v-model="taskName" placeholder="short, stable name" class="w-48" />
                  <Input
                    v-model="taskBody"
                    placeholder="what to do"
                    class="min-w-0 flex-1"
                    @keyup.enter="createTask"
                  />
                  <Button @click="createTask">Open card</Button>
                </div>
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle>Board <span class="text-muted-foreground font-normal">{{ tasks.length }} cards</span></CardTitle>
              </CardHeader>
              <CardContent><Board :team="team" :tasks="tasks" /></CardContent>
            </Card>
          </template>

          <Card v-else-if="tab === 'team'">
            <CardHeader><CardTitle>Team</CardTitle></CardHeader>
            <CardContent>
              <TeamEditor
                :library="library"
                :team="team"
                :harnesses="harnesses"
                :models="models"
                :running="status.running"
                @set-team="setTeam"
                @save-role="saveRole"
              />
            </CardContent>
          </Card>

          <Card v-else>
            <CardHeader class="flex-row items-center justify-between">
              <CardTitle>
                Readiness
                <span class="text-muted-foreground font-normal">
                  a team that cannot work must not reach a running board
                </span>
              </CardTitle>
              <Button size="sm" variant="outline" @click="checkReadiness">Re-check</Button>
            </CardHeader>
            <CardContent>
              <ReadinessPanel :readiness="readiness" />
              <p v-if="!readiness" class="text-muted-foreground text-xs">
                Not checked yet. Press Re-check.
              </p>
            </CardContent>
          </Card>
        </div>

        <aside class="flex min-w-0 flex-col gap-4">
          <Card>
            <CardHeader><CardTitle>Attention</CardTitle></CardHeader>
            <CardContent>
              <Attention
                :attention="attention"
                @approve="act.approve"
                @reject="act.reject"
                @answer="act.answer"
              />
            </CardContent>
          </Card>

          <Card v-if="status.roles.length">
            <CardHeader><CardTitle>Roles</CardTitle></CardHeader>
            <CardContent>
              <ul class="flex flex-col gap-2">
                <li v-for="r in status.roles" :key="r.role" class="flex flex-wrap items-center gap-2">
                  <span class="w-20 text-xs font-medium">{{ r.role }}</span>
                  <Badge :variant="stateVariant(r.state)">{{ r.state }}</Badge>
                  <Badge v-if="r.terminal" variant="secondary">terminal</Badge>
                  <span v-if="r.restarts" class="text-muted-foreground text-xs">
                    {{ r.restarts }} restarts
                  </span>
                  <!-- A failed role says why. It never reads as merely idle. -->
                  <span v-if="r.lastError" class="text-destructive w-full text-xs break-words">
                    {{ r.lastError }}
                  </span>
                </li>
              </ul>
            </CardContent>
          </Card>
        </aside>
      </div>
    </main>
  </div>
</template>

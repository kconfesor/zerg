<script setup lang="ts">
/**
 * The projects, and what is configured about the one you pick.
 *
 * Two columns rather than a picker in the header and settings somewhere else.
 * Everything on this screen is scoped to one project, which is exactly what the
 * old arrangement made hard to see: project settings lived in a Settings view
 * whose other three tabs were daemon-wide, so the same page changed one repo or
 * all of them depending on which tab you were on.
 *
 * Selecting here also opens the project. There is no useful state where the
 * project you are configuring is not the project you are running.
 */
import { ref } from 'vue'
import { FolderGit2, Plus, Trash2 } from '@lucide/vue'
import { api, type Integration, type Project } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

const props = defineProps<{ projects: Project[]; current: Project | null }>()
const emit = defineEmits<{
  open: [project: Project]
  changed: []
  updated: [project: Project]
}>()

const INTEGRATIONS: { value: Integration; label: string; why: string }[] = [
  {
    value: 'merge',
    label: 'Merge into the base branch',
    why: 'The last role fast-forwards the base branch when it approves. Right for a repository you own outright; wrong wherever the base is protected.',
  },
  {
    value: 'pr',
    label: 'Open a pull request',
    why: "Pushes the work to its own branch and opens a PR, using the last role's handoff note as the description. Needs the gh CLI and a remote.",
  },
  {
    value: 'branch',
    label: 'Leave it on a branch',
    why: 'The task finishes and nothing else happens. Landing it is your decision, taken later.',
  },
]

const adding = ref(false)
const newPath = ref('')
const newBranch = ref('')
const busy = ref(false)
const note = ref<{ tone: 'ok' | 'bad'; text: string } | null>(null)
const confirmDelete = ref<Project | null>(null)

function fail(e: unknown) {
  note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
}

async function add() {
  if (!newPath.value.trim()) return
  busy.value = true
  note.value = null
  try {
    const p = await api.createProject(newPath.value.trim(), newBranch.value.trim())
    adding.value = false
    newPath.value = ''
    newBranch.value = ''
    emit('changed')
    emit('open', p)
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

async function setIntegration(mode: Integration) {
  if (!props.current || props.current.integration === mode) return
  busy.value = true
  note.value = null
  try {
    emit('updated', await api.setIntegration(props.current.id, mode))
    note.value = { tone: 'ok', text: 'Saved. It applies to the next task that finishes.' }
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

async function remove(p: Project) {
  busy.value = true
  note.value = null
  try {
    await api.deleteProject(p.id)
    confirmDelete.value = null
    emit('changed')
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

async function sweep() {
  if (!props.current) return
  busy.value = true
  note.value = null
  try {
    const r = await api.sweep(props.current.id)
    const mb = (r.bytesFreed / 1048576).toFixed(1)
    note.value = {
      tone: 'ok',
      text: r.bytesFreed ? `Freed ${mb} MB.` : 'Nothing to reclaim — the worktrees are clean.',
    }
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="flex flex-col gap-4 lg:flex-row lg:items-start">
    <!-- The list. Full width on a phone, a column beside the detail above lg. -->
    <div class="flex shrink-0 flex-col gap-1 lg:w-64">
      <button
        v-for="p in projects"
        :key="p.id"
        type="button"
        :class="[
          'focus-visible:outline-ring flex items-center gap-2 border px-2.5 py-2 text-left text-xs transition-colors focus-visible:outline-2',
          current?.id === p.id
            ? 'border-primary/50 bg-primary/[0.08] text-foreground'
            : 'hover:bg-muted text-muted-foreground border-transparent',
        ]"
        @click="emit('open', p)"
      >
        <FolderGit2 :size="14" class="shrink-0" aria-hidden="true" />
        <span class="min-w-0">
          <span class="block truncate font-medium">{{ p.name }}</span>
          <span class="text-muted-foreground block truncate text-[10px]">{{ p.path }}</span>
        </span>
      </button>

      <p v-if="!projects.length" class="text-muted-foreground px-1 py-3 text-[11px]">
        No projects yet.
      </p>

      <Button variant="outline" size="sm" class="mt-1 gap-1.5" @click="adding = true">
        <Plus :size="14" aria-hidden="true" />
        Add a project
      </Button>
    </div>

    <!-- The detail. -->
    <div class="min-w-0 flex-1">
      <p
        v-if="note"
        :class="[
          'mb-3 px-3 py-2 text-xs',
          note.tone === 'bad'
            ? 'bg-destructive/10 text-destructive'
            : 'bg-[var(--status-good)]/10 text-[var(--status-good)]',
        ]"
      >
        {{ note.text }}
      </p>

      <p v-if="!current" class="text-muted-foreground text-xs">
        Pick a project, or add one. Everything on this screen belongs to the one you pick.
      </p>

      <div v-else class="flex flex-col gap-4">
        <Card>
          <CardHeader>
            <CardTitle class="text-sm">{{ current.name }}</CardTitle>
            <CardDescription class="font-mono text-[11px] break-all">
              {{ current.path }} · {{ current.baseBranch }}
            </CardDescription>
          </CardHeader>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle class="text-sm">Where finished work goes</CardTitle>
            <CardDescription class="text-[11px]">
              What the last role does when it approves. Per project, not per role: only the last
              role integrates, and which role that is changes when you change the team.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <RadioGroup
              :model-value="current.integration"
              :disabled="busy"
              class="gap-3"
              @update:model-value="(v) => setIntegration(v as Integration)"
            >
              <div v-for="opt in INTEGRATIONS" :key="opt.value" class="flex items-start gap-2">
                <RadioGroupItem :id="`int-${opt.value}`" :value="opt.value" class="mt-0.5" />
                <Label :for="`int-${opt.value}`" class="cursor-pointer text-xs font-normal">
                  {{ opt.label }}
                  <span class="text-muted-foreground block text-[11px] leading-snug">
                    {{ opt.why }}
                  </span>
                </Label>
              </div>
            </RadioGroup>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle class="text-sm">Disk</CardTitle>
            <CardDescription class="text-[11px]">
              Removes files this project's own .gitignore already calls disposable, from every
              role's worktree. Never untracked work. The policy lives in Settings; this runs it now.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Button size="sm" variant="outline" :disabled="busy" @click="sweep">
              Sweep worktrees now
            </Button>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle class="text-destructive text-sm">Remove from zerg</CardTitle>
            <CardDescription class="text-[11px]">
              Forgets the project, its team, its tasks and their history. The repository itself is
              left exactly as it is, worktrees included.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Button size="sm" variant="destructive" class="gap-1.5" @click="confirmDelete = current">
              <Trash2 :size="14" aria-hidden="true" />
              Remove
            </Button>
          </CardContent>
        </Card>
      </div>
    </div>

    <!-- Add -->
    <Dialog v-model:open="adding">
      <DialogContent class="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a project</DialogTitle>
          <DialogDescription>
            Point zerg at a git repository. It starts with the default team; everything else is a
            checkbox in Team.
          </DialogDescription>
        </DialogHeader>
        <div class="flex flex-col gap-3">
          <div class="flex flex-col gap-1.5">
            <Label>Path</Label>
            <Input v-model="newPath" placeholder="/Users/you/source/your-repo" autofocus />
          </div>
          <div class="flex flex-col gap-1.5">
            <Label>Base branch</Label>
            <Input v-model="newBranch" />
            <span class="text-muted-foreground text-[11px]">
              Left empty, zerg uses the repository's current branch.
            </span>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" @click="adding = false">Cancel</Button>
          <Button :disabled="!newPath.trim() || busy" @click="add">Add</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <!-- Removing is destructive and permanent for zerg's own record of the
         work, so it asks, and says exactly what survives. -->
    <Dialog :open="!!confirmDelete" @update:open="(v) => !v && (confirmDelete = null)">
      <DialogContent class="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Remove {{ confirmDelete?.name }}?</DialogTitle>
          <DialogDescription>
            Its tasks, their history and their costs are deleted from zerg. Nothing is done to the
            repository at
            <code class="break-all">{{ confirmDelete?.path }}</code
            >, and the worktrees stay where they are.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" @click="confirmDelete = null">Cancel</Button>
          <Button variant="destructive" :disabled="busy" @click="remove(confirmDelete!)">
            Remove
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>

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
import { computed, ref, useId, watch } from 'vue'
import { latest } from '@/lib/latest'
import { Plus, Trash2 } from '@lucide/vue'
import { api, type Integration, type Project } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import ProjectAvatar from '@/components/ProjectAvatar.vue'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
} from '@/components/ui/select'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

// Every field's label points at the control it names. Without the pairing a
// screen reader reads an unlabelled control, and clicking the word does not
// focus the field it sits above.
const pathId = useId()
const branchId = useId()
const nameId = useId()
const iconId = useId()
const draftId = useId()

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

/**
 * What the repository already carries.
 *
 * Asked of the daemon rather than guessed at here: it is the side with the
 * filesystem, and "which of these files is the logo" is a question about a
 * directory, not about a project record.
 */
/**
 * The value that means "no icon, draw initials".
 *
 * A sentinel rather than the empty string, which is what this actually stores:
 * reka reserves "" to mean "nothing is selected, show the placeholder" and
 * refuses it as an item value outright. Safe as a sentinel because a stored
 * icon is always a path ending in an image extension, so this can never
 * collide with a real one.
 */
const NO_ICON = 'none:initials'

const candidates = ref<{ path: string; bytes: number }[]>([])
const scanning = ref(false)
const newestScan = latest()

async function loadIcons() {
  if (!props.current) {
    candidates.value = []
    return
  }
  const current = newestScan()
  scanning.value = true
  try {
    const r = await api.projectIcons(props.current.id)
    if (!current()) return
    candidates.value = r.candidates
  } catch {
    if (!current()) return
    candidates.value = []
  } finally {
    if (current()) scanning.value = false
  }
}
watch(() => props.current?.id, loadIcons, { immediate: true })

/**
 * The name is a label, not an identity — the path is what makes a project one
 * project — so it is safe to change and nothing behind it moves. Saved on blur
 * or Enter rather than behind a button: there is one field, and a Save next to
 * one field is a click asking to be forgotten.
 */
const nameDraft = ref('')
watch(
  () => props.current,
  (p) => (nameDraft.value = p?.name ?? ''),
  { immediate: true },
)

async function rename() {
  const name = nameDraft.value.trim()
  if (!props.current || !name || name === props.current.name) {
    nameDraft.value = props.current?.name ?? ''
    return
  }
  busy.value = true
  note.value = null
  try {
    emit('updated', await api.renameProject(props.current.id, name))
    emit('changed')
  } catch (e) {
    fail(e)
    nameDraft.value = props.current.name
  } finally {
    busy.value = false
  }
}

async function setIcon(icon: string) {
  if (!props.current || icon === (props.current.icon ?? '')) return
  busy.value = true
  note.value = null
  try {
    emit('updated', await api.setProjectIcon(props.current.id, icon))
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

/**
 * What to call a candidate on its tile.
 *
 * The file name, because that is what distinguishes monogram_teal.svg from
 * monogram_black.svg and truncating the path cuts off exactly that. But a
 * monorepo has a favicon.svg per app, and three tiles reading "favicon.svg" is
 * the same problem again — so a name that is not unique carries the directory
 * that makes it so.
 */
const iconLabels = computed(() => {
  const count = new Map<string, number>()
  for (const c of candidates.value) {
    const base = c.path.split('/').pop() ?? c.path
    count.set(base, (count.get(base) ?? 0) + 1)
  }
  const out = new Map<string, string>()
  for (const c of candidates.value) {
    const parts = c.path.split('/')
    const base = parts.pop() ?? c.path
    out.set(c.path, (count.get(base) ?? 0) > 1 ? `${parts.slice(-1)[0] ?? ''}/${base}` : base)
  }
  return out
})

/** The URL a candidate previews at: the same endpoint the avatar uses, asked
 *  for a path this project has not necessarily chosen yet. */
function previewURL(path: string): string {
  return `/api/projects/${props.current?.id}/icon?p=${encodeURIComponent(path)}&preview=${encodeURIComponent(path)}`
}

async function saveIntegration(mode: Integration, prDraft: boolean) {
  if (!props.current) return
  busy.value = true
  note.value = null
  try {
    emit('updated', await api.setIntegration(props.current.id, mode, prDraft))
    note.value = { tone: 'ok', text: 'Saved. It applies to the next task that finishes.' }
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

function setIntegration(mode: Integration) {
  if (!props.current || props.current.integration === mode) return
  return saveIntegration(mode, props.current.prDraft)
}

function setDraft(prDraft: boolean) {
  if (!props.current || props.current.prDraft === prDraft) return
  return saveIntegration(props.current.integration, prDraft)
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
      text: r.bytesFreed ? `Freed ${mb} MB.` : 'Nothing to reclaim, the worktrees are clean.',
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
        <!-- The same mark the switcher shows, so the two lists are one list. -->
        <ProjectAvatar :project="p" size="sm" />
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
            <div class="flex items-start gap-3">
              <ProjectAvatar :project="current" />
              <div class="min-w-0">
                <CardTitle class="text-sm">{{ current.name }}</CardTitle>
                <CardDescription class="font-mono text-[11px] break-all">
                  {{ current.path }} · {{ current.baseBranch }}
                </CardDescription>
              </div>
            </div>
          </CardHeader>
          <CardContent class="flex flex-col gap-4">
            <div class="flex max-w-sm flex-col gap-1.5">
              <Label :for="nameId" class="text-[11px]">Name</Label>
              <Input
                :id="nameId"
                v-model="nameDraft"
                class="h-8 text-xs"
                maxlength="120"
                :disabled="busy"
                @blur="rename"
                @keyup.enter="rename"
              />
              <p class="text-muted-foreground text-[11px] leading-snug">
                What the switcher and the board call it. The repository is identified by its path,
                so renaming changes nothing else, including the derived mark's colour.
              </p>
            </div>

            <div class="flex max-w-sm flex-col gap-1.5">
              <Label :for="iconId" class="text-[11px]">Icon</Label>
              <!-- A list, not a grid of tiles. Twenty-five marks laid out as
                   thumbnails is a wall to scan rather than a choice to make,
                   and the one already picked was lost among them. Closed, this
                   shows exactly what is in use. -->
              <Select
                :model-value="current.icon || NO_ICON"
                :disabled="busy || scanning"
                @update:model-value="(v) => setIcon(v === NO_ICON ? '' : String(v ?? ''))"
              >
                <SelectTrigger :id="iconId" class="h-auto w-full justify-start gap-2 py-1.5 data-[size=default]:h-auto">
                  <ProjectAvatar :project="current" size="sm" />
                  <span class="min-w-0 flex-1 truncate text-left font-mono text-[11px]">
                    {{ current.icon ? iconLabels.get(current.icon) ?? current.icon : 'initials' }}
                  </span>
                </SelectTrigger>
                <SelectContent position="popper" align="start" class="max-h-72">
                  <!-- The way back to the derived mark is one of the options,
                       not a button parked beside them: "no icon" is a choice
                       about the same thing every other row is about. -->
                  <SelectItem :value="NO_ICON" class="py-1.5">
                    <span class="flex min-w-0 items-center gap-2">
                      <ProjectAvatar :project="{ ...current, icon: '' }" size="sm" />
                      <span class="truncate text-[11px]">initials, drawn from the project</span>
                    </span>
                  </SelectItem>
                  <SelectItem v-for="c in candidates" :key="c.path" :value="c.path" class="py-1.5">
                    <span class="flex min-w-0 items-center gap-2">
                      <img
                        :src="previewURL(c.path)"
                        alt=""
                        class="size-6 shrink-0 object-contain"
                      />
                      <span class="truncate font-mono text-[11px]" :title="c.path">
                        {{ iconLabels.get(c.path) }}
                      </span>
                    </span>
                  </SelectItem>
                </SelectContent>
              </Select>
            </div>

            <p class="text-muted-foreground -mt-2 text-[11px] leading-snug">
              <template v-if="scanning">Looking through the repository…</template>
              <template v-else-if="candidates.length">
                {{ candidates.length }} image{{ candidates.length === 1 ? '' : 's' }} found in this
                repository. Read from the file each time, so editing it in the repository changes
                what the cockpit shows.
              </template>
              <template v-else>
                Nothing icon-shaped in this repository. zerg looks for favicons, logos and app
                icons. Initials on a colour taken from the project are used instead, which are
                distinct without being configured and stable across renames.
              </template>
            </p>
          </CardContent>
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

            <div
              v-if="current.integration === 'pr'"
              class="bg-muted/40 mt-4 flex items-start gap-2 border px-3 py-2.5"
            >
              <Checkbox
                :id="draftId"
                :model-value="current.prDraft"
                :disabled="busy"
                class="mt-0.5"
                @update:model-value="setDraft(Boolean($event))"
              />
              <Label :for="draftId" class="cursor-pointer text-xs font-normal">
                Create pull requests as drafts
                <span class="text-muted-foreground block text-[11px] leading-snug">
                  Drafts stay out of the review queue until someone marks them ready. Existing pull
                  requests are never changed by this setting.
                </span>
              </Label>
            </div>
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
            Point zerg at a git repository. It starts with the reusable Default team; choose or
            customize another one in Team.
          </DialogDescription>
        </DialogHeader>
        <div class="flex flex-col gap-3">
          <div class="flex flex-col gap-1.5">
            <Label :for="pathId">Path</Label>
            <Input :id="pathId" v-model="newPath" placeholder="/Users/you/source/your-repo" autofocus />
          </div>
          <div class="flex flex-col gap-1.5">
            <Label :for="branchId">Base branch</Label>
            <Input :id="branchId" v-model="newBranch" />
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

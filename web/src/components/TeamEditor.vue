<script setup lang="ts">
import { computed, ref, useId, watch } from 'vue'
import { joinArgs, splitArgs } from '@/lib/args'
import type {
  Model,
  ProjectRole,
  ProjectTeam,
  ProjectTeamUpdate,
  ResolvedRole,
  RoleOverrides,
  RoleTemplate,
  TeamPreset,
  TeamPresetRole,
} from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import ModelPicker from '@/components/ModelPicker.vue'
import RoleOverrideDialog from '@/components/RoleOverrideDialog.vue'
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

const props = defineProps<{
  library: RoleTemplate[]
  presets: TeamPreset[]
  projectTeam: ProjectTeam
  harnesses: string[]
  models: Record<string, Model[]>
  running: boolean
}>()
const emit = defineEmits<{
  setTeam: [team: ProjectTeamUpdate]
  savePreset: [preset: TeamPreset]
  createPreset: [name: string, roles: TeamPresetRole[]]
  deletePreset: [id: string]
  saveRole: [role: RoleTemplate]
}>()

const CUSTOM_TEAM = '__custom__'
const selectedPresetId = ref('')
watch(
  () => [props.projectTeam.presetId, props.presets] as const,
  ([projectPreset, presets]) => {
    if (selectedPresetId.value && presets.some((p) => p.id === selectedPresetId.value)) return
    selectedPresetId.value = projectPreset ?? presets[0]?.id ?? ''
  },
  { immediate: true },
)

const activePreset = computed(() => props.presets.find((p) => p.id === selectedPresetId.value) ?? null)
const projectPreset = computed(() => props.presets.find((p) => p.id === props.projectTeam.presetId) ?? null)
const libraryById = computed(() => new Map(props.library.map((r) => [r.id, r])))
const projectSelectedIds = computed(() => new Set(props.projectTeam.roles.map((r) => r.id)))
const presetSelectedIds = computed(() => new Set(activePreset.value?.roles.map((r) => r.templateId) ?? []))

function cloneOverrides(source: Partial<RoleOverrides>): RoleOverrides {
  return {
    harnessOverride: source.harnessOverride ?? null,
    modelOverride: source.modelOverride ?? null,
    argsOverride: source.argsOverride == null ? null : [...source.argsOverride],
    receiveOverride: source.receiveOverride ?? null,
    batchMaxItemsOverride: source.batchMaxItemsOverride ?? null,
    batchMaxAgeSecOverride: source.batchMaxAgeSecOverride ?? null,
    promptOverride: source.promptOverride ?? null,
    gateOverride: source.gateOverride ?? null,
  }
}

function hasRoleOverrides(o: Partial<RoleOverrides>) {
  return (
    o.harnessOverride != null ||
    o.modelOverride != null ||
    o.argsOverride != null ||
    o.receiveOverride != null ||
    o.batchMaxItemsOverride != null ||
    o.batchMaxAgeSecOverride != null ||
    o.promptOverride != null ||
    o.gateOverride != null
  )
}

function apply(base: RoleTemplate, o: Partial<RoleOverrides>): RoleTemplate {
  return {
    ...base,
    harness: o.harnessOverride ?? base.harness,
    model: o.modelOverride ?? base.model,
    args: o.argsOverride == null ? [...base.args] : [...o.argsOverride],
    receive: o.receiveOverride ?? base.receive,
    batchMaxItems: o.batchMaxItemsOverride ?? base.batchMaxItems,
    batchMaxAgeSec: o.batchMaxAgeSecOverride ?? base.batchMaxAgeSec,
    prompt: o.promptOverride ?? base.prompt,
    gate: o.gateOverride ?? base.gate,
  }
}

function asProjectRole(r: ResolvedRole): ProjectRole {
  return {
    templateId: r.id,
    position: r.position,
    enabled: r.enabled,
    ...cloneOverrides(r),
  }
}

function submitProject(roles: ProjectRole[], topologyOverride = props.projectTeam.topologyOverride, presetId = props.projectTeam.presetId) {
  emit('setTeam', { presetId, topologyOverride, roles })
}

function selectProjectPreset(value: unknown) {
  const id = String(value ?? '')
  if (!id) return
  if (id === CUSTOM_TEAM) {
    submitProject(props.projectTeam.roles.map(asProjectRole), true, null)
    return
  }
  if (id === props.projectTeam.presetId && !props.projectTeam.topologyOverride) return
  // Selecting another preset intentionally clears local settings. Keeping raw
  // overrides from a different baseline is more surprising than starting clean.
  submitProject([], false, id)
}

function customizeTopology() {
  submitProject(props.projectTeam.roles.map(asProjectRole), true)
}

function usePresetTopology() {
  const allowed = new Set(
    props.presets.find((p) => p.id === props.projectTeam.presetId)?.roles.map((r) => r.templateId) ?? [],
  )
  submitProject(
    props.projectTeam.roles.filter((r) => allowed.has(r.id)).map(asProjectRole),
    false,
  )
}

function toggleProject(tpl: RoleTemplate) {
  const roles = props.projectTeam.roles.map(asProjectRole)
  const next = projectSelectedIds.value.has(tpl.id)
    ? roles.filter((r) => r.templateId !== tpl.id)
    : [...roles, { templateId: tpl.id, enabled: true, ...cloneOverrides({}) }]
  submitProject(next, true)
}

function moveProject(index: number, delta: number) {
  const next = props.projectTeam.roles.map(asProjectRole)
  const target = index + delta
  if (target < 0 || target >= next.length) return
  ;[next[index], next[target]] = [next[target], next[index]]
  submitProject(next, true)
}

function setProjectEnabled(role: ResolvedRole, enabled: boolean) {
  submitProject(
    props.projectTeam.roles.map((r) => ({ ...asProjectRole(r), enabled: r.id === role.id ? enabled : r.enabled })),
    true,
  )
}

const projectRoleOpen = ref(false)
const projectRole = ref<ResolvedRole | null>(null)
const projectInherited = ref<RoleTemplate | null>(null)

function editProjectRole(role: ResolvedRole) {
  const template = libraryById.value.get(role.id)
  if (!template) return
  const presetRole = props.presets
    .find((p) => p.id === props.projectTeam.presetId)
    ?.roles.find((r) => r.templateId === role.id)
  projectRole.value = role
  projectInherited.value = presetRole ? apply(template, presetRole) : { ...template, args: [...template.args] }
  projectRoleOpen.value = true
}

function saveProjectRole(overrides: RoleOverrides) {
  if (!projectRole.value) return
  submitProject(
    props.projectTeam.roles.map((r) =>
      r.id === projectRole.value?.id ? { ...asProjectRole(r), ...overrides } : asProjectRole(r),
    ),
  )
}

function presetEffective(role: TeamPresetRole): RoleTemplate | null {
  const template = libraryById.value.get(role.templateId)
  return template ? apply(template, role) : null
}

function updatePreset(roles: TeamPresetRole[], name = activePreset.value?.name ?? '') {
  if (!activePreset.value) return
  emit('savePreset', { ...activePreset.value, name, roles })
}

function togglePreset(tpl: RoleTemplate) {
  if (!activePreset.value) return
  const roles = activePreset.value.roles.map((r) => ({ ...r, ...cloneOverrides(r) }))
  const next = presetSelectedIds.value.has(tpl.id)
    ? roles.filter((r) => r.templateId !== tpl.id)
    : [...roles, { templateId: tpl.id, position: roles.length, enabled: true, ...cloneOverrides({}) }]
  updatePreset(next)
}

function movePreset(index: number, delta: number) {
  if (!activePreset.value) return
  const next = activePreset.value.roles.map((r) => ({ ...r, ...cloneOverrides(r) }))
  const target = index + delta
  if (target < 0 || target >= next.length) return
  ;[next[index], next[target]] = [next[target], next[index]]
  updatePreset(next)
}

function setPresetEnabled(role: TeamPresetRole, enabled: boolean) {
  if (!activePreset.value) return
  updatePreset(activePreset.value.roles.map((r) => ({ ...r, enabled: r.templateId === role.templateId ? enabled : r.enabled })))
}

const presetRoleOpen = ref(false)
const presetRoleSource = ref<TeamPresetRole | null>(null)
const presetRoleEffective = ref<RoleTemplate | null>(null)
const presetRoleInherited = ref<RoleTemplate | null>(null)

function editPresetRole(role: TeamPresetRole) {
  const template = libraryById.value.get(role.templateId)
  if (!template) return
  presetRoleSource.value = role
  presetRoleInherited.value = { ...template, args: [...template.args] }
  presetRoleEffective.value = apply(template, role)
  presetRoleOpen.value = true
}

function savePresetRole(overrides: RoleOverrides) {
  if (!activePreset.value || !presetRoleSource.value) return
  updatePreset(
    activePreset.value.roles.map((r) =>
      r.templateId === presetRoleSource.value?.templateId ? { ...r, ...overrides } : r,
    ),
  )
}

const presetName = ref('')
watch(activePreset, (p) => (presetName.value = p?.name ?? ''), { immediate: true })
function renamePreset() {
  const name = presetName.value.trim()
  if (activePreset.value && name && name !== activePreset.value.name) updatePreset(activePreset.value.roles, name)
}

const creating = ref(false)
const newPresetName = ref('')
function createPreset() {
  const name = newPresetName.value.trim()
  if (!name) return
  const roles = (activePreset.value?.roles ?? []).map((r, position) => ({
    ...r,
    position,
    ...cloneOverrides(r),
  }))
  emit('createPreset', name, roles)
  newPresetName.value = ''
  creating.value = false
}

// The role library remains the lowest layer. Editing it is deliberately
// separate from editing a preset or project override.
const libraryOpen = ref(false)
const editingLibrary = ref<RoleTemplate | null>(null)
const libraryArgs = ref('')
const nameId = useId()
const harnessId = useId()
const argsId = useId()
const receiveId = useId()
const gateId = useId()
const batchItemsId = useId()
const batchAgeId = useId()
const promptId = useId()

function editLibraryRole(tpl: RoleTemplate) {
  editingLibrary.value = { ...tpl, args: [...tpl.args] }
  libraryArgs.value = joinArgs(tpl.args)
  libraryOpen.value = true
}

function saveLibraryRole() {
  if (!editingLibrary.value) return
  emit('saveRole', { ...editingLibrary.value, args: splitArgs(libraryArgs.value) })
  libraryOpen.value = false
}
</script>

<template>
  <Tabs default-value="presets" class="gap-5">
    <TabsList variant="line" class="w-full justify-start border-b bg-transparent p-0">
      <TabsTrigger value="presets" class="max-w-44 flex-none px-3 py-2.5">
        Reusable teams
      </TabsTrigger>
      <TabsTrigger value="project" class="max-w-44 flex-none px-3 py-2.5">
        This project
        <Badge v-if="projectTeam.topologyOverride" variant="secondary" class="ml-1">custom</Badge>
      </TabsTrigger>
    </TabsList>

    <!-- The common path: build and maintain the teams projects can select. -->
    <TabsContent value="presets" class="flex flex-col gap-4">
      <section class="border bg-card">
        <div class="flex flex-col gap-3 p-4 lg:flex-row lg:items-end">
          <div class="flex min-w-64 flex-1 flex-col gap-1.5">
            <Label for="managed-team">Reusable team</Label>
            <Select v-model="selectedPresetId">
              <SelectTrigger id="managed-team"><SelectValue placeholder="Choose a team" /></SelectTrigger>
              <SelectContent>
                <SelectItem v-for="preset in presets" :key="preset.id" :value="preset.id">
                  {{ preset.name }}
                </SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div v-if="activePreset" class="flex min-w-64 flex-1 flex-col gap-1.5">
            <div class="flex items-center gap-2">
              <Label for="managed-team-name">Name</Label>
              <Badge v-if="activePreset.builtin" variant="outline">built-in</Badge>
            </div>
            <Input
              id="managed-team-name"
              v-model="presetName"
              @keyup.enter="renamePreset"
              @blur="renamePreset"
            />
          </div>

          <div class="flex gap-2">
            <Button variant="outline" @click="creating = true">Duplicate team</Button>
            <Button
              v-if="activePreset && !activePreset.builtin"
              variant="destructive"
              @click="emit('deletePreset', activePreset.id)"
            >
              Delete
            </Button>
          </div>
        </div>
      </section>

      <div v-if="activePreset" class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <section class="min-w-0 border bg-card">
          <div class="flex items-start justify-between gap-3 border-b px-4 py-3">
            <div>
              <h2 class="text-sm font-semibold">Pipeline</h2>
              <p class="text-muted-foreground mt-0.5 text-[11px]">
                Work flows from top to bottom. These settings become the live defaults for every
                project using {{ activePreset.name }}.
              </p>
            </div>
            <Badge variant="secondary">
              {{ activePreset.roles.length }} role{{ activePreset.roles.length === 1 ? '' : 's' }}
            </Badge>
          </div>

          <ol class="divide-y">
            <li
              v-for="(role, i) in activePreset.roles"
              :key="role.templateId"
              class="group flex items-center gap-3 px-4 py-3"
            >
              <span class="text-muted-foreground grid size-6 shrink-0 place-items-center border text-[11px]">
                {{ i + 1 }}
              </span>
              <Checkbox
                :model-value="role.enabled"
                :aria-label="`${libraryById.get(role.templateId)?.name ?? 'role'} enabled`"
                @update:model-value="setPresetEnabled(role, !role.enabled)"
              />
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-1.5">
                  <span :class="['text-xs font-medium', !role.enabled && 'line-through opacity-50']">
                    {{ libraryById.get(role.templateId)?.name ?? role.templateId }}
                  </span>
                  <Badge v-if="hasRoleOverrides(role)" variant="secondary">custom defaults</Badge>
                  <Badge v-if="presetEffective(role)?.gate === 'approval'" variant="outline">gate</Badge>
                </div>
                <p class="text-muted-foreground mt-0.5 truncate text-[11px]">
                  {{ presetEffective(role)?.harness }} · {{ presetEffective(role)?.model || 'harness default' }}
                  · {{ presetEffective(role)?.receive }}
                </p>
              </div>
              <Button size="sm" variant="outline" @click="editPresetRole(role)">Settings</Button>
              <div class="flex">
                <Button size="icon-sm" variant="ghost" :disabled="i === 0" aria-label="Move up" @click="movePreset(i, -1)">↑</Button>
                <Button
                  size="icon-sm"
                  variant="ghost"
                  :disabled="i === activePreset.roles.length - 1"
                  aria-label="Move down"
                  @click="movePreset(i, 1)"
                >↓</Button>
              </div>
              <Button
                size="sm"
                variant="ghost"
                class="text-muted-foreground hover:text-destructive"
                @click="togglePreset(libraryById.get(role.templateId)!)"
              >
                Remove
              </Button>
            </li>
            <li v-if="!activePreset.roles.length" class="px-6 py-12 text-center">
              <p class="text-sm font-medium">This team has no roles</p>
              <p class="text-muted-foreground mt-1 text-xs">Add one from the role library.</p>
            </li>
          </ol>
        </section>

        <aside class="border bg-card lg:self-start">
          <div class="border-b px-4 py-3">
            <h2 class="text-sm font-semibold">Role library</h2>
            <p class="text-muted-foreground mt-0.5 text-[11px]">
              Add roles to this team. Template edits change the lowest default everywhere.
            </p>
          </div>
          <ul class="divide-y">
            <li v-for="tpl in library" :key="tpl.id" class="flex items-center gap-2 px-3 py-2.5">
              <div class="min-w-0 flex-1">
                <p class="truncate text-xs font-medium">{{ tpl.name }}</p>
                <p class="text-muted-foreground truncate text-[10px]">
                  {{ tpl.harness }} · {{ tpl.model || 'default' }}
                </p>
              </div>
              <Button size="xs" variant="ghost" @click="editLibraryRole(tpl)">Template</Button>
              <Button
                size="xs"
                :variant="presetSelectedIds.has(tpl.id) ? 'secondary' : 'outline'"
                :disabled="presetSelectedIds.has(tpl.id)"
                @click="togglePreset(tpl)"
              >
                {{ presetSelectedIds.has(tpl.id) ? 'Added' : 'Add' }}
              </Button>
            </li>
          </ul>
        </aside>
      </div>

      <p v-else class="text-muted-foreground border px-4 py-10 text-center text-xs">
        Create a reusable team to begin.
      </p>
    </TabsContent>

    <!-- Project-specific work is intentionally separate from global defaults. -->
    <TabsContent value="project" force-mount class="flex flex-col gap-4 data-[state=inactive]:hidden">
      <section class="border bg-card">
        <div class="flex flex-col gap-3 p-4 lg:flex-row lg:items-end">
          <div class="flex min-w-64 flex-1 flex-col gap-1.5">
            <Label for="project-team-source">Team used by this project</Label>
            <Select
              :model-value="projectTeam.presetId ?? CUSTOM_TEAM"
              :disabled="running"
              @update:model-value="selectProjectPreset"
            >
              <SelectTrigger id="project-team-source"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem v-for="preset in presets" :key="preset.id" :value="preset.id">
                  {{ preset.name }}
                </SelectItem>
                <SelectItem :value="CUSTOM_TEAM">Standalone custom team</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div class="min-w-0 flex-[2]">
            <p class="text-xs font-medium">
              <template v-if="projectTeam.presetId && !projectTeam.topologyOverride">
                Following {{ projectPreset?.name ?? 'the reusable team' }}
              </template>
              <template v-else-if="projectTeam.presetId">Custom pipeline based on {{ projectPreset?.name }}</template>
              <template v-else>Standalone project team</template>
            </p>
            <p class="text-muted-foreground mt-1 text-[11px] leading-snug">
              Role settings can be changed per project without changing the reusable team. A custom
              pipeline also owns membership, order and enablement.
            </p>
          </div>

          <Button
            v-if="projectTeam.presetId && !projectTeam.topologyOverride"
            variant="outline"
            :disabled="running"
            @click="customizeTopology"
          >
            Customize pipeline
          </Button>
          <Button
            v-if="projectTeam.presetId && projectTeam.topologyOverride"
            variant="outline"
            :disabled="running"
            @click="usePresetTopology"
          >
            Follow team pipeline again
          </Button>
        </div>
      </section>

      <div class="grid gap-4" :class="projectTeam.topologyOverride && 'lg:grid-cols-[minmax(0,1fr)_20rem]'">
        <section class="min-w-0 border bg-card">
          <div class="flex items-start justify-between gap-3 border-b px-4 py-3">
            <div>
              <h2 class="text-sm font-semibold">Project pipeline</h2>
              <p class="text-muted-foreground mt-0.5 text-[11px]">
                Open Settings on a role to override its harness, model, prompt, arguments or policy.
              </p>
            </div>
            <Badge v-if="projectTeam.topologyOverride" variant="secondary">project-owned order</Badge>
          </div>

          <ol class="divide-y">
            <li
              v-for="(role, i) in projectTeam.roles"
              :key="role.id"
              class="flex items-center gap-3 px-4 py-3"
            >
              <span class="text-muted-foreground grid size-6 shrink-0 place-items-center border text-[11px]">
                {{ i + 1 }}
              </span>
              <Checkbox
                :model-value="role.enabled"
                :disabled="running || !projectTeam.topologyOverride"
                :aria-label="`${role.name} enabled`"
                @update:model-value="setProjectEnabled(role, !role.enabled)"
              />
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-1.5">
                  <span :class="['text-xs font-medium', !role.enabled && 'line-through opacity-50']">
                    {{ role.name }}
                  </span>
                  <Badge v-if="role.overridden" variant="secondary">project override</Badge>
                  <Badge v-if="role.terminal">terminal</Badge>
                </div>
                <p class="text-muted-foreground mt-0.5 truncate text-[11px]">
                  {{ role.harness }} · {{ role.model || 'harness default' }} · {{ role.receive }}
                </p>
              </div>
              <Button size="sm" variant="outline" @click="editProjectRole(role)">Settings</Button>
              <template v-if="projectTeam.topologyOverride">
                <div class="flex">
                  <Button size="icon-sm" variant="ghost" :disabled="running || i === 0" aria-label="Move up" @click="moveProject(i, -1)">↑</Button>
                  <Button
                    size="icon-sm"
                    variant="ghost"
                    :disabled="running || i === projectTeam.roles.length - 1"
                    aria-label="Move down"
                    @click="moveProject(i, 1)"
                  >↓</Button>
                </div>
                <Button
                  size="sm"
                  variant="ghost"
                  class="text-muted-foreground hover:text-destructive"
                  :disabled="running"
                  @click="toggleProject(libraryById.get(role.id)!)"
                >
                  Remove
                </Button>
              </template>
            </li>
            <li v-if="!projectTeam.roles.length" class="px-6 py-12 text-center">
              <p class="text-sm font-medium">This project has no roles</p>
              <p class="text-muted-foreground mt-1 text-xs">Add one before starting the swarm.</p>
            </li>
          </ol>
          <p v-if="running" class="text-muted-foreground border-t px-4 py-2.5 text-[11px]">
            Stop the swarm to change the pipeline. Settings are picked up on the next spawn.
          </p>
        </section>

        <aside v-if="projectTeam.topologyOverride" class="border bg-card lg:self-start">
          <div class="border-b px-4 py-3">
            <h2 class="text-sm font-semibold">Add a role</h2>
            <p class="text-muted-foreground mt-0.5 text-[11px]">Available library roles for this project.</p>
          </div>
          <ul class="divide-y">
            <li v-for="tpl in library" :key="tpl.id" class="flex items-center gap-2 px-3 py-2.5">
              <div class="min-w-0 flex-1">
                <p class="truncate text-xs font-medium">{{ tpl.name }}</p>
                <p class="text-muted-foreground truncate text-[10px]">{{ tpl.harness }} · {{ tpl.model }}</p>
              </div>
              <Button
                size="xs"
                :variant="projectSelectedIds.has(tpl.id) ? 'secondary' : 'outline'"
                :disabled="running || projectSelectedIds.has(tpl.id)"
                @click="toggleProject(tpl)"
              >
                {{ projectSelectedIds.has(tpl.id) ? 'Added' : 'Add' }}
              </Button>
            </li>
          </ul>
        </aside>
      </div>
    </TabsContent>
  </Tabs>

  <RoleOverrideDialog
    v-model:open="presetRoleOpen"
    :role="presetRoleEffective"
    :inherited="presetRoleInherited"
    scope="Reusable-team defaults"
    :harnesses="harnesses"
    :models="models"
    @save="savePresetRole"
  />
  <RoleOverrideDialog
    v-model:open="projectRoleOpen"
    :role="projectRole"
    :inherited="projectInherited"
    scope="Settings for this project only"
    :harnesses="harnesses"
    :models="models"
    @save="saveProjectRole"
  />

  <Dialog v-model:open="creating">
    <DialogContent class="sm:max-w-md">
      <DialogHeader>
        <DialogTitle>Duplicate reusable team</DialogTitle>
        <DialogDescription>
          Copies the selected pipeline and defaults. The new team can then evolve independently.
        </DialogDescription>
      </DialogHeader>
      <div class="flex flex-col gap-1.5">
        <Label for="new-team-name">Name</Label>
        <Input id="new-team-name" v-model="newPresetName" autofocus @keyup.enter="createPreset" />
      </div>
      <DialogFooter>
        <Button variant="outline" @click="creating = false">Cancel</Button>
        <Button :disabled="!newPresetName.trim()" @click="createPreset">Create team</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>

  <Dialog v-model:open="libraryOpen">
    <DialogContent v-if="editingLibrary" class="gap-0 overflow-hidden p-0 sm:max-w-2xl">
      <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
        <DialogTitle>{{ editingLibrary.name }} template</DialogTitle>
        <DialogDescription>
          This is the lowest default. Reusable teams and projects without an override follow it.
        </DialogDescription>
      </DialogHeader>
      <DialogBody class="grid gap-3 sm:grid-cols-2">
        <div class="flex flex-col gap-1.5">
          <Label :for="nameId">Name</Label>
          <Input :id="nameId" v-model="editingLibrary.name" />
        </div>
        <div class="flex flex-col gap-1.5">
          <Label :for="harnessId">Harness</Label>
          <Select v-model="editingLibrary.harness">
            <SelectTrigger :id="harnessId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="h in harnesses" :key="h" :value="h">{{ h }}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div class="flex flex-col gap-1.5">
          <ModelPicker v-model="editingLibrary.model" :models="models[editingLibrary.harness] ?? []" label="Model" />
        </div>
        <div class="flex flex-col gap-1.5">
          <Label :for="argsId">Arguments</Label>
          <Input :id="argsId" v-model="libraryArgs" />
        </div>
        <div class="flex flex-col gap-1.5">
          <Label :for="receiveId">Receive</Label>
          <Select v-model="editingLibrary.receive">
            <SelectTrigger :id="receiveId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="task">task</SelectItem>
              <SelectItem value="batch">batch</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div class="flex flex-col gap-1.5">
          <Label :for="gateId">Gate</Label>
          <Select v-model="editingLibrary.gate">
            <SelectTrigger :id="gateId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="none">none</SelectItem>
              <SelectItem value="approval">approval</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <template v-if="editingLibrary.receive === 'batch'">
          <div class="flex flex-col gap-1.5">
            <Label :for="batchItemsId">Batch max items</Label>
            <Input :id="batchItemsId" v-model.number="editingLibrary.batchMaxItems" type="number" min="1" />
          </div>
          <div class="flex flex-col gap-1.5">
            <Label :for="batchAgeId">Batch max age (seconds)</Label>
            <Input :id="batchAgeId" v-model.number="editingLibrary.batchMaxAgeSec" type="number" min="1" />
          </div>
        </template>
        <div class="flex flex-col gap-1.5 sm:col-span-2">
          <Label :for="promptId">Prompt</Label>
          <Textarea :id="promptId" v-model="editingLibrary.prompt" rows="12" />
        </div>
      </DialogBody>
      <DialogFooter class="hairline-t shrink-0 px-5 py-4">
        <Button variant="outline" @click="libraryOpen = false">Cancel</Button>
        <Button @click="saveLibraryRole">Save template</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

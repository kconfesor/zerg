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
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
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
  <div class="flex flex-col gap-4">
    <Card>
      <CardHeader>
        <CardTitle class="text-sm">This project's team</CardTitle>
        <CardDescription class="text-[11px]">
          Pick a reusable team, then override only what this repository needs. Unchanged settings
          keep following the reusable team.
        </CardDescription>
      </CardHeader>
      <CardContent class="flex flex-wrap items-end gap-3">
        <div class="flex min-w-56 flex-col gap-1.5">
          <Label for="project-team-source">Team</Label>
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
              <SelectItem :value="CUSTOM_TEAM">Custom project team</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <Badge v-if="projectTeam.topologyOverride" variant="secondary">custom pipeline</Badge>
        <Button
          v-if="projectTeam.presetId && !projectTeam.topologyOverride"
          size="sm"
          variant="outline"
          :disabled="running"
          @click="customizeTopology"
        >
          Customize pipeline
        </Button>
        <Button
          v-if="projectTeam.presetId && projectTeam.topologyOverride"
          size="sm"
          variant="outline"
          :disabled="running"
          @click="usePresetTopology"
        >
          Use preset pipeline
        </Button>
      </CardContent>
    </Card>

    <div class="grid gap-4 xl:grid-cols-2">
      <Card>
        <CardHeader>
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div>
              <CardTitle class="text-sm">Reusable team defaults</CardTitle>
              <CardDescription class="text-[11px]">
                Managed once and shared by every project that selects it.
              </CardDescription>
            </div>
            <Button size="sm" variant="outline" @click="creating = true">Duplicate as new</Button>
          </div>
        </CardHeader>
        <CardContent class="flex flex-col gap-3">
          <Select v-model="selectedPresetId">
            <SelectTrigger><SelectValue placeholder="Choose a team" /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="preset in presets" :key="preset.id" :value="preset.id">
                {{ preset.name }}
              </SelectItem>
            </SelectContent>
          </Select>

          <div v-if="activePreset" class="flex gap-2">
            <Input v-model="presetName" @keyup.enter="renamePreset" @blur="renamePreset" />
            <Button
              v-if="!activePreset.builtin"
              variant="destructive"
              size="sm"
              @click="emit('deletePreset', activePreset.id)"
            >
              Delete
            </Button>
          </div>

          <div v-if="activePreset" class="grid gap-3 lg:grid-cols-2">
            <div class="border">
              <div class="border-b px-3 py-2 text-xs font-semibold">Role library</div>
              <ul class="divide-y">
                <li v-for="tpl in library" :key="tpl.id" class="flex items-center gap-2 px-3 py-2">
                  <Checkbox
                    :model-value="presetSelectedIds.has(tpl.id)"
                    @update:model-value="togglePreset(tpl)"
                  />
                  <span class="text-xs font-medium">{{ tpl.name }}</span>
                  <Button size="xs" variant="ghost" class="ml-auto" @click="editLibraryRole(tpl)">
                    Library
                  </Button>
                </li>
              </ul>
            </div>

            <div class="border">
              <div class="border-b px-3 py-2 text-xs font-semibold">Preset pipeline</div>
              <ul class="divide-y">
                <li
                  v-for="(role, i) in activePreset.roles"
                  :key="role.templateId"
                  class="flex items-center gap-2 px-3 py-2"
                >
                  <span class="text-muted-foreground w-4 text-xs">{{ i + 1 }}</span>
                  <Checkbox
                    :model-value="role.enabled"
                    @update:model-value="setPresetEnabled(role, !role.enabled)"
                  />
                  <span class="text-xs font-medium">
                    {{ libraryById.get(role.templateId)?.name ?? role.templateId }}
                  </span>
                  <Button size="xs" variant="ghost" class="ml-auto" @click="editPresetRole(role)">
                    Settings
                  </Button>
                  <Button size="icon-xs" variant="ghost" :disabled="i === 0" @click="movePreset(i, -1)">↑</Button>
                  <Button
                    size="icon-xs"
                    variant="ghost"
                    :disabled="i === activePreset.roles.length - 1"
                    @click="movePreset(i, 1)"
                  >↓</Button>
                </li>
                <li v-if="!activePreset.roles.length" class="text-muted-foreground px-3 py-3 text-xs">
                  Pick roles from the library.
                </li>
              </ul>
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle class="text-sm">Project pipeline</CardTitle>
          <CardDescription class="text-[11px]">
            Settings edits stay in this project. Pipeline edits make the role list local.
          </CardDescription>
        </CardHeader>
        <CardContent class="flex flex-col gap-3">
          <div v-if="projectTeam.topologyOverride" class="border">
            <div class="border-b px-3 py-2 text-xs font-semibold">Available roles</div>
            <div class="flex flex-wrap gap-x-4 gap-y-2 p-3">
              <label v-for="tpl in library" :key="tpl.id" class="flex items-center gap-2 text-xs">
                <Checkbox
                  :model-value="projectSelectedIds.has(tpl.id)"
                  :disabled="running"
                  @update:model-value="toggleProject(tpl)"
                />
                {{ tpl.name }}
              </label>
            </div>
          </div>

          <div class="border">
            <div class="border-b px-3 py-2 text-xs font-semibold">Work flows top to bottom</div>
            <ul class="divide-y">
              <li
                v-for="(role, i) in projectTeam.roles"
                :key="role.id"
                class="flex items-center gap-2 px-3 py-2"
              >
                <span class="text-muted-foreground w-4 text-xs">{{ i + 1 }}</span>
                <Checkbox
                  :model-value="role.enabled"
                  :disabled="running"
                  @update:model-value="setProjectEnabled(role, !role.enabled)"
                />
                <span :class="['text-xs font-medium', !role.enabled && 'line-through opacity-50']">
                  {{ role.name }}
                </span>
                <span class="text-muted-foreground text-xs">{{ role.harness }} · {{ role.model || 'default' }}</span>
                <Badge v-if="role.overridden" variant="secondary">override</Badge>
                <Badge v-if="role.terminal">terminal</Badge>
                <Button size="xs" variant="ghost" class="ml-auto" @click="editProjectRole(role)">
                  Settings
                </Button>
                <template v-if="projectTeam.topologyOverride">
                  <Button size="icon-xs" variant="ghost" :disabled="running || i === 0" @click="moveProject(i, -1)">↑</Button>
                  <Button
                    size="icon-xs"
                    variant="ghost"
                    :disabled="running || i === projectTeam.roles.length - 1"
                    @click="moveProject(i, 1)"
                  >↓</Button>
                </template>
              </li>
              <li v-if="!projectTeam.roles.length" class="text-muted-foreground px-3 py-3 text-xs">
                This team has no roles.
              </li>
            </ul>
          </div>
          <p v-if="running" class="text-muted-foreground text-[11px]">
            Stop the swarm to change the pipeline. Role settings apply on the next spawn.
          </p>
        </CardContent>
      </Card>
    </div>
  </div>

  <RoleOverrideDialog
    v-model:open="presetRoleOpen"
    :role="presetRoleEffective"
    :inherited="presetRoleInherited"
    scope="These defaults belong to the reusable team"
    :harnesses="harnesses"
    :models="models"
    @save="savePresetRole"
  />
  <RoleOverrideDialog
    v-model:open="projectRoleOpen"
    :role="projectRole"
    :inherited="projectInherited"
    scope="These overrides belong only to this project"
    :harnesses="harnesses"
    :models="models"
    @save="saveProjectRole"
  />

  <Dialog v-model:open="creating">
    <DialogContent class="sm:max-w-md">
      <DialogHeader>
        <DialogTitle>New reusable team</DialogTitle>
        <DialogDescription>
          Starts as a copy of the selected team. Change its roles and defaults after creating it.
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
        <DialogTitle>{{ editingLibrary.name }}</DialogTitle>
        <DialogDescription>
          The role library is the lowest default. This can affect every preset and project.
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
        <Button @click="saveLibraryRole">Save library role</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

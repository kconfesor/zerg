<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Star } from '@lucide/vue'
import type {
  Model,
  ProjectTeam,
  ProjectTeamUpdate,
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
import RoleOverrideDialog from '@/components/RoleOverrideDialog.vue'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

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
}>()

const selectedPresetId = ref('')
const selectAfterCreate = ref('')

watch(
  () => [props.projectTeam.presetId, props.presets] as const,
  ([projectPreset, presets]) => {
    if (selectAfterCreate.value) {
      const created = presets.find((p) => p.name === selectAfterCreate.value)
      if (created) {
        selectedPresetId.value = created.id
        selectAfterCreate.value = ''
        return
      }
    }
    if (selectedPresetId.value && presets.some((p) => p.id === selectedPresetId.value)) return
    selectedPresetId.value = projectPreset ?? presets[0]?.id ?? ''
  },
  { immediate: true },
)

const activePreset = computed(() => props.presets.find((p) => p.id === selectedPresetId.value) ?? null)
const libraryById = computed(() => new Map(props.library.map((role) => [role.id, role])))
const selectedRoleIds = computed(
  () => new Set(activePreset.value?.roles.map((role) => role.templateId) ?? []),
)
const projectHasLocalChanges = computed(
  () => props.projectTeam.topologyOverride || props.projectTeam.roles.some((role) => role.overridden),
)
const terminalTemplateId = computed(() => {
  const roles = activePreset.value?.roles ?? []
  for (let i = roles.length - 1; i >= 0; i--) {
    if (roles[i].enabled) return roles[i].templateId
  }
  return ''
})

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

function hasRoleOverrides(overrides: Partial<RoleOverrides>) {
  return (
    overrides.harnessOverride != null ||
    overrides.modelOverride != null ||
    overrides.argsOverride != null ||
    overrides.receiveOverride != null ||
    overrides.batchMaxItemsOverride != null ||
    overrides.batchMaxAgeSecOverride != null ||
    overrides.promptOverride != null ||
    overrides.gateOverride != null
  )
}

function apply(base: RoleTemplate, overrides: Partial<RoleOverrides>): RoleTemplate {
  return {
    ...base,
    harness: overrides.harnessOverride ?? base.harness,
    model: overrides.modelOverride ?? base.model,
    args: overrides.argsOverride == null ? [...base.args] : [...overrides.argsOverride],
    receive: overrides.receiveOverride ?? base.receive,
    batchMaxItems: overrides.batchMaxItemsOverride ?? base.batchMaxItems,
    batchMaxAgeSec: overrides.batchMaxAgeSecOverride ?? base.batchMaxAgeSec,
    prompt: overrides.promptOverride ?? base.prompt,
    gate: overrides.gateOverride ?? base.gate,
  }
}

function effectiveRole(role: TeamPresetRole): RoleTemplate | null {
  const template = libraryById.value.get(role.templateId)
  return template ? apply(template, role) : null
}

function saveRoles(roles: TeamPresetRole[]) {
  const preset = activePreset.value
  if (!preset) return
  emit('savePreset', {
    ...preset,
    roles: roles.map((role, position) => ({ ...role, position })),
  })
}

function toggleRole(template: RoleTemplate) {
  const preset = activePreset.value
  if (!preset) return
  const roles = preset.roles.map((role) => ({ ...role, ...cloneOverrides(role) }))
  if (selectedRoleIds.value.has(template.id)) {
    saveRoles(roles.filter((role) => role.templateId !== template.id))
    return
  }
  saveRoles([
    ...roles,
    {
      templateId: template.id,
      position: roles.length,
      enabled: true,
      ...cloneOverrides({}),
    },
  ])
}

function moveRole(index: number, delta: number) {
  const preset = activePreset.value
  if (!preset) return
  const target = index + delta
  if (target < 0 || target >= preset.roles.length) return
  const roles = preset.roles.map((role) => ({ ...role, ...cloneOverrides(role) }))
  ;[roles[index], roles[target]] = [roles[target], roles[index]]
  saveRoles(roles)
}

const roleSettingsOpen = ref(false)
const editingPresetRole = ref<TeamPresetRole | null>(null)
const editingEffectiveRole = ref<RoleTemplate | null>(null)
const editingInheritedRole = ref<RoleTemplate | null>(null)

function editRoleSettings(template: RoleTemplate) {
  const presetRole = activePreset.value?.roles.find((role) => role.templateId === template.id)
  if (!presetRole) return
  editingPresetRole.value = presetRole
  editingInheritedRole.value = { ...template, args: [...template.args] }
  editingEffectiveRole.value = apply(template, presetRole)
  roleSettingsOpen.value = true
}

function saveRoleSettings(overrides: RoleOverrides) {
  const preset = activePreset.value
  const source = editingPresetRole.value
  if (!preset || !source) return
  saveRoles(
    preset.roles.map((role) =>
      role.templateId === source.templateId ? { ...role, ...overrides } : role,
    ),
  )
}

function useTeam(preset: TeamPreset) {
  selectedPresetId.value = preset.id
  emit('setTeam', {
    presetId: preset.id,
    topologyOverride: false,
    roles: [],
  })
}

const cloning = ref(false)
const cloneName = ref('')
const renaming = ref(false)
const renameName = ref('')
const renamingPresetId = ref('')
const confirmDelete = ref<TeamPreset | null>(null)

function openClone(preset: TeamPreset) {
  selectedPresetId.value = preset.id
  cloneName.value = `${preset.name} copy`
  cloning.value = true
}

function openRename(preset: TeamPreset) {
  selectedPresetId.value = preset.id
  renamingPresetId.value = preset.id
  renameName.value = preset.name
  renaming.value = true
}

function renameTeam() {
  const preset = props.presets.find((item) => item.id === renamingPresetId.value)
  const name = renameName.value.trim()
  if (!preset || preset.builtin || !name || name === preset.name) {
    renaming.value = false
    return
  }
  emit('savePreset', { ...preset, name })
  renaming.value = false
}

function deleteTeam() {
  const preset = confirmDelete.value
  if (!preset) return
  emit('deletePreset', preset.id)
  confirmDelete.value = null
}

function cloneTeam() {
  const preset = activePreset.value
  const name = cloneName.value.trim()
  if (!preset || !name) return
  selectAfterCreate.value = name
  emit(
    'createPreset',
    name,
    preset.roles.map((role, position) => ({
      ...role,
      position,
      ...cloneOverrides(role),
    })),
  )
  cloning.value = false
}
</script>

<template>
  <div class="grid min-h-[34rem] border bg-card lg:grid-cols-[22rem_minmax(18rem,0.9fr)_minmax(20rem,1.1fr)]">
    <!-- Column 1: choose the reusable team being viewed. -->
    <section class="min-w-0 border-b lg:border-r lg:border-b-0">
      <div class="border-b px-3 py-3">
        <h2 class="text-xs font-semibold uppercase tracking-wide">Teams</h2>
        <p class="text-muted-foreground mt-0.5 text-[10px]">Team setup</p>
      </div>

      <ul class="divide-y">
        <li
          v-for="preset in presets"
          :key="preset.id"
          :class="selectedPresetId === preset.id && 'bg-primary/[0.08]'"
        >
          <button
            type="button"
            class="focus-visible:outline-ring flex w-full items-center gap-2 px-3 pt-3 pb-2 text-left focus-visible:outline-2"
            @click="selectedPresetId = preset.id"
          >
            <Star
              v-if="projectTeam.presetId === preset.id"
              :size="15"
              class="text-primary fill-primary shrink-0"
              :aria-label="`${preset.name} is in use`"
            />
            <span v-else class="size-[15px] shrink-0" aria-hidden="true" />
            <span class="min-w-0 flex-1">
              <span class="block truncate text-xs font-medium">{{ preset.name }}</span>
              <span class="text-muted-foreground mt-0.5 block text-[10px]">
                {{ preset.roles.length }} role{{ preset.roles.length === 1 ? '' : 's' }}
              </span>
            </span>
          </button>
          <div class="flex items-center gap-1 px-3 pb-3 pl-9">
            <Button size="xs" variant="ghost" @click="openClone(preset)">Clone</Button>
            <Button
              size="xs"
              variant="ghost"
              :disabled="preset.builtin"
              @click="openRename(preset)"
            >
              Rename
            </Button>
            <Button
              size="xs"
              class="flex-none"
              style="width: 96px; min-width: 96px; max-width: 96px"
              :variant="projectTeam.presetId === preset.id ? 'secondary' : 'outline'"
              :disabled="running || (projectTeam.presetId === preset.id && !projectHasLocalChanges)"
              @click="useTeam(preset)"
            >
              {{ projectTeam.presetId === preset.id && !projectHasLocalChanges ? 'In use' : 'Use this Team' }}
            </Button>
          </div>
        </li>
        <li v-if="!presets.length" class="text-muted-foreground px-3 py-6 text-center text-xs">
          No teams yet.
        </li>
      </ul>

      <div v-if="activePreset && !activePreset.builtin" class="border-t p-3 lg:mt-auto">
        <Button
          size="sm"
          variant="ghost"
          class="text-muted-foreground hover:text-destructive w-full"
          @click="confirmDelete = activePreset"
        >
          Delete {{ activePreset.name }}
        </Button>
      </div>
    </section>

    <!-- Column 2: membership and per-team role settings. -->
    <section class="min-w-0 border-b lg:border-r lg:border-b-0">
      <div class="border-b px-4 py-3">
        <h2 class="text-xs font-semibold uppercase tracking-wide">Roles</h2>
        <p class="text-muted-foreground mt-0.5 text-[10px]">
          Add roles and configure how they run in {{ activePreset?.name ?? 'this team' }}
        </p>
      </div>

      <ul v-if="activePreset" class="divide-y">
        <li v-for="template in library" :key="template.id" class="flex items-center gap-3 px-4 py-3">
          <Checkbox
            :model-value="selectedRoleIds.has(template.id)"
            :aria-label="`${template.name} in ${activePreset.name}`"
            @update:model-value="toggleRole(template)"
          />
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5">
              <span class="truncate text-xs font-medium">{{ template.name }}</span>
              <Badge
                v-if="hasRoleOverrides(activePreset.roles.find((role) => role.templateId === template.id) ?? {})"
                variant="secondary"
              >
                custom
              </Badge>
            </div>
            <p class="text-muted-foreground mt-0.5 truncate text-[10px]">
              <template v-if="activePreset.roles.find((role) => role.templateId === template.id)">
                {{ effectiveRole(activePreset.roles.find((role) => role.templateId === template.id)!)?.harness }}
                · {{ effectiveRole(activePreset.roles.find((role) => role.templateId === template.id)!)?.model || 'default model' }}
              </template>
              <template v-else>Not in this team</template>
            </p>
          </div>
          <Button
            size="sm"
            variant="outline"
            :disabled="!selectedRoleIds.has(template.id)"
            @click="editRoleSettings(template)"
          >
            Settings
          </Button>
        </li>
      </ul>
      <p v-else class="text-muted-foreground px-4 py-10 text-center text-xs">
        Select a team first.
      </p>
    </section>

    <!-- Column 3: order and project assignment. -->
    <section class="flex min-w-0 flex-col">
      <div class="border-b px-4 py-3">
        <h2 class="text-xs font-semibold uppercase tracking-wide">Pipeline</h2>
        <p class="text-muted-foreground mt-0.5 text-[10px]">Work flows from top to bottom</p>
      </div>

      <ol v-if="activePreset" class="divide-y">
        <li
          v-for="(role, index) in activePreset.roles"
          :key="role.templateId"
          class="flex items-center gap-3 px-4 py-3"
        >
          <span class="text-muted-foreground grid size-6 shrink-0 place-items-center border text-[11px]">
            {{ index + 1 }}
          </span>
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-1.5">
              <span class="truncate text-xs font-medium">
                {{ libraryById.get(role.templateId)?.name ?? role.templateId }}
              </span>
              <Badge v-if="role.templateId === terminalTemplateId">terminal</Badge>
            </div>
            <p class="text-muted-foreground mt-0.5 truncate text-[10px]">
              {{ effectiveRole(role)?.harness }} · {{ effectiveRole(role)?.model || 'default model' }}
            </p>
          </div>
          <div class="flex">
            <Button
              size="icon-sm"
              variant="ghost"
              :disabled="index === 0"
              :aria-label="`Move ${libraryById.get(role.templateId)?.name} up`"
              @click="moveRole(index, -1)"
            >
              ↑
            </Button>
            <Button
              size="icon-sm"
              variant="ghost"
              :disabled="index === activePreset.roles.length - 1"
              :aria-label="`Move ${libraryById.get(role.templateId)?.name} down`"
              @click="moveRole(index, 1)"
            >
              ↓
            </Button>
          </div>
        </li>
        <li v-if="!activePreset.roles.length" class="px-6 py-14 text-center">
          <p class="text-sm font-medium">No roles selected</p>
          <p class="text-muted-foreground mt-1 text-xs">Check roles in the middle column.</p>
        </li>
      </ol>
    </section>
  </div>

  <RoleOverrideDialog
    v-model:open="roleSettingsOpen"
    :role="editingEffectiveRole"
    :inherited="editingInheritedRole"
    :scope="`${activePreset?.name ?? 'Team'} role settings`"
    :harnesses="harnesses"
    :models="models"
    @save="saveRoleSettings"
  />

  <Dialog v-model:open="cloning">
    <DialogContent class="sm:max-w-md">
      <DialogHeader>
        <DialogTitle>Clone {{ activePreset?.name }}</DialogTitle>
        <DialogDescription>
          Copies its roles, pipeline and settings. Edit the clone, then choose “Use this Team”.
        </DialogDescription>
      </DialogHeader>
      <div class="flex flex-col gap-1.5">
        <Label for="clone-team-name">Team name</Label>
        <Input id="clone-team-name" v-model="cloneName" autofocus @keyup.enter="cloneTeam" />
      </div>
      <DialogFooter>
        <Button variant="outline" @click="cloning = false">Cancel</Button>
        <Button :disabled="!cloneName.trim()" @click="cloneTeam">Clone team</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>

  <Dialog v-model:open="renaming">
    <DialogContent class="sm:max-w-md">
      <DialogHeader>
        <DialogTitle>Rename team</DialogTitle>
        <DialogDescription>
          Projects using this team keep using it. Only its display name changes.
        </DialogDescription>
      </DialogHeader>
      <div class="flex flex-col gap-1.5">
        <Label for="rename-team-name">Team name</Label>
        <Input id="rename-team-name" v-model="renameName" autofocus @keyup.enter="renameTeam" />
      </div>
      <DialogFooter>
        <Button variant="outline" @click="renaming = false">Cancel</Button>
        <Button :disabled="!renameName.trim()" @click="renameTeam">Rename team</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>

  <Dialog :open="!!confirmDelete" @update:open="(open) => !open && (confirmDelete = null)">
    <DialogContent class="sm:max-w-md">
      <DialogHeader>
        <DialogTitle>Delete {{ confirmDelete?.name }}?</DialogTitle>
        <DialogDescription>
          This permanently removes the reusable team and its role settings. A team currently used
          by a project must be replaced there before it can be deleted.
        </DialogDescription>
      </DialogHeader>
      <DialogFooter>
        <Button variant="outline" @click="confirmDelete = null">Cancel</Button>
        <Button variant="destructive" @click="deleteTeam">Delete team</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

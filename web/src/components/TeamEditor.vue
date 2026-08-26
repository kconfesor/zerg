<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { CircleCheck, Copy, Pencil, SlidersHorizontal, Star, Trash2 } from '@lucide/vue'
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
import { Switch } from '@/components/ui/switch'
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
  /** Why the last team action was refused, if it was. Rendered where it was
   *  pressed: the page banner behind this dialog is not visible on a phone. */
  actionError?: string
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

/**
 * Whether the team on screen is the one this project actually runs.
 *
 * The two middle columns render the *selected* preset, and the selection falls
 * back to the first team alphabetically when a project has none — which is
 * every project migrated from before teams existed. Editing there changes a
 * shared team used by every other project that adopted it, while the board in
 * front of you keeps running something else. Saying so is the difference
 * between a list and a trap.
 */
const editingSomethingElse = computed(
  () => !!activePreset.value && props.projectTeam.presetId !== activePreset.value.id,
)
const projectRunsItsOwn = computed(
  () => !props.projectTeam.presetId && props.projectTeam.roles.length > 0,
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

/**
 * A change that would stop a running agent asks first.
 *
 * Not a lock. The old editor disabled these controls whenever a swarm was up,
 * because a team edit then reached nobody until the next respawn — "stop the
 * swarm to change the pipeline" was the honest instruction. The daemon
 * reconciles live now: it spawns a role added to the team and stops one taken
 * off, within the second. Disabling the controls would give that away.
 *
 * What is left to protect is the surprise. Taking a role off a team, or turning
 * it off, kills an agent that may be mid-turn and returns its work to the
 * queue — and a team is shared, so it happens to every project using it, not
 * just the one on screen.
 */
type Pending = { question: string; detail?: string; confirm?: string; run: () => void }
const pending = ref<Pending | null>(null)

function guard(question: string, run: () => void, extra?: Omit<Pending, 'question' | 'run'>) {
  if (!props.running) {
    run()
    return
  }
  pending.value = { question, run, ...extra }
}

function toggleRole(template: RoleTemplate) {
  const preset = activePreset.value
  if (!preset) return
  const roles = preset.roles.map((role) => ({ ...role, ...cloneOverrides(role) }))
  if (selectedRoleIds.value.has(template.id)) {
    guard(`Take ${template.name} off ${preset.name}?`, () =>
      saveRoles(roles.filter((role) => role.templateId !== template.id)),
    )
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

/**
 * Park a role without taking it off the team.
 *
 * `enabled` has never stopped being read — terminality is the last *enabled*
 * role, and the overmind only spawns enabled ones — but the control that set it
 * was dropped, so a role stored disabled could not be turned back on from
 * anywhere. That includes every project migrated from before teams existed with
 * a disabled trailing role.
 *
 * Distinct from unchecking it in the Roles column, which removes it from the
 * team and takes its position and overrides with it.
 */
function setEnabled(templateId: string, enabled: boolean) {
  const preset = activePreset.value
  if (!preset) return
  const apply = () =>
    saveRoles(
      preset.roles.map((role) => ({
        ...role,
        ...cloneOverrides(role),
        enabled: role.templateId === templateId ? enabled : role.enabled,
      })),
    )
  // Turning one off stops a live agent, so it is asked about rather than done.
  if (!enabled) {
    guard(`Stop ${libraryById.value.get(templateId)?.name ?? 'this role'}?`, apply)
    return
  }
  apply()
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

/**
 * What the primary button on a team row says, or nothing at all.
 *
 * Three states, and only two of them are an action. A team this project does
 * not run can be adopted. A team it runs whose pipeline it has *overridden* can
 * be followed again — that is the one thing pressing this undoes, since field
 * overrides survive either way. A team it simply runs needs no button: the star
 * and the "in use" line already say so, and a disabled button reading "In use"
 * is a control that looks broken rather than a label that reads.
 */
function useLabel(preset: TeamPreset): string {
  if (props.projectTeam.presetId !== preset.id) return 'Use this team'
  return props.projectTeam.topologyOverride ? 'Follow this pipeline again' : ''
}

/**
 * Adopting a team while agents run asks, rather than refusing.
 *
 * This button used to disable itself whenever a swarm was up, which is the rule
 * the rest of this editor stopped following when the daemon learned to
 * reconcile: setTeam reconciles the running swarm, so the change lands in about
 * a second. What is left is the surprise — roles the new team does not have are
 * stopped — so it is a question, and only for this project, because a project's
 * team assignment is the one edit here that is not shared.
 */
function adopt(preset: TeamPreset) {
  guard(`Put this project on ${preset.name}?`, () => useTeam(preset), {
    detail:
      'Agents are running. Roles this team does not have are stopped and their work goes back to the queue; roles it adds start within a second or so. Only this project is affected.',
    confirm: 'Switch team',
  })
}

function useTeam(preset: TeamPreset) {
  selectedPresetId.value = preset.id
  // The project's own overrides come with it.
  //
  // SetProjectTeam replaces the override layer wholesale — it deletes every
  // project_role_overrides row and re-inserts from what it is sent — so an
  // empty array here does not mean "leave them alone", it means "delete them".
  // Pressing this on the team already in use is exactly when a project has
  // overrides worth keeping, including every one migration 013 carried across
  // from the old project_roles columns.
  //
  // Filtered to the preset's own roles: SetProjectTeam refuses an override for
  // a role the preset does not contain, and adopting a team that drops a role
  // legitimately drops that role's overrides with it.
  const inPreset = new Set(preset.roles.map((r) => r.templateId))
  const keep = props.projectTeam.roles
    .filter((role) => inPreset.has(role.id) && hasRoleOverrides(role))
    .map((role) => ({ templateId: role.id, enabled: role.enabled, ...cloneOverrides(role) }))
  emit('setTeam', {
    presetId: preset.id,
    topologyOverride: false,
    roles: keep,
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

/**
 * Ask, then wait for the team to actually be gone.
 *
 * Closing on the emit assumes the delete succeeded, and it need not: the daemon
 * refuses to delete a team a project still runs. The refusal then landed in the
 * page banner behind this dialog, which on a phone is nowhere. Closing when the
 * preset leaves the list instead is both the true signal and race-free — no
 * flag to reset, and a refusal simply leaves the dialog up with the reason in
 * it.
 */
function deleteTeam() {
  const preset = confirmDelete.value
  if (!preset) return
  emit('deletePreset', preset.id)
}

watch(
  () => props.presets,
  (presets) => {
    const going = confirmDelete.value
    if (going && !presets.some((p) => p.id === going.id)) confirmDelete.value = null
  },
)

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
  <!-- Which team the columns below are showing, when it is not the one this
       project runs. Amber and worded: nothing is broken, but an edit here lands
       somewhere other than where the reader is looking. -->
  <p
    v-if="editingSomethingElse"
    class="mb-3 flex items-start gap-2.5 border border-[var(--status-warning)]/40 bg-[var(--status-warning)]/[0.07] px-3 py-2.5 text-[11px] leading-relaxed"
  >
    <span
      class="text-background mt-px grid size-4 shrink-0 place-items-center rounded-full bg-[var(--status-warning)] text-[10px] font-bold"
      aria-hidden="true"
      >!</span
    >
    <span>
      You are looking at <b class="font-semibold">{{ activePreset?.name }}</b
      >,
      <template v-if="projectRunsItsOwn">
        which this project does not use — it runs a pipeline of its own. Changes here apply to every
        project that has adopted {{ activePreset?.name }}, not to this one.
      </template>
      <template v-else>
        which this project has not adopted. Changes here apply to every project that has.
      </template>
      Press <b class="font-semibold">Use this Team</b> to put this project on it.
    </span>
  </p>

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
          <div class="flex items-start gap-1 px-3 pt-2.5">
            <button
              type="button"
              class="focus-visible:outline-ring flex min-w-0 flex-1 items-start gap-2 py-0.5 text-left focus-visible:outline-2"
              @click="selectedPresetId = preset.id"
            >
              <Star
                v-if="projectTeam.presetId === preset.id"
                :size="15"
                class="text-primary fill-primary mt-px shrink-0"
                :aria-label="`${preset.name} is in use`"
              />
              <span v-else class="mt-px size-[15px] shrink-0" aria-hidden="true" />
              <span class="min-w-0 flex-1">
                <span class="block truncate text-xs font-medium">{{ preset.name }}</span>
                <span class="text-muted-foreground mt-0.5 block text-[10px]">
                  {{ preset.roles.length }} role{{ preset.roles.length === 1 ? '' : 's' }}
                  <template v-if="projectTeam.presetId === preset.id">
                    · in use{{ projectHasLocalChanges ? ', with local changes' : '' }}
                  </template>
                </span>
              </span>
            </button>

            <!-- Always present, never on hover: a control that appears only
                 when the pointer is over its row is a control that does not
                 exist on a phone, and this column is the one place a team can
                 be renamed or removed. Icons because three words of button
                 label per row, times a list of teams, is what pushed the
                 primary action off its own width. -->
            <div class="flex shrink-0 items-center">
              <!-- The primary action, and still an icon: a full-width button
                   under every row made the list read as a stack of buttons with
                   names above them rather than as a list of teams. The star in
                   the row already says which one is in use, so this only has to
                   be the way to change that. -->
              <Button
                v-if="useLabel(preset)"
                size="icon-xs"
                variant="ghost"
                class="text-primary hover:text-primary"
                :title="useLabel(preset)"
                :aria-label="useLabel(preset)"
                @click="adopt(preset)"
              >
                <CircleCheck :size="14" />
              </Button>
              <span v-else class="size-6 shrink-0" aria-hidden="true" />
              <Button
                size="icon-xs"
                variant="ghost"
                :title="`Clone ${preset.name}`"
                :aria-label="`Clone ${preset.name}`"
                @click="openClone(preset)"
              >
                <Copy :size="14" />
              </Button>
              <Button
                size="icon-xs"
                variant="ghost"
                :disabled="preset.builtin"
                :title="preset.builtin ? 'The built-in team cannot be renamed' : `Rename ${preset.name}`"
                :aria-label="`Rename ${preset.name}`"
                @click="openRename(preset)"
              >
                <Pencil :size="14" />
              </Button>
              <Button
                size="icon-xs"
                variant="ghost"
                class="hover:text-destructive"
                :disabled="preset.builtin || projectTeam.presetId === preset.id"
                :title="
                  preset.builtin
                    ? 'The built-in team cannot be deleted'
                    : projectTeam.presetId === preset.id
                      ? 'This project runs this team — put it on another one first'
                      : `Delete ${preset.name}`
                "
                :aria-label="`Delete ${preset.name}`"
                @click="confirmDelete = preset"
              >
                <Trash2 :size="14" />
              </Button>
            </div>
          </div>

          <div class="pb-2.5" />
        </li>
        <li v-if="!presets.length" class="text-muted-foreground px-3 py-6 text-center text-xs">
          No teams yet.
        </li>
      </ul>

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
          <!-- A switch, like the pipeline's. Both answer "is this role part of
               the run", and two controls for one kind of question read as two
               different kinds of question. -->
          <Switch
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
            size="icon-xs"
            variant="ghost"
            class="shrink-0"
            :disabled="!selectedRoleIds.has(template.id)"
            :title="
              selectedRoleIds.has(template.id)
                ? `Settings for ${template.name} in ${activePreset.name}`
                : `Add ${template.name} to this team to configure it`
            "
            :aria-label="`Settings for ${template.name}`"
            @click="editRoleSettings(template)"
          >
            <SlidersHorizontal :size="14" />
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
        <p class="text-muted-foreground mt-0.5 text-[10px]">
          <template v-if="running">
            Agents are running — changes apply immediately, to every project on this team
          </template>
          <template v-else>Work flows from top to bottom</template>
        </p>
      </div>

      <ol v-if="activePreset" class="divide-y">
        <li
          v-for="(role, index) in activePreset.roles"
          :key="role.templateId"
          :class="[
            'flex items-center gap-3 px-4 py-3',
            role.enabled ? '' : 'opacity-55',
          ]"
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
              <!-- A parked role keeps its place and its settings; it just does
                   not run, and work routes past it. -->
              <Badge v-if="!role.enabled" variant="outline">off</Badge>
            </div>
            <p class="text-muted-foreground mt-0.5 truncate text-[10px]">
              {{ effectiveRole(role)?.harness }} · {{ effectiveRole(role)?.model || 'default model' }}
            </p>
          </div>
          <Switch
            :model-value="role.enabled"
            :aria-label="`${libraryById.get(role.templateId)?.name ?? role.templateId} runs`"
            :title="role.enabled ? 'Running in this pipeline' : 'Parked — keeps its place, does not run'"
            @update:model-value="(v: boolean) => setEnabled(role.templateId, v)"
          />
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

  <!-- Asked only while agents are up, and only for the changes that stop
       one. Reordering does not kill anything, so it is not asked about. -->
  <Dialog :open="!!pending" @update:open="(v) => !v && (pending = null)">
    <DialogContent v-if="pending" class="sm:max-w-md">
      <DialogHeader>
        <DialogTitle>{{ pending.question }}</DialogTitle>
        <DialogDescription>
          {{
            pending.detail ??
            'Agents are running. This stops it within a second or so, and anything it was working on goes back to the queue for whoever picks it up next. Every project using this team is affected, not only this one.'
          }}
        </DialogDescription>
      </DialogHeader>
      <DialogFooter>
        <Button variant="outline" @click="pending = null">Cancel</Button>
        <Button
          variant="destructive"
          @click="
            () => {
              pending?.run()
              pending = null
            }
          "
        >
          {{ pending.confirm ?? 'Stop it' }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>

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
      <p
        v-if="actionError"
        class="bg-destructive/10 text-destructive px-3 py-2 text-xs"
        role="alert"
      >
        {{ actionError }}
      </p>
      <DialogFooter>
        <Button variant="outline" @click="confirmDelete = null">Cancel</Button>
        <Button variant="destructive" @click="deleteTeam">Delete team</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

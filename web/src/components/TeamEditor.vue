<script setup lang="ts">
import { computed, ref, useId } from 'vue'
import type { Model, ProjectRole, ResolvedRole, RoleTemplate } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import ModelPicker from '@/components/ModelPicker.vue'
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

// Every field's label points at the control it names. Without the pairing a
// screen reader reads an unlabelled control, and clicking the word does not
// focus the field it sits above.
const nameId = useId()
const harnessId = useId()
const receiveId = useId()
const gateId = useId()
const batchId = useId()
const promptId = useId()

const props = defineProps<{
  library: RoleTemplate[]
  team: ResolvedRole[]
  harnesses: string[]
  models: Record<string, Model[]>
  running: boolean
}>()
const emit = defineEmits<{
  setTeam: [roles: ProjectRole[]]
  saveRole: [role: RoleTemplate]
}>()

const editing = ref<RoleTemplate | null>(null)
const open = ref(false)
const custom = ref(false)

const selectedIds = computed(() => new Set(props.team.map((r) => r.id)))

/** Sending the whole team, never a diff: a reorder and a selection change are
 *  the same operation, so they cannot half-apply. */
function submit(roles: ResolvedRole[]) {
  emit(
    'setTeam',
    // Both overrides round-trip exactly as they came. Deriving them from the
    // resolved values instead meant a reorder sent the resolved model as an
    // override — pinning a model nobody had pinned — and dropped argsOverride
    // entirely, erasing arguments that were never part of the edit.
    roles.map((r) => ({
      templateId: r.id,
      enabled: r.enabled,
      modelOverride: r.modelOverride ?? null,
      argsOverride: r.argsOverride ?? null,
    })),
  )
}

function toggle(tpl: RoleTemplate) {
  if (selectedIds.value.has(tpl.id)) {
    submit(props.team.filter((r) => r.id !== tpl.id))
  } else {
    submit([
      ...props.team,
      { ...tpl, position: props.team.length, enabled: true, overridden: false, terminal: false },
    ])
  }
}

function move(index: number, delta: number) {
  const next = [...props.team]
  const target = index + delta
  if (target < 0 || target >= next.length) return
  ;[next[index], next[target]] = [next[target], next[index]]
  submit(next)
}

function setEnabled(role: ResolvedRole, enabled: boolean) {
  submit(props.team.map((r) => (r.id === role.id ? { ...r, enabled } : r)))
}

function edit(tpl: RoleTemplate) {
  editing.value = { ...tpl, args: [...tpl.args] }
  custom.value =
    !!tpl.model && !(props.models[tpl.harness] ?? []).some((m) => m.ID === tpl.model)
  open.value = true
}

function save() {
  if (editing.value) {
    emit('saveRole', editing.value)
    open.value = false
  }
}
</script>

<template>
  <div class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.3fr)]">
    <!-- The library: the idea of each role, shared by every project. -->
    <div class="border">
      <div class="border-b px-3 py-2 text-xs font-semibold">
        Library
        <span class="text-muted-foreground ml-2 font-normal">edit once, use anywhere</span>
      </div>
      <ul class="divide-y">
        <li
          v-for="tpl in library"
          :key="tpl.id"
          class="hover:bg-muted flex items-center gap-2.5 px-3 py-2"
        >
          <Checkbox
            :model-value="selectedIds.has(tpl.id)"
            :disabled="running"
            @update:model-value="toggle(tpl)"
          />
          <span class="text-xs font-medium">{{ tpl.name }}</span>
          <span class="text-muted-foreground text-xs">
            {{ tpl.harness }} · {{ tpl.model || 'default' }}
          </span>
          <Badge v-if="tpl.gate === 'approval'" variant="secondary">gate</Badge>
          <Button size="xs" variant="ghost" class="ml-auto" @click="edit(tpl)">Edit</Button>
        </li>
      </ul>
    </div>

    <!-- This project's pipeline. -->
    <div class="border">
      <div class="border-b px-3 py-2 text-xs font-semibold">
        Pipeline
        <span class="text-muted-foreground ml-2 font-normal">work flows top to bottom</span>
      </div>
      <ul class="divide-y">
        <li v-for="(role, i) in team" :key="role.id" class="flex items-center gap-2 px-3 py-2">
          <span class="text-muted-foreground w-4 text-xs">{{ i + 1 }}</span>
          <Checkbox
            :model-value="role.enabled"
            :disabled="running"
            @update:model-value="setEnabled(role, !role.enabled)"
          />
          <span :class="['text-xs font-medium', !role.enabled && 'line-through opacity-50']">
            {{ role.name }}
          </span>
          <span class="text-muted-foreground text-xs">{{ role.model }}</span>
          <Badge v-if="role.overridden" variant="secondary">override</Badge>
          <!-- Terminal is computed as the last enabled role, so disabling the
               last one promotes the one before it with no edit anywhere. -->
          <Badge v-if="role.terminal">terminal</Badge>
          <span class="ml-auto flex gap-1">
            <Button size="icon-xs" variant="ghost" :disabled="running || i === 0" @click="move(i, -1)">
              ↑
            </Button>
            <Button
              size="icon-xs"
              variant="ghost"
              :disabled="running || i === team.length - 1"
              @click="move(i, 1)"
            >
              ↓
            </Button>
          </span>
        </li>
        <li v-if="!team.length" class="text-muted-foreground px-3 py-3 text-xs">
          No roles selected. Tick one in the library.
        </li>
      </ul>
      <p v-if="running" class="text-muted-foreground border-t px-3 py-2 text-xs">
        Stop the swarm to change the pipeline.
      </p>
    </div>
  </div>

  <!-- Role editor. Editing a template changes that role everywhere, which is
       the point of a library and worth saying out loud. -->
  <Dialog v-model:open="open">
    <DialogContent v-if="editing" class="gap-0 overflow-hidden p-0 sm:max-w-2xl">
      <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
        <DialogTitle class="flex items-center gap-2">
          {{ editing.name }}
          <Badge v-if="editing.builtin" variant="outline">built-in</Badge>
        </DialogTitle>
        <DialogDescription>
          Editing a library role changes it in every project that uses it.
        </DialogDescription>
      </DialogHeader>

      <DialogBody class="grid gap-3 sm:grid-cols-2">
        <div class="flex flex-col gap-1.5">
          <Label :for="nameId">Name</Label>
          <Input :id="nameId" v-model="editing.name" />
        </div>

        <div class="flex flex-col gap-1.5">
          <Label :for="harnessId">Harness</Label>
          <Select v-model="editing.harness">
            <SelectTrigger :id="harnessId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="h in harnesses" :key="h" :value="h">{{ h }}</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <div class="flex flex-col gap-1.5">
          <ModelPicker v-model="editing.model" :models="models[editing.harness] ?? []" label="Model" />
          <span class="text-muted-foreground text-xs">
            {{ (models[editing.harness] ?? []).length }} listed by {{ editing.harness }}. A working
            model can be absent from a catalog, so anything you type is accepted.
          </span>
        </div>

        <div class="flex flex-col gap-1.5">
          <Label :for="receiveId">Receive</Label>
          <Select v-model="editing.receive">
            <SelectTrigger :id="receiveId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="task">task — one at a time</SelectItem>
              <SelectItem value="batch">batch — several at once</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <div class="flex flex-col gap-1.5">
          <Label :for="gateId">Gate</Label>
          <Select v-model="editing.gate">
            <SelectTrigger :id="gateId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="none">none</SelectItem>
              <SelectItem value="approval">approval</SelectItem>
            </SelectContent>
          </Select>
          <span class="text-muted-foreground text-xs">
            An approval gate holds this role's handoffs for a human.
          </span>
        </div>

        <div v-if="editing.receive === 'batch'" class="flex flex-col gap-1.5">
          <Label :for="batchId">Batch max items</Label>
          <Input :id="batchId" v-model.number="editing.batchMaxItems" type="number" min="1" />
        </div>

        <div class="flex flex-col gap-1.5 sm:col-span-2">
          <Label :for="promptId">Prompt</Label>
          <Textarea :id="promptId" v-model="editing.prompt" rows="12" class="leading-relaxed" />
          <span class="text-muted-foreground text-xs">
            Composed with the shared instructions at every spawn.
          </span>
        </div>
      </DialogBody>

      <DialogFooter class="hairline-t shrink-0 px-5 py-4">
        <Button variant="outline" @click="open = false">Cancel</Button>
        <Button @click="save">Save</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

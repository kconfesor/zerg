<script setup lang="ts">
import { computed, ref } from 'vue'
import type { Model, ProjectRole, ResolvedRole, RoleTemplate } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import {
  Dialog,
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

/** What the Select shows: a catalogued id, or the Custom sentinel when the
 *  role runs something the harness never listed. */
const modelChoice = computed(() => {
  const m = editing.value?.model ?? ''
  if (custom.value || !m) return custom.value ? '__custom__' : ''
  const known = (props.models[editing.value!.harness] ?? []).some((x) => x.ID === m)
  return known ? m : '__custom__'
})

function pickModel(value: unknown) {
  const v = String(value ?? '')
  if (v === '__custom__') {
    custom.value = true
    return
  }
  custom.value = false
  if (editing.value) editing.value.model = v
}

const selectedIds = computed(() => new Set(props.team.map((r) => r.id)))

/** Sending the whole team, never a diff: a reorder and a selection change are
 *  the same operation, so they cannot half-apply. */
function submit(roles: ResolvedRole[]) {
  emit(
    'setTeam',
    roles.map((r) => ({
      templateId: r.id,
      enabled: r.enabled,
      modelOverride: r.overridden ? r.model : null,
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
    <DialogContent v-if="editing" class="sm:max-w-2xl">
      <DialogHeader>
        <DialogTitle class="flex items-center gap-2">
          {{ editing.name }}
          <Badge v-if="editing.builtin" variant="outline">built-in</Badge>
        </DialogTitle>
        <DialogDescription>
          Editing a library role changes it in every project that uses it.
        </DialogDescription>
      </DialogHeader>

      <div class="grid max-h-[60vh] gap-3 overflow-y-auto sm:grid-cols-2">
        <div class="flex flex-col gap-1.5">
          <Label>Name</Label>
          <Input v-model="editing.name" />
        </div>

        <div class="flex flex-col gap-1.5">
          <Label>Harness</Label>
          <Select v-model="editing.harness">
            <SelectTrigger><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="h in harnesses" :key="h" :value="h">{{ h }}</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <div class="flex flex-col gap-1.5">
          <Label>Model</Label>
          <Select :model-value="modelChoice" @update:model-value="pickModel">
            <SelectTrigger class="w-full"><SelectValue placeholder="harness default" /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="m in models[editing.harness] ?? []" :key="m.ID" :value="m.ID">
                {{ m.ID }}
              </SelectItem>
              <SelectItem value="__custom__">Custom…</SelectItem>
            </SelectContent>
          </Select>
          <Input
            v-if="modelChoice === '__custom__'"
            v-model="editing.model"
            placeholder="openai-codex/gpt-5.6-sol"
          />
          <span class="text-muted-foreground text-xs">
            A working model can be absent from a catalog, so custom ids are allowed.
          </span>
        </div>

        <div class="flex flex-col gap-1.5">
          <Label>Receive</Label>
          <Select v-model="editing.receive">
            <SelectTrigger><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="task">task — one at a time</SelectItem>
              <SelectItem value="batch">batch — several at once</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <div class="flex flex-col gap-1.5">
          <Label>Gate</Label>
          <Select v-model="editing.gate">
            <SelectTrigger><SelectValue /></SelectTrigger>
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
          <Label>Batch max items</Label>
          <Input v-model.number="editing.batchMaxItems" type="number" min="1" />
        </div>

        <div class="flex flex-col gap-1.5 sm:col-span-2">
          <Label>Prompt</Label>
          <Textarea v-model="editing.prompt" rows="12" class="leading-relaxed" />
          <span class="text-muted-foreground text-xs">
            Composed with the shared instructions at every spawn.
          </span>
        </div>
      </div>

      <DialogFooter>
        <Button variant="outline" @click="open = false">Cancel</Button>
        <Button @click="save">Save</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

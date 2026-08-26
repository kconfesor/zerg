<script setup lang="ts">
import { reactive, useId, watch } from 'vue'
import { joinArgs, splitArgs } from '@/lib/args'
import { roleOverrides } from '@/lib/role-overrides'
import type { Model, RoleOverrides, RoleTemplate } from '@/lib/api'
import { Button } from '@/components/ui/button'
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

const props = defineProps<{
  open: boolean
  role: RoleTemplate | null
  inherited: RoleTemplate | null
  scope: string
  harnesses: string[]
  models: Record<string, Model[]>
}>()
const emit = defineEmits<{
  'update:open': [open: boolean]
  save: [overrides: RoleOverrides]
}>()

const harnessId = useId()
const argsId = useId()
const receiveId = useId()
const gateId = useId()
const batchItemsId = useId()
const batchAgeId = useId()
const promptId = useId()

const form = reactive({
  harness: '',
  model: '',
  args: '',
  receive: 'task' as 'task' | 'batch',
  batchMaxItems: 8,
  batchMaxAgeSec: 300,
  prompt: '',
  gate: 'none' as 'none' | 'approval',
})

function load(role: RoleTemplate | null) {
  if (!role) return
  form.harness = role.harness
  form.model = role.model
  form.args = joinArgs(role.args ?? [])
  form.receive = role.receive
  form.batchMaxItems = role.batchMaxItems
  form.batchMaxAgeSec = role.batchMaxAgeSec
  form.prompt = role.prompt
  form.gate = role.gate
}

watch(
  () => [props.open, props.role] as const,
  ([open, role]) => open && load(role),
  { immediate: true },
)

function save() {
  const base = props.inherited
  const role = props.role
  if (!base || !role) return
  emit('save', roleOverrides({
    ...role,
    harness: form.harness,
    model: form.model,
    args: splitArgs(form.args),
    receive: form.receive,
    batchMaxItems: form.batchMaxItems,
    batchMaxAgeSec: form.batchMaxAgeSec,
    prompt: form.prompt,
    gate: form.gate,
  }, base))
  emit('update:open', false)
}

function reset() {
  load(props.inherited)
}
</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent v-if="role && inherited" class="gap-0 overflow-hidden p-0 sm:max-w-2xl">
      <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
        <DialogTitle>{{ role.name }}</DialogTitle>
        <DialogDescription>
          {{ scope }}. Fields left at their inherited value keep following their source.
        </DialogDescription>
      </DialogHeader>

      <DialogBody class="grid gap-3 sm:grid-cols-2">
        <div class="flex flex-col gap-1.5">
          <Label :for="harnessId">Harness</Label>
          <Select v-model="form.harness">
            <SelectTrigger :id="harnessId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="h in harnesses" :key="h" :value="h">{{ h }}</SelectItem>
            </SelectContent>
          </Select>
          <span class="text-muted-foreground text-[11px]">Inherited: {{ inherited.harness }}</span>
        </div>

        <div class="flex flex-col gap-1.5">
          <ModelPicker v-model="form.model" :models="models[form.harness] ?? []" label="Model" />
          <span class="text-muted-foreground text-[11px]">
            Inherited: {{ inherited.model || 'harness default' }}
          </span>
        </div>

        <div class="flex flex-col gap-1.5 sm:col-span-2">
          <Label :for="argsId">Arguments</Label>
          <Input :id="argsId" v-model="form.args" placeholder="--flag value" />
          <span class="text-muted-foreground text-[11px]">
            Shell-style quoting is only for grouping. Empty means no arguments.
          </span>
        </div>

        <div class="flex flex-col gap-1.5">
          <Label :for="receiveId">Receive</Label>
          <Select v-model="form.receive">
            <SelectTrigger :id="receiveId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="task">task — one at a time</SelectItem>
              <SelectItem value="batch">batch — several at once</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <div class="flex flex-col gap-1.5">
          <Label :for="gateId">Gate</Label>
          <Select v-model="form.gate">
            <SelectTrigger :id="gateId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="none">none</SelectItem>
              <SelectItem value="approval">approval</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <template v-if="form.receive === 'batch'">
          <div class="flex flex-col gap-1.5">
            <Label :for="batchItemsId">Batch max items</Label>
            <Input :id="batchItemsId" v-model.number="form.batchMaxItems" type="number" min="1" />
          </div>
          <div class="flex flex-col gap-1.5">
            <Label :for="batchAgeId">Batch max age (seconds)</Label>
            <Input :id="batchAgeId" v-model.number="form.batchMaxAgeSec" type="number" min="1" />
          </div>
        </template>

        <div class="flex flex-col gap-1.5 sm:col-span-2">
          <Label :for="promptId">Prompt</Label>
          <Textarea :id="promptId" v-model="form.prompt" rows="12" class="leading-relaxed" />
          <span class="text-muted-foreground text-[11px]">
            Composed with the shared instructions at every spawn.
          </span>
        </div>
      </DialogBody>

      <DialogFooter class="hairline-t shrink-0 px-5 py-4">
        <Button variant="ghost" class="mr-auto" @click="reset">Use all inherited values</Button>
        <Button variant="outline" @click="emit('update:open', false)">Cancel</Button>
        <Button :disabled="!form.harness.trim()" @click="save">Save overrides</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

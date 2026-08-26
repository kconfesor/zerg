<script setup lang="ts">
import { computed, reactive, useId, watch } from 'vue'
import { joinArgs, splitArgs } from '@/lib/args'
import { roleOverrides } from '@/lib/role-overrides'
import type { Model, RoleOverrides, RoleTemplate } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
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

type Field =
  | 'harness'
  | 'model'
  | 'args'
  | 'receive'
  | 'batchMaxItems'
  | 'batchMaxAgeSec'
  | 'prompt'
  | 'gate'

function differs(field: Field): boolean {
  const base = props.inherited
  if (!base) return false
  if (field === 'args') {
    const args = splitArgs(form.args)
    return args.length !== base.args.length || args.some((v, i) => v !== base.args[i])
  }
  return form[field] !== base[field]
}

function resetField(field: Field) {
  const base = props.inherited
  if (!base) return
  if (field === 'args') form.args = joinArgs(base.args)
  else form[field] = base[field] as never
}

const overrideCount = computed(
  () =>
    (['harness', 'model', 'args', 'receive', 'batchMaxItems', 'batchMaxAgeSec', 'prompt', 'gate'] as Field[])
      .filter(differs).length,
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

      <DialogBody class="grid gap-4 sm:grid-cols-2">
        <div class="bg-muted/40 flex items-center gap-2 border px-3 py-2.5 sm:col-span-2">
          <Badge :variant="overrideCount ? 'secondary' : 'outline'">
            {{ overrideCount ? `${overrideCount} override${overrideCount === 1 ? '' : 's'}` : 'all defaults' }}
          </Badge>
          <span class="text-muted-foreground text-[11px]">
            Only changed fields stop following their inherited value.
          </span>
          <Button v-if="overrideCount" size="xs" variant="ghost" class="ml-auto" @click="reset">
            Reset all
          </Button>
        </div>

        <div class="flex flex-col gap-1.5">
          <div class="flex items-center justify-between gap-2">
            <Label :for="harnessId">Harness</Label>
            <Button v-if="differs('harness')" size="xs" variant="ghost" @click="resetField('harness')">
              Use default
            </Button>
          </div>
          <Select v-model="form.harness">
            <SelectTrigger :id="harnessId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="h in harnesses" :key="h" :value="h">{{ h }}</SelectItem>
            </SelectContent>
          </Select>
          <span class="text-muted-foreground flex items-center gap-1.5 text-[11px]">
            <Badge :variant="differs('harness') ? 'secondary' : 'outline'">
              {{ differs('harness') ? 'overridden' : 'team default' }}
            </Badge>
            Default: {{ inherited.harness }}
          </span>
        </div>

        <div class="flex flex-col gap-1.5">
          <ModelPicker v-model="form.model" :models="models[form.harness] ?? []" label="Model" />
          <span class="text-muted-foreground flex items-center gap-1.5 text-[11px]">
            <Badge :variant="differs('model') ? 'secondary' : 'outline'">
              {{ differs('model') ? 'overridden' : 'team default' }}
            </Badge>
            Default: {{ inherited.model || 'harness default' }}
            <Button v-if="differs('model')" size="xs" variant="ghost" class="ml-auto" @click="resetField('model')">
              Use default
            </Button>
          </span>
        </div>

        <div class="flex flex-col gap-1.5 sm:col-span-2">
          <div class="flex items-center justify-between gap-2">
            <Label :for="argsId">Arguments</Label>
            <Button v-if="differs('args')" size="xs" variant="ghost" @click="resetField('args')">
              Use default
            </Button>
          </div>
          <Input :id="argsId" v-model="form.args" placeholder="--flag value" />
          <span class="text-muted-foreground flex items-center gap-1.5 text-[11px]">
            <Badge :variant="differs('args') ? 'secondary' : 'outline'">
              {{ differs('args') ? 'overridden' : 'team default' }}
            </Badge>
            Empty explicitly removes all arguments; reset restores inheritance.
          </span>
        </div>

        <div class="flex flex-col gap-1.5">
          <div class="flex items-center justify-between gap-2">
            <Label :for="receiveId">Receive</Label>
            <Button v-if="differs('receive')" size="xs" variant="ghost" @click="resetField('receive')">
              Use default
            </Button>
          </div>
          <Select v-model="form.receive">
            <SelectTrigger :id="receiveId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="task">task — one at a time</SelectItem>
              <SelectItem value="batch">batch — several at once</SelectItem>
            </SelectContent>
          </Select>
          <span class="text-muted-foreground text-[11px]">
            {{ differs('receive') ? 'Overridden' : 'Using team default' }}
          </span>
        </div>

        <div class="flex flex-col gap-1.5">
          <div class="flex items-center justify-between gap-2">
            <Label :for="gateId">Gate</Label>
            <Button v-if="differs('gate')" size="xs" variant="ghost" @click="resetField('gate')">
              Use default
            </Button>
          </div>
          <Select v-model="form.gate">
            <SelectTrigger :id="gateId"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="none">none</SelectItem>
              <SelectItem value="approval">approval</SelectItem>
            </SelectContent>
          </Select>
          <span class="text-muted-foreground text-[11px]">
            {{ differs('gate') ? 'Overridden' : 'Using team default' }}
          </span>
        </div>

        <template v-if="form.receive === 'batch'">
          <div class="flex flex-col gap-1.5">
            <div class="flex items-center justify-between gap-2">
              <Label :for="batchItemsId">Batch max items</Label>
              <Button v-if="differs('batchMaxItems')" size="xs" variant="ghost" @click="resetField('batchMaxItems')">
                Use default
              </Button>
            </div>
            <Input :id="batchItemsId" v-model.number="form.batchMaxItems" type="number" min="1" />
          </div>
          <div class="flex flex-col gap-1.5">
            <div class="flex items-center justify-between gap-2">
              <Label :for="batchAgeId">Batch max age (seconds)</Label>
              <Button v-if="differs('batchMaxAgeSec')" size="xs" variant="ghost" @click="resetField('batchMaxAgeSec')">
                Use default
              </Button>
            </div>
            <Input :id="batchAgeId" v-model.number="form.batchMaxAgeSec" type="number" min="1" />
          </div>
        </template>

        <div class="flex flex-col gap-1.5 sm:col-span-2">
          <div class="flex items-center justify-between gap-2">
            <Label :for="promptId">Prompt</Label>
            <Button v-if="differs('prompt')" size="xs" variant="ghost" @click="resetField('prompt')">
              Use default
            </Button>
          </div>
          <Textarea :id="promptId" v-model="form.prompt" rows="12" class="leading-relaxed" />
          <span class="text-muted-foreground flex items-center gap-1.5 text-[11px]">
            <Badge :variant="differs('prompt') ? 'secondary' : 'outline'">
              {{ differs('prompt') ? 'overridden' : 'team default' }}
            </Badge>
            Composed with the shared instructions at every spawn.
          </span>
        </div>
      </DialogBody>

      <DialogFooter class="hairline-t shrink-0 px-5 py-4">
        <span class="text-muted-foreground mr-auto text-[11px]">
          {{ overrideCount ? `${overrideCount} fields will be saved locally` : 'This role will follow every default' }}
        </span>
        <Button variant="outline" @click="emit('update:open', false)">Cancel</Button>
        <Button :disabled="!form.harness.trim()" @click="save">Save</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>

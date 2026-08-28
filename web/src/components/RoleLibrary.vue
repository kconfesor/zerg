<script setup lang="ts">
/**
 * The role library: the idea of each role, before any team arranges them.
 *
 * It lives in Settings rather than beside the team editor because it is not a
 * team. A team is an ordering of roles for a project; a role is what that entry
 * means — which harness runs it, on which model, under what prompt. Editing one
 * changes it for every team and every project that uses it, which is the point
 * of a library and the reason it does not belong on a screen about one project.
 *
 * Self-contained, like the rest of Settings: it fetches what it needs rather
 * than having it threaded down through the shell.
 */
import { computed, onMounted, ref, useId, watch } from 'vue'
import { Flag, Play, Plus, Trash2 } from '@lucide/vue'
import { api, type Model, type RoleTemplate, type TeamPreset } from '@/lib/api'
import { latest } from '@/lib/latest'
import { usePending } from '@/lib/pending'
import ModelPicker from '@/components/ModelPicker.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
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

const library = ref<RoleTemplate[]>([])
const harnesses = ref<string[]>([])
/**
 * The value that means "leave the harness alone".
 *
 * Not the empty string: reka's SelectItem refuses one, because Select uses ""
 * to mean no selection at all, and an item carrying it throws on mount rather
 * than rendering. A level is one word, so this cannot collide with a real one.
 */
const HARNESS_DEFAULT = 'harness default'

/** Told to whoever else is holding a copy of the library. */
const emit = defineEmits<{ changed: [] }>()

const models = ref<Record<string, Model[]>>({})
/** The reasoning levels each harness accepts. Read from the daemon, since only
 *  the adapter knows what its CLI will take. */
const thinking = ref<Record<string, string[]>>({})

/**
 * A level the new harness does not take is not a level.
 *
 * The field hides itself for a harness with no such control, and kept its old
 * value while hidden, so a role moved from pi to claude went on asking for
 * "off", which claude exits on rather than runs.
 */
watch(
  () => editing.value?.harness,
  (harness) => {
    if (!editing.value || !harness) return
    const levels = thinking.value[harness] ?? []
    if (editing.value.thinking && !levels.includes(editing.value.thinking)) {
      editing.value.thinking = ''
    }
  },
)

/** The select's value, which is the level or the sentinel above. */
const thinkingChoice = computed({
  get: () => editing.value?.thinking || HARNESS_DEFAULT,
  set: (v: string) => {
    if (editing.value) editing.value.thinking = v === HARNESS_DEFAULT ? '' : v
  },
})
const presets = ref<TeamPreset[]>([])
const note = ref<{ tone: 'ok' | 'bad'; text: string } | null>(null)
const busy = usePending()
const newest = latest()

const nameId = useId()
const harnessId = useId()
const thinkingId = useId()
const receiveId = useId()
const gateId = useId()
const finisherId = useId()
const batchId = useId()
const promptId = useId()

async function load() {
  const current = newest()
  try {
    const [roles, hs, ps] = await Promise.all([api.roles(), api.harnesses(), api.teamPresets()])
    if (!current()) return
    library.value = roles
    harnesses.value = hs
    presets.value = ps
    // The catalogs, so the model picker can narrow rather than constrain.
    const catalogs = await Promise.all(hs.map((h) => api.models(h).catch(() => [] as Model[])))
    const levels = await Promise.all(hs.map((h) => api.thinking(h).catch(() => [] as string[])))
    thinking.value = Object.fromEntries(hs.map((h, i) => [h, levels[i]]))
    if (!current()) return
    models.value = Object.fromEntries(hs.map((h, i) => [h, catalogs[i]]))
  } catch (e) {
    if (!current()) return
    note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
  }
}
onMounted(load)

/**
 * Which teams each role is in.
 *
 * Read-only, and the reason deleting a role is not a quiet act: the foreign key
 * cascades, so removing one takes it out of every team that contains it without
 * anything else being said.
 */
const teamsUsing = computed(() => {
  const out = new Map<string, string[]>()
  for (const p of presets.value) {
    for (const r of p.roles ?? []) {
      out.set(r.templateId, [...(out.get(r.templateId) ?? []), p.name])
    }
  }
  return out
})

const editing = ref<RoleTemplate | null>(null)
const creating = ref(false)
const open = ref(false)
const confirmDelete = ref<RoleTemplate | null>(null)

function edit(tpl: RoleTemplate) {
  creating.value = false
  editing.value = { ...tpl, args: [...(tpl.args ?? [])] }
  open.value = true
}

function create() {
  creating.value = true
  editing.value = {
    id: '',
    name: '',
    harness: harnesses.value[0] ?? 'claude',
    model: '',
    thinking: '',
    args: [],
    receive: 'task',
    batchMaxItems: 8,
    batchMaxAgeSec: 300,
    prompt: '',
    gate: 'none',
    finisher: false,
    builtin: false,
  } as RoleTemplate
  open.value = true
}

async function save() {
  const role = editing.value
  if (!role) return
  await busy.run('save', async () => {
    note.value = null
    try {
      if (creating.value) {
        await api.createRole(role)
      } else {
        await api.updateRole(role)
      }
      open.value = false
      await load()
      // The rest of the cockpit holds its own copy of the library, taken at
      // startup. Without this, adding a role to a pipeline placed it by its old
      // "ends a pipeline" setting, and a team editor open in another tab kept
      // the old prompt and model.
      emit('changed')
      // A running swarm already has its processes; the daemon reconciles the
      // ones whose harness changed, and the rest pick this up when they next
      // respawn. Saying which is better than implying a control that does not
      // exist — there is no per-role restart.
      note.value = {
        tone: 'ok',
        text: creating.value
          ? `Added ${role.name}. Put it on a team to use it.`
          : `Saved ${role.name}. Running roles pick this up when they next spawn.`,
      }
    } catch (e) {
      note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
    }
  })
}

async function remove(tpl: RoleTemplate) {
  await busy.run(`del:${tpl.id}`, async () => {
    note.value = null
    try {
      await api.deleteRole(tpl.id)
      confirmDelete.value = null
      await load()
      emit('changed')
      note.value = { tone: 'ok', text: `Removed ${tpl.name}.` }
    } catch (e) {
      note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
    }
  })
}
</script>

<template>
  <div class="flex flex-col gap-3">
    <p
      v-if="note"
      :class="[
        'px-3 py-2 text-xs',
        note.tone === 'bad'
          ? 'bg-destructive/10 text-destructive'
          : 'bg-[var(--status-good)]/10 text-[var(--status-good)]',
      ]"
    >
      {{ note.text }}
    </p>

    <p class="text-muted-foreground max-w-[70ch] text-[11px] leading-snug">
      A role is what an entry on a team <em>means</em>: its harness, model, prompt and gate. Teams
      arrange these; they do not define them. Editing one here changes it for every team and every
      project that uses it, which is what a library is for.
    </p>

    <div class="hairline flex flex-col">
      <div
        v-for="tpl in library"
        :key="tpl.id"
        class="hairline-b hover:bg-muted/40 flex items-center gap-3 px-3 py-2.5 last:border-b-0"
      >
        <button
          type="button"
          class="focus-visible:outline-ring min-w-0 flex-1 text-left focus-visible:outline-2"
          @click="edit(tpl)"
        >
          <span class="flex items-center gap-2">
            <span class="truncate text-xs font-medium">{{ tpl.name }}</span>
            <Badge v-if="tpl.builtin" variant="outline" class="text-[9px]">built-in</Badge>
            <Badge v-if="tpl.finisher" variant="secondary" class="gap-1 text-[9px]">
              <Flag :size="9" aria-hidden="true" />
              ends a pipeline
            </Badge>
            <Badge v-if="tpl.gate === 'approval'" variant="secondary" class="text-[9px]">
              approval gate
            </Badge>
            <!-- A role that is not in any pipeline. Said here because
                 everything else on this screen is something you put in a team,
                 and this one is not: the daemon starts it when somebody asks
                 to see the app running. -->
            <Badge v-if="tpl.purpose === 'runner'" variant="secondary" class="gap-1 text-[9px]">
              <Play :size="9" aria-hidden="true" />
              runs your app
            </Badge>
          </span>
          <span class="text-muted-foreground mt-0.5 block truncate font-mono text-[10px]">
            {{ tpl.harness }}<span v-if="tpl.model"> · {{ tpl.model }}</span>
            <span v-if="tpl.receive === 'batch'"> · batches {{ tpl.batchMaxItems }}</span>
          </span>
        </button>

        <span class="text-muted-foreground hidden shrink-0 text-[10px] sm:block">
          <template v-if="teamsUsing.get(tpl.id)?.length">
            on {{ teamsUsing.get(tpl.id)!.join(', ') }}
          </template>
          <template v-else>on no team</template>
        </span>

        <Button
          size="icon-sm"
          variant="ghost"
          class="text-muted-foreground hover:text-destructive shrink-0"
          :disabled="busy.is(`del:${tpl.id}`)"
          :title="`Remove ${tpl.name} from the library`"
          :aria-label="`Remove ${tpl.name} from the library`"
          @click="confirmDelete = tpl"
        >
          <Trash2 :size="13" aria-hidden="true" />
        </Button>
      </div>

      <p v-if="!library.length" class="text-muted-foreground px-3 py-6 text-center text-[11px]">
        No roles yet.
      </p>
    </div>

    <div>
      <Button variant="outline" size="sm" class="gap-1.5" @click="create">
        <Plus :size="14" aria-hidden="true" />
        Add a role
      </Button>
    </div>

    <!-- Editing a library role changes it everywhere, which is worth saying on
         the screen that does it rather than only in a doc. -->
    <Dialog v-model:open="open">
      <DialogContent v-if="editing" class="gap-0 overflow-hidden p-0 sm:max-w-2xl">
        <DialogHeader class="hairline-b shrink-0 px-5 py-4 pr-12">
          <DialogTitle class="flex items-center gap-2">
            {{ creating ? 'New role' : editing.name }}
            <Badge v-if="editing.builtin" variant="outline">built-in</Badge>
          </DialogTitle>
          <DialogDescription>
            <template v-if="creating">
              A role is available to every team once it exists. Nothing runs it until a team
              includes it.
            </template>
            <template v-else>
              Changes here reach every team and project that uses this role.
              <template v-if="teamsUsing.get(editing.id)?.length">
                Currently on {{ teamsUsing.get(editing.id)!.join(', ') }}.
              </template>
            </template>
          </DialogDescription>
        </DialogHeader>

        <DialogBody class="grid gap-3 sm:grid-cols-2">
          <div class="flex flex-col gap-1.5">
            <Label :for="nameId">Name</Label>
            <Input :id="nameId" v-model="editing.name" />
            <span class="text-muted-foreground text-[11px]">
              Becomes a worktree directory and the name every handoff uses: lower case, no spaces.
            </span>
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
            <ModelPicker
              v-model="editing.model"
              :models="models[editing.harness] ?? []"
              label="Model"
            />
            <span class="text-muted-foreground text-[11px]">
              {{ (models[editing.harness] ?? []).length }} listed by {{ editing.harness }}. A working
              model can be absent from a catalog, so anything you type is accepted.
            </span>
          </div>

          <!-- Offered only where the harness has one, and with that harness's
               own levels: claude takes low through max, pi starts at off. -->
          <div v-if="(thinking[editing.harness] ?? []).length" class="flex flex-col gap-1.5">
            <Label :for="thinkingId">Thinking</Label>
            <Select v-model="thinkingChoice">
              <SelectTrigger :id="thinkingId"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem :value="HARNESS_DEFAULT">{{ HARNESS_DEFAULT }}</SelectItem>
                <SelectItem v-for="level in thinking[editing.harness] ?? []" :key="level" :value="level">
                  {{ level }}
                </SelectItem>
              </SelectContent>
            </Select>
            <span class="text-muted-foreground text-[11px]">
              How hard {{ editing.harness }} reasons before answering. It costs tokens and time, so
              it is worth raising for the roles that review and lowering for the ones that do not.
            </span>
          </div>

          <div class="flex flex-col gap-1.5">
            <Label :for="receiveId">Receive</Label>
            <Select v-model="editing.receive">
              <SelectTrigger :id="receiveId"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="task">task, one at a time</SelectItem>
                <SelectItem value="batch">batch, several at once</SelectItem>
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
            <span class="text-muted-foreground text-[11px]">
              An approval gate holds this role's handoffs for a human.
            </span>
          </div>

          <p
            v-if="editing.purpose === 'runner'"
            class="bg-muted/30 border-l-primary border border-l-2 p-2 text-[11px] leading-relaxed"
          >
            This role is not part of any pipeline. It is started when somebody
            asks to see the app running: it reads the project, works out how it
            serves itself, and starts it. It never claims work, never appears on
            the board, and cannot be added to a team. Its harness, model,
            thinking and prompt are yours to change, like any other role's.
          </p>

          <!-- Where this role belongs in a pipeline, which is a fact about the
               role rather than about any one team: a reviewer or a cleaner ends
               the work wherever it appears, and a planner never does. -->
          <div v-if="editing.purpose !== 'runner'" class="flex flex-col gap-1.5">
            <Label :for="finisherId">Ends a pipeline</Label>
            <label class="flex items-start gap-2.5 text-xs">
              <Switch
                :id="finisherId"
                :model-value="editing.finisher"
                aria-label="This role ends a pipeline"
                class="mt-0.5"
                @update:model-value="(v: boolean) => editing && (editing.finisher = v)"
              />
              <span class="text-muted-foreground">
                Joins a team at the end and finishes the task there.
              </span>
            </label>
          </div>

          <div v-if="editing.receive === 'batch'" class="flex flex-col gap-1.5">
            <Label :for="batchId">Batch max items</Label>
            <Input :id="batchId" v-model.number="editing.batchMaxItems" type="number" min="1" />
          </div>

          <div class="flex flex-col gap-1.5 sm:col-span-2">
            <Label :for="promptId">Prompt</Label>
            <Textarea :id="promptId" v-model="editing.prompt" rows="12" class="leading-relaxed" />
            <span class="text-muted-foreground text-[11px]">
              Composed with the shared instructions at every spawn, so an edit is live the next time
              this role starts.
            </span>
          </div>
        </DialogBody>

        <DialogFooter class="hairline-t shrink-0 px-5 py-4">
          <Button variant="outline" @click="open = false">Cancel</Button>
          <Button :disabled="!editing.name.trim() || busy.is('save')" @click="save">
            {{ busy.is('save') ? 'Saving…' : 'Save' }}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <Dialog :open="!!confirmDelete" @update:open="(v) => !v && (confirmDelete = null)">
      <DialogContent v-if="confirmDelete" class="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Remove {{ confirmDelete.name }}?</DialogTitle>
          <DialogDescription>
            <template v-if="teamsUsing.get(confirmDelete.id)?.length">
              It is on {{ teamsUsing.get(confirmDelete.id)!.join(', ') }} and will be taken off
              {{ teamsUsing.get(confirmDelete.id)!.length === 1 ? 'that team' : 'those teams' }}.
              Work already done keeps its history.
            </template>
            <template v-else>
              It is on no team, so nothing else changes. Work already done keeps its history.
            </template>
            <template v-if="confirmDelete.builtin">
              This is a built-in role, so a restart puts it back.
            </template>
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" @click="confirmDelete = null">Cancel</Button>
          <Button
            variant="destructive"
            :disabled="busy.is(`del:${confirmDelete.id}`)"
            @click="remove(confirmDelete)"
          >
            Remove
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>

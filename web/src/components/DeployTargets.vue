<script setup lang="ts">
/**
 * Where a project's work can be run.
 *
 * A target is a name and a command, edited here because configuration in this
 * project is rows rather than a file somebody has to find. zerg knows nothing
 * about Docker or any host: it runs the command in a checkout of the commit
 * being previewed, with $PORT set, and proxies whatever answers.
 */
import { onMounted, ref } from 'vue'
import { Play, Trash2 } from '@lucide/vue'
import { api, type DeployTarget } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

const props = defineProps<{ projectId: string | null }>()

const targets = ref<DeployTarget[]>([])
const error = ref('')
const busy = ref(false)

const draft = ref({ name: '', command: '', stopCommand: '', cwd: '', readySecs: 120 })

async function load() {
  if (!props.projectId) return
  try {
    targets.value = await api.targets(props.projectId)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}
onMounted(load)

async function add() {
  if (!props.projectId || !draft.value.name.trim() || !draft.value.command.trim()) return
  busy.value = true
  error.value = ''
  try {
    await api.saveTarget(props.projectId, { ...draft.value, kind: 'local' })
    draft.value = { name: '', command: '', stopCommand: '', cwd: '', readySecs: 120 }
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

async function remove(t: DeployTarget) {
  if (!props.projectId) return
  try {
    await api.deleteTarget(props.projectId, t.id)
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}
</script>

<template>
  <div class="flex flex-col gap-4">
    <div>
      <h3 class="text-xs font-semibold">Run it here</h3>
      <p class="text-muted-foreground mt-1 text-[11px] leading-relaxed">
        A command that serves this project on the port zerg gives it. At an approval you can run
        the exact commit being decided about, in a checkout of its own, and click the result before
        deciding: nothing is merged and your own checkout is not touched. The command must bind
        <code>$PORT</code> and stay in the foreground.
      </p>
    </div>

    <div v-for="t in targets" :key="t.id" class="flex items-start gap-2 border p-2">
      <Play :size="12" aria-hidden="true" class="text-primary mt-0.5 shrink-0" />
      <div class="min-w-0 flex-1">
        <p class="text-[11px] font-medium">{{ t.name }}</p>
        <p class="text-muted-foreground font-mono text-[10px] break-all">{{ t.command }}</p>
        <p v-if="t.stopCommand" class="text-muted-foreground font-mono text-[10px] break-all">
          stop: {{ t.stopCommand }}
        </p>
        <p class="text-muted-foreground text-[10px]">
          waits {{ t.readySecs }}s for the port<template v-if="t.cwd"> · in {{ t.cwd }}</template>
        </p>
      </div>
      <button
        type="button"
        class="text-muted-foreground hover:text-destructive focus-visible:outline-ring shrink-0 focus-visible:outline-2"
        :aria-label="`Remove ${t.name}`"
        @click="remove(t)"
      >
        <Trash2 :size="12" aria-hidden="true" />
      </button>
    </div>

    <div class="flex flex-col gap-2 border border-dashed p-2">
      <div class="flex flex-col gap-1">
        <Label for="target-name" class="text-[11px]">Name</Label>
        <Input id="target-name" v-model="draft.name" placeholder="compose" class="h-8" />
      </div>
      <div class="flex flex-col gap-1">
        <Label for="target-command" class="text-[11px]">Command</Label>
        <Input
          id="target-command"
          v-model="draft.command"
          placeholder="docker compose up --force-recreate"
          class="h-8 font-mono"
        />
      </div>
      <div class="flex flex-col gap-1">
        <Label for="target-stop" class="text-[11px]">
          Stop command
          <span class="text-muted-foreground font-normal">
            · optional, for what an interrupt does not clean up
          </span>
        </Label>
        <Input
          id="target-stop"
          v-model="draft.stopCommand"
          placeholder="docker compose down"
          class="h-8 font-mono"
        />
      </div>
      <div class="flex gap-2">
        <div class="flex flex-1 flex-col gap-1">
          <Label for="target-cwd" class="text-[11px]">
            Folder <span class="text-muted-foreground font-normal">· optional</span>
          </Label>
          <Input id="target-cwd" v-model="draft.cwd" placeholder="web" class="h-8 font-mono" />
        </div>
        <div class="flex w-32 flex-col gap-1">
          <Label for="target-ready" class="text-[11px]">Wait (s)</Label>
          <Input id="target-ready" v-model.number="draft.readySecs" type="number" class="h-8" />
        </div>
      </div>
      <Button
        size="sm"
        variant="outline"
        class="self-start"
        :disabled="busy || !draft.name.trim() || !draft.command.trim()"
        @click="add"
      >
        Add target
      </Button>
    </div>

    <p v-if="error" class="text-destructive text-[11px]">{{ error }}</p>
  </div>
</template>

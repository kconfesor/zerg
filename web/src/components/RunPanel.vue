<script setup lang="ts">
/**
 * Running the project so somebody can look at it.
 *
 * The daemon runs nothing here. It starts an agent with the commit checked out
 * and three verbs, and that agent reads the repository to work out how this
 * project serves itself. What is on screen is therefore a conversation with
 * it: what it is doing, what it learned, and the two things a person needs to
 * be able to do when it is wrong -- correct it, or start again.
 */
import { computed, onUnmounted, ref, watch } from 'vue'
import { ExternalLink, MessageCircleQuestion, Play, RotateCcw, Square } from '@lucide/vue'
import { api, type RunState } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

const props = defineProps<{
  projectId: string | undefined
  /**
   * The commit to run.
   *
   * At a gate it is the one being decided about, and at a finished card the
   * one that landed. Absent means the base branch as it stands, which is what
   * "run this project" means when nobody is looking at a particular change --
   * and the daemon resolves it, because only the repository knows the head.
   */
  commit?: string
  taskId?: string
}>()

const emit = defineEmits<{
  /** A service appeared, so whatever lists artifacts should look again. */
  served: []
}>()

const state = ref<RunState | null>(null)
const busy = ref(false)
const error = ref('')
const guidance = ref('')
const editingNote = ref(false)
const noteDraft = ref('')

let poll: number | undefined

async function refresh() {
  if (!props.projectId) return
  try {
    const next = await api.runState(props.projectId)
    const was = state.value?.state
    state.value = next
    // Tell the parent once, when it arrives: the artifact list is what shows
    // the link, and it does not otherwise know to look again.
    if (next.state === 'serving' && was !== 'serving') emit('served')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

/**
 * Polled while it is working, not always.
 *
 * A build is minutes and an agent reading a repository is not fast, so the
 * panel has to keep asking; a project sitting idle does not, and a poll per
 * second per open tab for nothing is how a local daemon starts feeling busy.
 */
function watchWhileWorking() {
  window.clearInterval(poll)
  if (state.value && ['working', 'asking'].includes(state.value.state)) {
    poll = window.setInterval(refresh, 2000)
  }
}

watch(() => props.projectId, refresh, { immediate: true })
watch(() => state.value?.state, watchWhileWorking)
onUnmounted(() => window.clearInterval(poll))

async function act(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
    await refresh()
    watchWhileWorking()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

const run = () =>
  act(async () => {
    if (!props.projectId) return
    await api.startRun(props.projectId, { commit: props.commit ?? '', taskId: props.taskId })
  })

const guide = () =>
  act(async () => {
    if (!props.projectId || !guidance.value.trim()) return
    await api.guideRun(props.projectId, guidance.value)
    guidance.value = ''
  })

const stop = () =>
  act(async () => {
    if (props.projectId) await api.stopRun(props.projectId)
  })

const saveNote = () =>
  act(async () => {
    if (props.projectId) await api.saveRunNote(props.projectId, noteDraft.value)
    editingNote.value = false
  })


/** Whether this commit is the one running, so "run" and "run again" are
 *  different offers. */
const running = computed(() => state.value && state.value.state !== 'idle')
const sameCommit = computed(() => state.value?.commit === props.commit)

/** What the button offers, which depends on what is being run and what is
 *  already up. */
const label = computed(() => {
  if (running.value && !sameCommit.value) return 'Run this one instead'
  return props.commit ? 'Run this change' : 'Run this project'
})

const says = computed(() => {
  switch (state.value?.state) {
    case 'working':
      return 'reading the project and starting it'
    case 'asking':
      return 'it asked you something; the question is in Attention'
    case 'serving':
      return sameCommit.value ? 'running' : 'running, but a different commit'
    case 'gave up':
      return state.value?.message || 'it could not start this'
    default:
      return ''
  }
})
</script>

<template>
  <div v-if="projectId" class="flex flex-col gap-2">
    <div class="flex flex-wrap items-center gap-2">
      <Button
        v-if="!running || !sameCommit"
        size="xs"
        variant="outline"
        class="h-7 gap-1.5 px-2 text-[11px]"
        :disabled="busy"
        @click="run"
      >
        <Play :size="12" aria-hidden="true" />
        {{ label }}
      </Button>

      <template v-if="running">
        <Button
          size="xs"
          variant="ghost"
          class="h-7 gap-1.5 px-2 text-[11px]"
          :disabled="busy"
          title="Start again from nothing, for when the session has gone in circles"
          @click="run"
        >
          <RotateCcw :size="12" aria-hidden="true" />
          start over
        </Button>
        <Button
          size="xs"
          variant="ghost"
          class="h-7 gap-1.5 px-2 text-[11px]"
          :disabled="busy"
          @click="stop"
        >
          <Square :size="12" aria-hidden="true" />
          stop
        </Button>
      </template>

      <!-- The link, where the state is. Saying "running" and stopping there
           answers the wrong question: the reason to run one is to open it. -->
      <a
        v-for="s in state?.services ?? []"
        :key="s.id"
        :href="s.url"
        target="_blank"
        rel="noopener noreferrer"
        class="text-primary focus-visible:outline-ring flex items-center gap-1 text-[11px] font-medium underline-offset-2 hover:underline focus-visible:outline-2"
      >
        {{ s.label || 'open it' }}
        <ExternalLink :size="11" aria-hidden="true" />
      </a>

      <span
        v-if="says"
        class="text-[11px]"
        :class="state?.state === 'gave up' ? 'text-[var(--status-warning)]' : 'text-muted-foreground'"
      >
        <MessageCircleQuestion
          v-if="state?.state === 'asking'"
          :size="12"
          aria-hidden="true"
          class="mr-1 inline-block align-[-2px]"
        />
        {{ says }}
      </span>
    </div>

    <!-- Correcting it, which is why its session stays alive: "no, the admin
         portal" reaches an agent that still remembers what it just tried. -->
    <div v-if="running" class="flex gap-2">
      <Input
        v-model="guidance"
        class="h-8 flex-1 text-xs"
        placeholder="tell it something: which app, which compose file, what it got wrong"
        @keyup.enter="guide"
      />
      <Button
        size="sm"
        variant="outline"
        class="h-8 shrink-0"
        :disabled="busy || !guidance.trim()"
        @click="guide"
      >
        Tell it
      </Button>
    </div>

    <!-- What it has worked out, which is why the second run is faster than the
         first. Editable, because the agent can be wrong and correcting it is
         cheaper than watching it fail again. -->
    <div v-if="state?.note || editingNote" class="bg-muted/20 border p-2">
      <p class="text-muted-foreground mb-1 flex items-center gap-1.5 text-[10px]">
        <span>
          how this project runs · {{ state?.noteAuthor === 'operator' ? 'you told it' : 'it worked this out' }}
        </span>
        <button
          v-if="!editingNote"
          type="button"
          class="hover:text-foreground focus-visible:outline-ring ml-auto underline-offset-2 hover:underline focus-visible:outline-2"
          @click="((noteDraft = state?.note ?? ''), (editingNote = true))"
        >
          correct it
        </button>
      </p>
      <p v-if="!editingNote" class="text-[11px] leading-relaxed whitespace-pre-wrap">
        {{ state?.note }}
      </p>
      <template v-else>
        <textarea
          v-model="noteDraft"
          rows="4"
          class="border-input bg-background w-full border p-2 font-mono text-[11px]"
        />
        <div class="mt-1 flex gap-2">
          <Button size="xs" class="h-6" :disabled="busy" @click="saveNote">Save</Button>
          <Button size="xs" variant="ghost" class="h-6" @click="editingNote = false">Cancel</Button>
        </div>
      </template>
    </div>

    <p v-if="error" class="text-destructive text-[11px]">{{ error }}</p>
  </div>
</template>

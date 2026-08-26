<script setup lang="ts">
/**
 * A conversation with an agent that has the project open and no part in the
 * pipeline.
 *
 * Built on the same event stream as everything else rather than a history of
 * its own: messages persist, survive a reload, and resume after a dropped
 * connection because they are events, and events already do all three.
 */
import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'
import {
  api,
  streamActivity,
  type ActivityEvent,
  type ActivityStream,
  type Model,
  type Project,
} from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import ModelPicker from '@/components/ModelPicker.vue'
import { Trash2 } from '@lucide/vue'
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
import { renderMarkdown } from '@/lib/markdown'
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupTextarea,
} from '@/components/ui/input-group'

// Every field's label points at the control it names. Without the pairing a
// screen reader reads an unlabelled control, and clicking the word does not
// focus the field it sits above.
const harnessId = useId()

const props = defineProps<{
  project: Project | null
  harnesses: string[]
  models: Record<string, Model[]>
}>()
const emit = defineEmits<{ updated: [project: Project] }>()

/** The project id, which is all the stream and the ask endpoint need. */
const projectId = computed(() => props.project?.id ?? null)

/** Only these two roles are part of the conversation. Everything else on the
 *  stream is the pipeline working, which belongs in Activity. */
const CHAT = 'chat'
const OPERATOR = 'operator'

interface Line {
  id: string
  who: 'you' | 'agent'
  text: string
  tool?: string
}

/**
 * A conversation is not a transcript, and an unbounded one costs a re-render
 * of every line for each new one. Old turns scroll out of reach long before
 * this; the durable record is the event stream, which keeps everything.
 */
const MAX_LINES = 500

const lines = ref<Line[]>([])
const draft = ref('')
const sending = ref(false)
const thinking = ref(false)
const error = ref('')
const viewport = ref<HTMLElement | null>(null)
let stream: ActivityStream | null = null

function accept(e: ActivityEvent) {
  if (e.role !== CHAT && e.role !== OPERATOR) return

  if (e.role === OPERATOR) {
    lines.value.push({ id: e.id, who: 'you', text: e.text ?? '' })
  } else if (e.kind === 'message' && e.text) {
    thinking.value = false
    lines.value.push({ id: e.id, who: 'agent', text: e.text })
  } else if (e.kind === 'tool_call') {
    // Shown, because "it is reading files" is the difference between working
    // and hung, and an answer about the repository should be traceable to what
    // was actually read.
    lines.value.push({ id: e.id, who: 'agent', text: '', tool: e.tool })
  } else if (e.kind === 'turn_end') {
    thinking.value = false
  } else if (e.kind === 'error') {
    thinking.value = false
    error.value = e.text ?? 'the agent failed'
  }
  queueFlush()
}

/**
 * Events arrive faster than a screen can usefully change.
 *
 * A replay delivers a burst, and pushing each line onto a reactive array
 * re-rendered the transcript and scheduled a scroll every time — work that
 * grows with the number of events for a result nobody can see. Trimming and
 * scrolling happen once per frame instead, which is as often as anything
 * actually appears.
 */
let frame = 0

function queueFlush() {
  if (frame) return
  frame = requestAnimationFrame(() => {
    frame = 0
    if (lines.value.length > MAX_LINES) {
      lines.value.splice(0, lines.value.length - MAX_LINES)
    }
    scrollToEnd()
  })
}

function connect() {
  stream?.close()
  if (frame) {
    cancelAnimationFrame(frame)
    frame = 0
  }
  lines.value = []
  if (!projectId.value) return
  stream = streamActivity(projectId.value, { onEvent: accept, onCaughtUp: scrollToEnd })
}

/** The effective harness: what is set, or the team's. */
const harness = ref(props.project?.chatHarness ?? '')
const model = ref(props.project?.chatModel ?? '')
/**
 * The value that means "inherit the team's harness", which is stored as an
 * empty string. reka reserves "" to mean "nothing selected, show the
 * placeholder" and refuses it as an item value — this option rendered a console
 * error and no row at all.
 */
const INHERIT = 'inherit:team'

const confirmReset = ref(false)

watch(
  () => props.project,
  (p) => {
    harness.value = p?.chatHarness ?? ''
    model.value = p?.chatModel ?? ''
  },
)

// Changing the agent ends the running session on the daemon, so two changes in
// flight at once would end two sessions and leave the later answer describing
// the earlier choice.
const changingAgent = ref(false)
const resetting = ref(false)

async function setAgent(h: string, m: string) {
  if (!projectId.value || changingAgent.value) return
  if (h === (props.project?.chatHarness ?? '') && m === (props.project?.chatModel ?? '')) return
  harness.value = h
  changingAgent.value = true
  try {
    // The running session was built from the old choice, so the daemon ends it;
    // the next question starts one that matches.
    emit('updated', await api.setChatAgent(projectId.value, h, m))
    lines.value = []
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    changingAgent.value = false
  }
}

async function resetChat() {
  if (!projectId.value || resetting.value) return
  resetting.value = true
  try {
    await api.resetChat(projectId.value)
    confirmReset.value = false
    lines.value = []
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    resetting.value = false
  }
}

function scrollToEnd() {
  nextTick(() => {
    const el = viewport.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

async function send() {
  const text = draft.value.trim()
  if (!text || !projectId.value) return
  sending.value = true
  error.value = ''
  try {
    await api.chat(projectId.value, text)
    draft.value = ''
    // The agent's first output can be a minute away on a large repository, so
    // say it is working rather than leaving the panel looking inert.
    thinking.value = true
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    sending.value = false
  }
}

/** Enter sends, shift+enter is a newline — the convention every chat uses. */
function onKeydown(ev: KeyboardEvent) {
  if (ev.key === 'Enter' && !ev.shiftKey) {
    ev.preventDefault()
    send()
  }
}

watch(projectId, connect, { immediate: true })
onBeforeUnmount(() => stream?.close())
</script>

<template>
  <div class="flex flex-col gap-3">
    <!-- Which agent answers, and the way to start over. Both belong here: the
         choice is about this conversation, and the reset is destructive enough
         that it should not be somewhere you press by accident. -->
    <div class="flex flex-wrap items-end gap-2">
      <div class="flex flex-col gap-1">
        <Label :for="harnessId" class="text-[10px]">Harness</Label>
        <Select
          :model-value="harness || INHERIT"
          @update:model-value="(v) => setAgent(v === INHERIT ? '' : String(v), model)"
        >
          <SelectTrigger :id="harnessId" size="sm" class="w-36"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem :value="INHERIT">inherit from the team</SelectItem>
            <SelectItem v-for="h in harnesses" :key="h" :value="h">{{ h }}</SelectItem>
          </SelectContent>
        </Select>
      </div>

      <ModelPicker
        v-model="model"
        :models="models[harness] ?? []"
        label="Model"
        input-class="h-8 w-56 text-xs"
        @commit="(m) => setAgent(harness, m)"
      />

      <Button
        variant="outline"
        size="sm"
        class="ml-auto gap-1.5"
        :disabled="!projectId"
        @click="confirmReset = true"
      >
        <Trash2 :size="13" aria-hidden="true" />
        End this chat
      </Button>
    </div>

    <div
      ref="viewport"
      class="bg-card h-[58vh] overflow-y-auto border p-3 md:h-[60vh]"
    >
      <p v-if="!lines.length" class="text-muted-foreground text-xs leading-relaxed">
        Ask about the repository — how something works, where a thing lives, whether an idea is
        already implemented. This agent reads the project and answers; it does not take work. When
        the answer is "that needs a change", queue it as a task on the Board.
      </p>

      <div class="flex flex-col gap-3">
        <!-- A line never changes once it is in the transcript, so it never
             needs patching again. -->
        <div v-for="l in lines" :key="l.id" v-memo="[]" class="text-xs">
          <template v-if="l.tool">
            <p class="text-muted-foreground font-mono text-[11px]">
              <span class="opacity-60">read</span> {{ l.tool }}
            </p>
          </template>
          <template v-else>
            <p
              class="mb-0.5 text-[11px] font-semibold"
              :class="l.who === 'you' ? 'text-muted-foreground' : 'text-[var(--chart-1)]'"
            >
              {{ l.who === 'you' ? 'you' : 'agent' }}
            </p>
            <!-- The agent writes Markdown, so it is rendered as Markdown. The
                 renderer escapes first and builds tags only from characters it
                 put there, so nothing an agent read out of the repository can
                 become HTML. Your own messages stay literal — you typed them,
                 and showing them back reformatted is confusing. -->
            <p v-if="l.who === 'you'" class="leading-relaxed whitespace-pre-wrap">{{ l.text }}</p>
            <div v-else class="md leading-relaxed" v-html="renderMarkdown(l.text)" />
          </template>
        </div>

        <p v-if="thinking" class="text-muted-foreground text-[11px] italic">thinking…</p>
      </div>
    </div>

    <p v-if="error" class="bg-destructive/10 text-destructive px-3 py-2 text-xs">{{ error }}</p>

    <!-- One control rather than a field with a button parked beside it. The
         two belong together — the button does nothing except to this text —
         and a separate box made them read as two decisions. -->
    <InputGroup>
      <InputGroupTextarea
        v-model="draft"
        rows="2"
        class="text-xs"
        placeholder="What does the evaluator do with unary minus?"
        :disabled="!projectId"
        @keydown="onKeydown"
      />
      <InputGroupAddon align="block-end">
        <span class="text-muted-foreground text-[10px]">enter to send</span>
        <InputGroupButton
          variant="default"
          size="sm"
          class="ml-auto"
          :disabled="sending || !draft.trim() || !projectId"
          @click="send"
        >
          {{ sending ? '…' : 'Ask' }}
        </InputGroupButton>
      </InputGroupAddon>
    </InputGroup>
    <Dialog v-model:open="confirmReset">
      <DialogContent class="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>End this chat?</DialogTitle>
          <DialogDescription>
            The conversation is deleted and the chat worktree is removed, along with anything the
            agent left in it. Your own checkout and every role's worktree are untouched, and no
            task history is affected. The next question starts a fresh one.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" @click="confirmReset = false">Cancel</Button>
          <Button variant="destructive" :disabled="resetting" @click="resetChat">End it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <p class="text-muted-foreground text-[11px]">
      Runs in its own worktree with no access to the work queue, so a question cannot disturb
      anything in flight.
    </p>
  </div>
</template>

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
  artifactBytes,
  streamActivity,
  type ActivityEvent,
  type ActivityStream,
  type Chat,
  type Model,
  type Project,
} from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import ModelPicker from '@/components/ModelPicker.vue'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Bot, ChevronDown, Paperclip, Plus, Square, X } from '@lucide/vue'
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
import AnswerBody from '@/components/AnswerBody.vue'
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

/** The project id, which scopes every conversation in it. */
const projectId = computed(() => props.project?.id ?? null)

/**
 * The conversations this project holds, and the one being read.
 *
 * Tabs rather than one thread: a second subject used to be either an
 * interruption of the first or a reason to delete it, since ending the chat was
 * the only way to start fresh. Each tab has its own transcript, its own files
 * and its own agent, and closing one takes exactly those.
 */
const chats = ref<Chat[]>([])
const openChat = ref<string | null>(null)
const renaming = ref<string | null>(null)
const renameDraft = ref('')
const confirmEnd = ref<Chat | null>(null)

/** What a tab says when nobody has named it and nothing has been said yet. */
const tabLabel = (c: Chat) => c.title || 'New chat'

async function loadChats(keepOpen = true) {
  if (!projectId.value) {
    chats.value = []
    openChat.value = null
    return
  }
  try {
    chats.value = await api.chats(projectId.value)
    // An empty project starts with one, because a chat screen with no chat in
    // it is a screen asking you to press something before you can type.
    if (!chats.value.length) {
      const made = await api.newChat(projectId.value)
      chats.value = [made]
    }
    if (!keepOpen || !chats.value.some((c) => c.id === openChat.value)) {
      openChat.value = chats.value[0]?.id ?? null
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function startChat() {
  if (!projectId.value) return
  try {
    const made = await api.newChat(projectId.value)
    chats.value = [made, ...chats.value]
    openChat.value = made.id
    draft.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function endChat(c: Chat) {
  if (!projectId.value) return
  try {
    await api.endChat(projectId.value, c.id)
    confirmEnd.value = null
    chats.value = chats.value.filter((x) => x.id !== c.id)
    if (openChat.value === c.id) openChat.value = chats.value[0]?.id ?? null
    // Closing the last one leaves nowhere to type, so a fresh one opens.
    if (!chats.value.length) await loadChats(false)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function commitRename(c: Chat) {
  const title = renameDraft.value.trim()
  renaming.value = null
  if (!projectId.value || !title || title === c.title) return
  try {
    await api.renameChat(projectId.value, c.id, title)
    c.title = title
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

/** Only these two roles are part of the conversation. Everything else on the
 *  stream is the pipeline working, which belongs in Activity. */
const CHAT = 'chat'
const OPERATOR = 'operator'

interface Line {
  id: string
  who: 'you' | 'agent'
  text: string
  tool?: string
  /** Files sent with this message, for the ones the person wrote. */
  files?: { name: string; artifactId: string }[]
  /** Still being written. A streamed answer is assembled from fragments and
   *  then replaced by the authoritative message when the block closes. */
  live?: boolean
  /** Sent, but behind an answer still being written. */
  queued?: boolean
  /** A turn that failed, as it happened. Kept in place rather than raised as a
   *  banner, because the conversation carried on after it. */
  failed?: string
}

/** A file chosen but not yet sent with a message. */
interface Pending {
  /** Local id, so a row can be removed before the upload finishes. */
  key: string
  name: string
  size: number
  /** The artifact once the upload lands; absent while it is in flight. */
  artifactId?: string
  error?: string
  preview?: string
}

/**
 * A conversation is not a transcript, and an unbounded one costs a re-render
 * of every line for each new one. Old turns scroll out of reach long before
 * this; the durable record is the event stream, which keeps everything.
 */
const MAX_LINES = 500

/** Whether to show the file rather than name it. By extension, because the
 *  transcript records what the file was called and not what it was. */
function isImage(name: string): boolean {
  return /\.(png|jpe?g|gif|webp|avif|svg)$/i.test(name)
}

const lines = ref<Line[]>([])
const draft = ref('')
const sending = ref(false)
const thinking = ref(false)
const error = ref('')

/** Files chosen for the next message, uploading or uploaded. */
const attachments = ref<Pending[]>([])
const stopping = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)
const dragging = ref(false)

/** Ready to send: nothing still uploading, and something to say. */
const canSend = computed(
  () =>
    !!projectId.value &&
    !!openChat.value &&
    (draft.value.trim() !== '' || attachments.value.some((a) => a.artifactId)) &&
    !attachments.value.some((a) => !a.artifactId && !a.error),
)
const viewport = ref<HTMLElement | null>(null)
let stream: ActivityStream | null = null

function accept(e: ActivityEvent) {
  if (e.role !== CHAT && e.role !== OPERATOR) return

  if (e.role === OPERATOR) {
    const files = (e.data?.attachments as Line['files']) ?? undefined
    // Typed while the agent was still writing. The daemon queues it and
    // answers it next; saying so is the difference between "it is coming" and
    // "did that send?".
    const queued = thinking.value
    lines.value.push({ id: e.id, who: 'you', text: e.text ?? '', files, queued })
  } else if (e.kind === 'message_delta' && e.text) {
    // A fragment of the answer being written. Assembled into one line rather
    // than pushed as many: the reader is watching a sentence appear, not a
    // list of syllables.
    thinking.value = false
    const last = lines.value[lines.value.length - 1]
    if (last?.who === 'agent' && last.live) {
      last.text += e.text
    } else {
      lines.value.push({ id: e.id, who: 'agent', text: e.text, live: true })
    }
  } else if (e.kind === 'message' && e.text) {
    thinking.value = false
    // The authoritative text for something already on screen in pieces. It
    // replaces the assembled version rather than following it, or every
    // streamed answer would appear twice.
    const last = lines.value[lines.value.length - 1]
    if (last?.who === 'agent' && last.live) {
      last.text = e.text
      last.id = e.id
      last.live = false
    } else {
      lines.value.push({ id: e.id, who: 'agent', text: e.text })
    }
  } else if (e.kind === 'tool_call') {
    // Shown, because "it is reading files" is the difference between working
    // and hung, and an answer about the repository should be traceable to what
    // was actually read.
    lines.value.push({ id: e.id, who: 'agent', text: '', tool: e.tool })
  } else if (e.kind === 'turn_end') {
    thinking.value = false
    // Whatever was still marked live is finished, streamed or not.
    for (const l of lines.value) l.live = false
    // The first queued message is now the one being answered.
    const next = lines.value.find((l) => l.queued)
    if (next) {
      next.queued = false
      thinking.value = true
    }
  } else if (e.kind === 'error') {
    thinking.value = false
    // A failure that already happened is part of the conversation, not news.
    //
    // The banner said "the harness reported an error without describing it"
    // on opening a tab whose transcript contained one, however old, and again
    // on every tab after it: an error from another conversation an hour ago,
    // presented as though it had just happened to the thing being read. Live
    // failures still interrupt; replayed ones are a line where they happened.
    if (replaying) {
      lines.value.push({ id: e.id, who: 'agent', text: '', failed: e.text || 'the agent failed' })
    } else {
      error.value = e.text ?? 'the agent failed'
    }
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

/**
 * Whether the events arriving are history rather than news.
 *
 * A replay delivers everything the conversation ever said, including whatever
 * went wrong in it. Treating those the same as live events made an hour-old
 * failure look like the state of the thing you had just opened.
 */
let replaying = true

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
  thinking.value = false
  if (!projectId.value || !openChat.value) return
  // One conversation, asked for by id. Without it the socket carries every
  // tab's answers into whichever one happens to be open.
  // Each tab starts clean: a banner from the last conversation is not about
  // this one, and it used to follow the reader from tab to tab.
  error.value = ''
  replaying = true
  stream = streamActivity(
    projectId.value,
    {
      onEvent: accept,
      onCaughtUp: () => {
        replaying = false
        scrollToEnd()
      },
    },
    { chat: openChat.value },
  )
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

/** What the collapsed control says: the choice, not the labels around it. */
const agentSummary = computed(() => {
  const h = harness.value || props.project?.chatHarness || ''
  const m = model.value || props.project?.chatModel || ''
  if (!h && !m) return "the team's agent"
  return [h || "the team's harness", m].filter(Boolean).join(' · ')
})


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

async function setAgent(h: string, m: string) {
  if (!projectId.value || changingAgent.value) return
  if (h === (props.project?.chatHarness ?? '') && m === (props.project?.chatModel ?? '')) return
  harness.value = h
  changingAgent.value = true
  try {
    // The running session was built from the old choice, so the daemon ends it;
    // the next question starts one that matches.
    emit('updated', await api.setChatAgent(projectId.value, h, m))
    // The sessions are gone, not the conversations: what was said stays, and
    // the next question is answered by the agent just chosen.
    connect()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    changingAgent.value = false
  }
}


/**
 * Takes files from the picker, a drop, or a paste.
 *
 * Uploaded as they are chosen rather than when the message is sent: a slow
 * upload does not hold what was typed, and one that fails does not take the
 * message with it. The row appears immediately so the person can see it
 * arriving and remove it if they picked the wrong thing.
 */
async function attach(files: FileList | File[] | null) {
  if (!files || !projectId.value || !openChat.value) return
  for (const file of Array.from(files)) {
    const key = `${file.name}-${file.size}-${Math.random().toString(36).slice(2)}`
    const row: Pending = { key, name: file.name, size: file.size }
    // A picture is shown before it has finished uploading: it is how you tell
    // at a glance that you attached the right screenshot.
    if (file.type.startsWith('image/')) row.preview = URL.createObjectURL(file)
    attachments.value.push(row)
    try {
      const made = await api.chatAttach(projectId.value, openChat.value!, file)
      row.artifactId = made.id
      row.name = made.name || row.name
    } catch (e) {
      row.error = e instanceof Error ? e.message : String(e)
    }
  }
}

function removeAttachment(key: string) {
  const row = attachments.value.find((a) => a.key === key)
  if (row?.preview) URL.revokeObjectURL(row.preview)
  attachments.value = attachments.value.filter((a) => a.key !== key)
}

/** A screenshot pasted straight in, which is how most of them arrive. */
function onPaste(ev: ClipboardEvent) {
  const files = Array.from(ev.clipboardData?.files ?? [])
  if (files.length) {
    ev.preventDefault()
    void attach(files)
  }
}

function onDrop(ev: DragEvent) {
  dragging.value = false
  const files = ev.dataTransfer?.files
  if (files?.length) void attach(files)
}

/**
 * Stops the answer being written.
 *
 * Not the conversation: the session stays up, so the next question still has
 * everything said before it. Ending the chat is a different button with a
 * confirmation on it.
 */
async function stop() {
  if (!projectId.value || !openChat.value || stopping.value) return
  stopping.value = true
  try {
    await api.interruptChat(projectId.value, openChat.value!)
    thinking.value = false
    // Anything queued behind it was about the answer just stopped.
    lines.value = lines.value.filter((l) => !l.queued)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    stopping.value = false
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
  const files = attachments.value.filter((a) => a.artifactId)
  if (!canSend.value) return
  sending.value = true
  error.value = ''
  try {
    await api.chat(
      projectId.value!,
      openChat.value!,
      text,
      files.map((f) => f.artifactId!),
    )
    // The tab may have just been named by this message, and its order changes
    // with use.
    void loadChats()
    draft.value = ''
    for (const f of attachments.value) if (f.preview) URL.revokeObjectURL(f.preview)
    attachments.value = []
    // The agent's first output can be a minute away on a large repository, so
    // say it is working rather than leaving the panel looking inert. A message
    // sent while it is already writing is queued by the daemon and answered
    // next, so this stays true either way.
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

watch(projectId, () => loadChats(false), { immediate: true })
watch(openChat, connect)
onBeforeUnmount(() => stream?.close())
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col gap-3">
    <!-- Which agent answers, and the way to open another conversation.
         The two selects are behind the button that names the current choice:
         they were two labelled controls sitting above every conversation,
         which on a phone wrapped to two rows and took 114 of the 648 pixels
         the screen had -- a permanent cost for a thing that is set once and
         then read, if ever. -->
    <div class="flex flex-wrap items-center gap-2">
      <Popover>
        <PopoverTrigger as-child>
          <Button variant="outline" size="sm" class="gap-1.5" :disabled="!projectId">
            <Bot :size="13" aria-hidden="true" />
            <span class="max-w-[14rem] truncate">{{ agentSummary }}</span>
            <ChevronDown :size="12" aria-hidden="true" class="opacity-60" />
          </Button>
        </PopoverTrigger>
        <PopoverContent align="start" class="w-72">
          <div class="flex flex-col gap-3">
            <div class="flex flex-col gap-1">
              <Label :for="harnessId" class="text-[10px]">Harness</Label>
              <Select
                :model-value="harness || INHERIT"
                @update:model-value="(v) => setAgent(v === INHERIT ? '' : String(v), model)"
              >
                <SelectTrigger :id="harnessId" size="sm" class="w-full"><SelectValue /></SelectTrigger>
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
              input-class="h-8 w-full text-xs"
              @commit="(m) => setAgent(harness, m)"
            />

            <!-- Said here rather than under every conversation, where it was
                 a permanent 54 pixels of explanation you read once. -->
            <p class="text-muted-foreground text-[11px] leading-relaxed">
              Runs in its own worktree with no access to the work queue, so a question cannot
              disturb anything in flight.
            </p>
          </div>
        </PopoverContent>
      </Popover>

      <Button
        variant="outline"
        size="sm"
        class="ml-auto gap-1.5"
        :disabled="!projectId"
        @click="startChat"
      >
        <Plus :size="13" aria-hidden="true" />
        New chat
      </Button>
    </div>

    <!-- The conversations, as tabs. A second subject used to be either an
         interruption of the first or a reason to delete it, since ending the
         chat was the only way to start fresh. Each of these has its own
         transcript, its own files and its own agent, and closing one takes
         exactly those and nothing else.

         Scrolls rather than wraps: a row of tabs that reflows moves the one
         you were about to click. -->
    <div v-if="chats.length" class="hairline-b flex gap-1 overflow-x-auto pb-1.5">
      <div
        v-for="c in chats"
        :key="c.id"
        class="group flex shrink-0 items-center gap-1 border px-2 py-1 text-[11px]"
        :class="
          c.id === openChat
            ? 'border-primary text-foreground'
            : 'text-muted-foreground hover:text-foreground border-transparent'
        "
      >
        <!-- Double click renames, which is where every tab strip puts it. -->
        <input
          v-if="renaming === c.id"
          v-model="renameDraft"
          class="border-input bg-background w-32 border px-1 text-[11px]"
          :aria-label="`Rename ${tabLabel(c)}`"
          @keyup.enter="commitRename(c)"
          @keyup.escape="renaming = null"
          @blur="commitRename(c)"
        />
        <button
          v-else
          type="button"
          class="focus-visible:outline-ring max-w-[12rem] truncate focus-visible:outline-2"
          :title="tabLabel(c)"
          @click="openChat = c.id"
          @dblclick="((renaming = c.id), (renameDraft = c.title))"
        >
          {{ tabLabel(c) }}
        </button>
        <button
          type="button"
          class="hover:text-destructive focus-visible:outline-ring shrink-0 opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100 focus-visible:outline-2"
          :aria-label="`Close ${tabLabel(c)}`"
          title="Close this chat, and delete it"
          @click.stop="confirmEnd = c"
        >
          <X :size="11" aria-hidden="true" />
        </button>
      </div>
    </div>

    <div
      ref="viewport"
      class="bg-card min-h-0 flex-1 overflow-y-auto border p-3"
    >
      <p v-if="!lines.length" class="text-muted-foreground text-xs leading-relaxed">
        Ask about the repository: how something works, where a thing lives, whether an idea is
        already implemented. This agent reads the project and answers; it does not take work. When
        the answer is "that needs a change", queue it as a task on the Board.
      </p>

      <div class="flex flex-col gap-3">
        <!-- A finished line never changes again, so it is memoised on the
             two things that can: the text while it is still being written,
             and whether it is still queued behind an answer. -->
        <div v-for="l in lines" :key="l.id" v-memo="[l.text, l.live, l.queued]" class="text-xs">
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
            <p v-if="l.who === 'you' && l.text" class="leading-relaxed whitespace-pre-wrap">
              {{ l.text }}
            </p>
            <AnswerBody v-else-if="l.who === 'agent' && l.text" :text="l.text" />

            <!-- Something that failed, where it failed. The conversation
                 carried on afterwards, so it reads as part of the account
                 rather than as the state of the screen. -->
            <p v-if="l.failed" class="text-[var(--status-warning)] text-[11px] italic">
              {{ l.failed }}
            </p>

            <!-- What was attached. The picture is shown, because a screenshot
                 you sent is the subject of the answer under it and a file name
                 is not; anything else is a link, since the agent read it from
                 the worktree and you may want to read it too. -->
            <div v-if="l.files?.length" class="mt-1.5 flex flex-wrap gap-2">
              <a
                v-for="f in l.files"
                :key="f.artifactId"
                :href="artifactBytes(f.artifactId)"
                target="_blank"
                rel="noopener noreferrer"
                class="focus-visible:outline-ring block border focus-visible:outline-2"
                :title="f.name"
              >
                <img
                  v-if="isImage(f.name)"
                  :src="artifactBytes(f.artifactId)"
                  :alt="f.name"
                  class="max-h-40 max-w-full object-contain"
                  loading="lazy"
                />
                <span v-else class="text-muted-foreground block px-2 py-1 text-[11px]">
                  {{ f.name }}
                </span>
              </a>
            </div>

            <p v-if="l.queued" class="text-muted-foreground mt-0.5 text-[10px] italic">
              waiting for the answer above
            </p>
          </template>
        </div>

        <p v-if="thinking" class="text-muted-foreground text-[11px] italic">thinking…</p>
      </div>
    </div>

    <p v-if="error" class="bg-destructive/10 text-destructive px-3 py-2 text-xs">{{ error }}</p>

    <!-- One control rather than a field with a button parked beside it. The
         two belong together — the button does nothing except to this text —
         and a separate box made them read as two decisions.

         The whole thing is a drop target: a screenshot is dragged in as often
         as it is picked, and a zone that only accepts a drop on the exact
         button is one people miss. -->
    <div
      class="relative"
      @dragover.prevent="dragging = true"
      @dragleave="dragging = false"
      @drop.prevent="onDrop"
    >
      <div
        v-if="dragging"
        class="border-primary bg-primary/5 text-primary pointer-events-none absolute inset-0 z-10 flex items-center justify-center border-2 border-dashed text-xs"
      >
        drop to attach
      </div>

      <!-- What is going with the next message. Shown before it is sent, so a
           wrong file can be taken off rather than discovered in the answer. -->
      <div v-if="attachments.length" class="mb-1.5 flex flex-wrap gap-2">
        <div
          v-for="a in attachments"
          :key="a.key"
          class="flex items-center gap-1.5 border px-1.5 py-1 text-[11px]"
          :class="a.error ? 'border-destructive text-destructive' : 'text-muted-foreground'"
        >
          <img
            v-if="a.preview"
            :src="a.preview"
            :alt="a.name"
            class="size-6 object-cover"
          />
          <span class="max-w-[12rem] truncate" :title="a.error || a.name">{{ a.name }}</span>
          <span v-if="!a.artifactId && !a.error" class="opacity-60">sending…</span>
          <span v-else-if="a.error" class="opacity-80">failed</span>
          <button
            type="button"
            class="hover:text-foreground focus-visible:outline-ring focus-visible:outline-2"
            :aria-label="`Remove ${a.name}`"
            @click="removeAttachment(a.key)"
          >
            <X :size="11" aria-hidden="true" />
          </button>
        </div>
      </div>

      <InputGroup>
        <InputGroupTextarea
          v-model="draft"
          rows="2"
          class="text-xs"
          placeholder="What does the evaluator do with unary minus?"
          :disabled="!projectId"
          @keydown="onKeydown"
          @paste="onPaste"
        />
        <InputGroupAddon align="block-end" class="gap-2">
          <input
            ref="fileInput"
            type="file"
            multiple
            class="hidden"
            @change="attach(($event.target as HTMLInputElement).files); ($event.target as HTMLInputElement).value = ''"
          />
          <InputGroupButton
            variant="ghost"
            size="sm"
            :disabled="!projectId"
            title="Attach a file or an image"
            aria-label="Attach a file"
            @click="fileInput?.click()"
          >
            <Paperclip :size="14" aria-hidden="true" />
          </InputGroupButton>
          <span class="text-muted-foreground text-[10px]">
            enter to send · drop or paste a file
          </span>

          <!-- Stopping the answer, not the conversation: the session and
               everything said before it stay, which is why this is not the
               same control as End this chat. -->
          <InputGroupButton
            v-if="thinking"
            variant="outline"
            size="sm"
            class="ml-auto gap-1.5"
            :disabled="stopping"
            title="Stop this answer. The conversation stays."
            @click="stop"
          >
            <Square :size="11" aria-hidden="true" />
            Stop
          </InputGroupButton>
          <InputGroupButton
            v-else
            variant="default"
            size="sm"
            class="ml-auto"
            :disabled="sending || !canSend"
            @click="send"
          >
            {{ sending ? '…' : 'Ask' }}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
    </div>

    <!-- Closing a tab is a deletion, and says so. It is the one thing here
         that cannot be undone: the transcript, the files attached to it and
         the worktree it ran in all go. -->
    <Dialog :open="!!confirmEnd" @update:open="(v: boolean) => !v && (confirmEnd = null)">
      <DialogContent class="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Close “{{ confirmEnd ? tabLabel(confirmEnd) : '' }}”?</DialogTitle>
          <DialogDescription>
            This conversation is deleted, along with the files attached to it and the worktree it
            ran in. Your other chats, your own checkout, every role's worktree and all task history
            are untouched.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" @click="confirmEnd = null">Cancel</Button>
          <Button
            variant="destructive"
            :disabled="!confirmEnd"
            @click="confirmEnd && endChat(confirmEnd)"
          >
            Close it
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <!-- Explanation you read once, and not at the cost of the conversation.
         On a phone this was 54 permanent pixels out of the 648 the screen
         gave the whole screen; it is in the agent popover, where somebody
         asking "what is this thing" is already looking. -->
    <p class="text-muted-foreground hidden text-[11px] sm:block">
      Runs in its own worktree with no access to the work queue, so a question cannot disturb
      anything in flight.
    </p>
  </div>
</template>

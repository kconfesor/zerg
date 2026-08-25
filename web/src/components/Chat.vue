<script setup lang="ts">
/**
 * A conversation with an agent that has the project open and no part in the
 * pipeline.
 *
 * Built on the same event stream as everything else rather than a history of
 * its own: messages persist, survive a reload, and resume after a dropped
 * connection because they are events, and events already do all three.
 */
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { api, streamActivity, type ActivityEvent, type ActivityStream } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'

const props = defineProps<{ projectId: string | null }>()

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
  scrollToEnd()
}

function connect() {
  stream?.close()
  lines.value = []
  if (!props.projectId) return
  stream = streamActivity(props.projectId, { onEvent: accept, onCaughtUp: scrollToEnd })
}

function scrollToEnd() {
  nextTick(() => {
    const el = viewport.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

async function send() {
  const text = draft.value.trim()
  if (!text || !props.projectId) return
  sending.value = true
  error.value = ''
  try {
    await api.chat(props.projectId, text)
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

watch(() => props.projectId, connect, { immediate: true })
onBeforeUnmount(() => stream?.close())
</script>

<template>
  <div class="flex flex-col gap-3">
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
        <div v-for="l in lines" :key="l.id" class="text-xs">
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
            <p class="leading-relaxed whitespace-pre-wrap">{{ l.text }}</p>
          </template>
        </div>

        <p v-if="thinking" class="text-muted-foreground text-[11px] italic">thinking…</p>
      </div>
    </div>

    <p v-if="error" class="bg-destructive/10 text-destructive px-3 py-2 text-xs">{{ error }}</p>

    <div class="flex items-end gap-2">
      <Textarea
        v-model="draft"
        rows="2"
        class="text-xs"
        placeholder="What does the evaluator do with unary minus?"
        :disabled="!projectId"
        @keydown="onKeydown"
      />
      <Button :disabled="sending || !draft.trim() || !projectId" @click="send">
        {{ sending ? '…' : 'Ask' }}
      </Button>
    </div>
    <p class="text-muted-foreground text-[11px]">
      Runs in its own worktree with no access to the work queue, so a question cannot disturb
      anything in flight.
    </p>
  </div>
</template>

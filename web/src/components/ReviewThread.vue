<script setup lang="ts">
/**
 * One thread of a review: where it points, what has been said, and what to do
 * about it.
 *
 * Its own component because a thread outlives the file it was written on. A
 * remark opened on a revision that was then rejected can point at a file the
 * next revision renamed or deleted, and rendering threads only under the files
 * of the current diff left that one on screen nowhere while it still held the
 * merge: an open remark with no settle button and no way to reach it.
 */
import { HelpCircle, MessageSquare } from '@lucide/vue'
import type { ReviewThread } from '@/lib/api'
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from '@/components/ui/input-group'

const props = defineProps<{
  thread: ReviewThread
  /** Whether an agent is answering this thread right now. */
  awaiting?: boolean
  /** Show the file name as well as the line: true where the thread is not
   *  already under its file. */
  anchored?: boolean
}>()

const draft = defineModel<string>('reply', { default: '' })

const emit = defineEmits<{
  settle: []
  raise: []
  send: []
  ask: []
}>()

function where(t: ReviewThread): string {
  const file = props.anchored ? (t.file ?? '') : ''
  if (t.line) return file ? `${file}:${t.line}` : `line ${t.line}`
  return file || 'this file'
}
</script>

<template>
  <div
    :class="[
      'border-l-2 pl-2',
      thread.state === 'open' ? 'border-l-[var(--status-warning)]' : 'border-l-border',
    ]"
  >
    <div class="text-muted-foreground mb-1 flex items-center gap-1.5 text-[10px]">
      <component
        :is="thread.kind === 'question' ? HelpCircle : MessageSquare"
        :size="10"
        aria-hidden="true"
      />
      <span class="min-w-0 truncate" :title="thread.file">{{ where(thread) }}</span>
      <span v-if="thread.kind === 'question'" class="shrink-0">· asked</span>
      <span v-else-if="thread.state === 'resolved'" class="shrink-0">· settled</span>
      <!-- A question holds nothing, so what it offers is a way to make it
           matter rather than a way to dismiss it. -->
      <button
        v-if="thread.kind === 'question'"
        type="button"
        class="hover:text-foreground focus-visible:outline-ring ml-auto -my-1.5 inline-flex min-h-8 shrink-0 items-center px-1 underline-offset-2 hover:underline focus-visible:outline-2 sm:my-0 sm:min-h-0 sm:px-0"
        title="Make this a remark, which has to be settled before the work lands"
        @click="emit('raise')"
      >
        raise it
      </button>
      <button
        v-else
        type="button"
        class="hover:text-foreground focus-visible:outline-ring ml-auto -my-1.5 inline-flex min-h-8 shrink-0 items-center px-1 underline-offset-2 hover:underline focus-visible:outline-2 sm:my-0 sm:min-h-0 sm:px-0"
        @click="emit('settle')"
      >
        {{ thread.state === 'open' ? 'settle' : 'reopen' }}
      </button>
    </div>
    <p v-for="c in thread.comments" :key="c.id" class="mb-1 text-[11px] leading-relaxed">
      <span class="text-muted-foreground font-semibold">{{ c.author }}</span>
      <span class="ml-1.5">{{ c.body }}</span>
    </p>
    <p v-if="awaiting" class="text-muted-foreground mb-1 text-[10px] italic">asking the agent…</p>
    <InputGroup>
      <InputGroupInput
        v-model="draft"
        :placeholder="thread.kind === 'question' ? 'ask a follow-up' : 'reply'"
        @keyup.enter="thread.kind === 'question' ? emit('ask') : emit('send')"
      />
      <InputGroupAddon align="inline-end">
        <InputGroupButton v-if="thread.kind === 'question'" size="sm" @click="emit('ask')">
          Ask
        </InputGroupButton>
        <InputGroupButton v-else size="sm" @click="emit('send')">Send</InputGroupButton>
      </InputGroupAddon>
    </InputGroup>
  </div>
</template>

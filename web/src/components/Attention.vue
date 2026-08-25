<script setup lang="ts">
import { ref } from 'vue'
import type { Attention } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

const props = defineProps<{ attention: Attention | null }>()
const emit = defineEmits<{
  approve: [id: string]
  reject: [id: string, note: string]
  answer: [id: string, answer: string]
}>()

const notes = ref<Record<string, string>>({})
const answers = ref<Record<string, string>>({})

function total(a: Attention | null): number {
  if (!a) return 0
  return a.approvals.length + a.clarifications.length + a.rework.tasks.length
}
</script>

<template>
  <div v-if="props.attention" class="flex flex-col gap-4">
    <p v-if="!total(props.attention)" class="text-muted-foreground text-xs">Nothing needs you.</p>

    <!-- Approvals: a spec waiting to be read before anything downstream runs. -->
    <div v-for="a in props.attention.approvals" :key="a.id" class="border p-3">
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <Badge>approval</Badge>
        <span class="text-xs font-medium">{{ a.taskName || 'untitled' }}</span>
        <span class="text-muted-foreground text-xs">from {{ a.fromRole }}</span>
      </div>
      <Input v-model="notes[a.id]" placeholder="reason, if rejecting" class="mb-2" />
      <div class="flex gap-2">
        <Button size="sm" @click="emit('approve', a.id)">Approve</Button>
        <Button size="sm" variant="destructive" @click="emit('reject', a.id, notes[a.id] ?? '')">
          Reject
        </Button>
      </div>
    </div>

    <!-- Questions. Without somewhere to put these, an agent waiting on an
         answer looks exactly like one that stopped for no reason. -->
    <div v-for="c in props.attention.clarifications" :key="c.id" class="border p-3">
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <Badge variant="secondary">question</Badge>
        <span class="text-muted-foreground text-xs">{{ c.role }} asks</span>
      </div>
      <p class="mb-2 text-xs break-words">{{ c.question }}</p>
      <div class="flex gap-2">
        <Input
          v-model="answers[c.id]"
          placeholder="your answer"
          class="min-w-0 flex-1"
          @keyup.enter="emit('answer', c.id, answers[c.id] ?? '')"
        />
        <Button size="sm" @click="emit('answer', c.id, answers[c.id] ?? '')">Answer</Button>
      </div>
    </div>

    <!-- Cards going in circles. Informational: rework is legitimate, and
         blocking the first bounce would train everyone to ignore this panel. -->
    <div
      v-for="t in props.attention.rework.tasks"
      :key="t.id"
      class="border border-[var(--status-warning)]/40 bg-[var(--status-warning)]/5 p-3"
    >
      <div class="mb-1 flex flex-wrap items-center gap-2">
        <Badge variant="secondary">looping</Badge>
        <span class="text-xs font-medium">{{ t.name }}</span>
      </div>
      <p class="text-muted-foreground text-xs">
        Sent backward {{ t.reworkCount }} times, at or over the threshold of
        {{ props.attention.rework.threshold }}. Two roles probably disagree about something worth
        settling.
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import type { Attention } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

const props = defineProps<{ attention: Attention | null; compact?: boolean }>()
const emit = defineEmits<{
  approve: [id: string]
  reject: [id: string, note: string]
  answer: [id: string, answer: string]
}>()

const notes = ref<Record<string, string>>({})
const answers = ref<Record<string, string>>({})

function empty(a: Attention | null): boolean {
  if (!a) return true
  return !a.approvals.length && !a.clarifications.length && !a.rework.tasks.length
}
</script>

<template>
  <div v-if="props.attention" class="flex flex-col gap-3">
    <!-- Nothing waiting should feel like calm, not like a broken panel. -->
    <div
      v-if="empty(props.attention)"
      class="text-muted-foreground/70 flex flex-col items-center gap-1 py-12 text-center"
    >
      <span class="text-[var(--status-good)]/70 text-lg leading-none">✓</span>
      <p class="text-xs">Nothing needs you.</p>
      <p class="text-[11px]">Approvals, questions and looping cards appear here.</p>
    </div>

    <!-- Approvals: a spec waiting to be read before anything downstream runs. -->
    <article
      v-for="a in props.attention.approvals"
      :key="a.id"
      class="rise bg-card border-l-primary border border-l-2 p-3"
    >
      <div class="mb-2.5 flex flex-wrap items-center gap-2">
        <Badge>approval</Badge>
        <span class="text-xs font-semibold">{{ a.taskName || 'untitled' }}</span>
        <span class="text-muted-foreground text-[11px]">from {{ a.fromRole }}</span>
      </div>
      <div class="flex flex-wrap gap-2">
        <Input
          v-model="notes[a.id]"
          placeholder="reason, if rejecting"
          class="min-w-0 flex-1"
        />
        <Button size="sm" @click="emit('approve', a.id)">Approve</Button>
        <Button size="sm" variant="destructive" @click="emit('reject', a.id, notes[a.id] ?? '')">
          Reject
        </Button>
      </div>
    </article>

    <!-- Questions. Without somewhere to put these, an agent waiting on an
         answer looks exactly like one that stopped for no reason. -->
    <article
      v-for="c in props.attention.clarifications"
      :key="c.id"
      class="rise bg-card border border-l-2 border-l-[var(--status-warning)] p-3"
    >
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <Badge variant="secondary">question</Badge>
        <span class="text-muted-foreground text-[11px]">{{ c.role }} asks</span>
      </div>
      <p class="mb-2.5 text-xs leading-relaxed break-words">{{ c.question }}</p>
      <div class="flex gap-2">
        <Input
          v-model="answers[c.id]"
          placeholder="your answer"
          class="min-w-0 flex-1"
          @keyup.enter="emit('answer', c.id, answers[c.id] ?? '')"
        />
        <Button size="sm" @click="emit('answer', c.id, answers[c.id] ?? '')">Answer</Button>
      </div>
    </article>

    <!-- Cards going in circles. Informational: rework is legitimate, and
         blocking the first bounce would train everyone to ignore this panel. -->
    <article
      v-for="t in props.attention.rework.tasks"
      :key="t.id"
      class="rise border border-l-2 border-[var(--status-warning)]/35 border-l-[var(--status-warning)] bg-[var(--status-warning)]/[0.06] p-3"
    >
      <div class="mb-1.5 flex flex-wrap items-center gap-2">
        <Badge variant="secondary">looping</Badge>
        <span class="text-xs font-semibold">{{ t.name }}</span>
      </div>
      <p class="text-muted-foreground text-[11px] leading-relaxed">
        Sent backward <span class="tabular text-foreground">{{ t.reworkCount }}</span> times, at or
        over the threshold of {{ props.attention.rework.threshold }}. Two roles probably disagree
        about something worth settling.
      </p>
    </article>
  </div>
</template>

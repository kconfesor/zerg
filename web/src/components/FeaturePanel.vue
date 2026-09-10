<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, shallowRef, useId, watch } from 'vue'
import { api, type FeatureDetail, type Task } from '@/lib/api'
import { latest } from '@/lib/latest'
import { renderMarkdown } from '@/lib/markdown'
import FeatureFiles from '@/components/FeatureFiles.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

// One durable view in Attention, on the board and in history. Commands and their
// errors stay here, so a failed merge never renders behind the enclosing dialog.
const props = defineProps<{ featureId: string; version?: string }>()
const emit = defineEmits<{ updated: [task: Task]; openTask: [task: Task] }>()
const detail = ref<FeatureDetail | null>(null)
const failed = shallowRef('')
const loadFailed = shallowRef('')
const busy = shallowRef(false)
const note = shallowRef('')
const noteId = useId()
const newest = latest()

async function load() {
  const current = newest()
  try {
    const result = await api.featureDetail(props.featureId)
    if (current()) { detail.value = result; loadFailed.value = '' }
  } catch (e) {
    if (current()) loadFailed.value = e instanceof Error ? e.message : String(e)
  }
}
watch(() => props.featureId, () => {
  detail.value = null
  failed.value = ''
  note.value = ''
})
watch(() => [props.featureId, props.version], load, { immediate: true })
// Decisions keep arriving while a board/history dialog stays open. Poll only
// while this view is mounted; do not erase an action's error with a later read.
let poll: ReturnType<typeof setInterval> | undefined
onMounted(() => { poll = setInterval(() => { if (!busy.value) void load() }, 5000) })
onUnmounted(() => { clearInterval(poll); newest() })

async function act(command: () => Promise<unknown>) {
  if (busy.value) return
  busy.value = true
  failed.value = ''
  try {
    await command()
    note.value = ''
    await load()
    if (detail.value) emit('updated', detail.value.task)
  } catch (e) {
    failed.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

const run = computed(() => detail.value?.run)
const live = computed(() => run.value?.state === 'running' || run.value?.state === 'conflict')
const plan = computed(() => detail.value?.plans.at(-1))
const review = computed(() => [...(detail.value?.reviews ?? [])].reverse().find((v) => v.headSha === run.value?.headSha))
const allDone = computed(() => {
  const d = detail.value
  return !!d && plan.value?.state === 'approved' && !!plan.value.items?.length &&
    plan.value.items.every((i) => d.children.some((c) => c.id === i.childTaskId && c.state === 'done')) &&
    d.children.every((c) => c.state === 'done')
})
const canLand = computed(() => run.value?.state === 'running' && allDone.value && review.value?.verdict === 'ok')
const landLabel = computed(() => detail.value?.integration === 'pr' ? 'Open pull request' :
  detail.value?.integration === 'branch' ? 'Approve; leave on branch' : `Land on ${detail.value?.baseBranch ?? 'base'}`)
const totalTokens = computed(() => {
  const u = detail.value?.usage
  return u ? u.inputTokens + u.cacheReadTokens + u.cacheWriteTokens + u.outputTokens : 0
})
function retryable(c: Task) {
  return live.value && (c.state === 'rejected' || (c.state === 'done' && review.value?.verdict === 'reject'))
}
function money(n: number) { return `$${n.toFixed(2)}` }
</script>

<template>
  <section class="min-w-0 space-y-3" aria-label="Feature review and history">
    <p v-if="failed || loadFailed" role="alert" class="text-destructive break-words text-xs">{{ failed || loadFailed }}</p>
    <p v-if="!detail" class="text-muted-foreground text-xs">Reading the feature…</p>
    <template v-else>
      <div class="flex flex-wrap items-center gap-2">
        <Badge>feature</Badge>
        <h3 class="min-w-0 break-words text-sm font-semibold">{{ detail.task.name }}</h3>
        <Badge variant="outline">{{ run?.state ?? (plan?.state === 'pending' ? 'plan approval' : 'planning') }}</Badge>
        <span class="text-muted-foreground text-xs">{{ totalTokens.toLocaleString() }} tokens · {{ money(detail.usage.costUsd) }}</span>
      </div>
      <p v-if="detail.usage.unpricedTurns" class="text-muted-foreground text-xs">
        Cost is incomplete: {{ detail.usage.unpricedTurns }} turns have no reported price.
      </p>
      <div class="md min-w-0 break-words text-xs" v-html="renderMarkdown(detail.task.body)" />

      <div v-for="p in detail.plans" :key="p.id" class="min-w-0 border p-3">
        <p class="mb-2 text-xs font-medium">Plan {{ p.n }} · {{ p.state }} · {{ p.itemCount }} subtasks</p>
        <p v-if="p.state === 'pending'" class="text-muted-foreground mb-2 text-xs">
          {{ p.estimateTokens ? `${p.estimateTokens.toLocaleString()} tokens · ${money(p.estimateCostUsd)} estimated from completed cards` : 'No completed-card history to estimate from' }}.
          Accepting creates these cards and starts the work.
        </p>
        <ol class="mb-2 space-y-2 text-xs">
          <li v-for="item in p.items ?? []" :key="item.id" class="break-words">
            <strong>{{ item.name }}</strong> · priority {{ item.priority }}
            <span v-if="item.after?.length"> · after {{ item.after.join(', ') }}</span>
            <p class="text-muted-foreground">{{ item.body }}</p>
            <p v-if="live && p.state === 'approved' && !detail.children.some(c => c.id === item.childTaskId)" class="text-destructive">Required card is missing; this feature cannot land.</p>
          </li>
        </ol>
        <p v-if="p.note" class="mb-2 text-xs">{{ p.decidedBy || 'operator' }}: {{ p.note }}</p>
        <FeatureFiles v-if="p.proseSha" :feature-id="featureId" :head="p.proseSha" :evidence="p.proseSha" />
        <p v-else class="text-muted-foreground text-xs">No prose evidence was attached to this revision.</p>
        <div v-if="p.state === 'pending'" class="mt-3 flex flex-wrap gap-2">
          <Button size="sm" :disabled="busy" @click="act(() => api.approvePlan(p.id))">Accept plan</Button>
          <Button variant="outline" size="sm" :disabled="busy || !note.trim()" @click="act(() => api.rejectPlan(p.id, note))">Reject plan</Button>
        </div>
      </div>

      <div v-if="detail.children.length" class="space-y-2">
        <p class="text-xs font-medium">Cards in this feature</p>
        <div v-for="child in detail.children" :key="child.id" class="flex min-w-0 flex-wrap items-center gap-2 text-xs">
          <Button variant="link" size="sm" class="h-auto max-w-full justify-start whitespace-normal p-0 text-left" @click="emit('openTask', child)">{{ child.name }}</Button>
          <span>{{ child.blocked ? 'blocked' : child.state }}</span>
          <Button v-if="retryable(child)" variant="outline" size="sm" :disabled="busy" @click="act(() => api.retryCard(child.id))">Retry {{ child.name }}</Button>
          <Button v-if="live && child.blocked && child.state === 'queued'" variant="outline" size="sm" :disabled="busy || !note.trim()" @click="act(() => api.waiveDependency(child.id, note))">Release {{ child.name }}</Button>
        </div>
      </div>

      <div v-for="v in detail.reviews" :key="v.id" class="min-w-0 border p-3 text-xs">
        <p class="mb-2 font-medium">{{ v.decidedBy }}: {{ v.verdict }} · {{ v.headSha.slice(0, 12) }} {{ v.headSha !== run?.headSha ? '(previous head)' : '' }}</p>
        <div class="md mb-2 break-words" v-html="renderMarkdown(v.note ?? '')" />
        <FeatureFiles v-if="v.evidenceSha" :feature-id="featureId" :head="v.evidenceSha" :evidence="v.evidenceSha" />
      </div>

      <FeatureFiles v-if="run?.headSha" :feature-id="featureId" :head="run.headSha" />
      <p v-if="live && allDone && !review" class="text-muted-foreground text-xs">Waiting for the architect to check this head against the original brief and accepted plan.</p>
      <p v-if="live && review?.verdict === 'reject'" class="text-xs">The feature was sent back. Retry a card above to make the correction; the new head will receive a fresh review.</p>
      <p v-if="run?.state === 'conflict'" class="text-destructive text-xs">Integration conflicted. Reject the affected handoff in Attention so its author can resolve the conflict.</p>

      <div v-if="live || plan?.state === 'pending'" class="space-y-1.5">
        <Label :for="noteId">Reason for sending back or releasing work</Label>
        <Textarea :id="noteId" v-model="note" placeholder="What must change, or why this dependency can be waived" />
      </div>
      <div v-if="live" class="flex flex-wrap gap-2">
        <Button v-if="canLand" size="sm" :disabled="busy" @click="act(() => api.landFeature(featureId, run!.headSha))">{{ landLabel }}</Button>
        <Button v-if="allDone" variant="outline" size="sm" :disabled="busy || !note.trim()" @click="act(() => api.rejectFeature(featureId, run!.headSha, note))">Send back for correction</Button>
        <Button variant="outline" size="sm" :disabled="busy" @click="act(() => api.refreshFeature(featureId, run!.headSha))">Refresh from {{ detail.baseBranch }}</Button>
      </div>
      <Button v-if="!run || live" variant="outline" size="sm" :disabled="busy" @click="act(() => api.cancelFeature(featureId))">Cancel feature</Button>

      <Collapsible v-if="detail.history.length" class="min-w-0">
        <CollapsibleTrigger as-child><Button variant="ghost" size="sm">Feature history ({{ detail.history.length }})</Button></CollapsibleTrigger>
        <CollapsibleContent>
          <ol class="space-y-2 border-l pl-3 text-xs">
            <li v-for="(step, index) in detail.history" :key="index" class="break-words">
              <span class="text-muted-foreground">{{ step.from }} · {{ new Date(step.at).toLocaleString() }}</span>
              <p>{{ step.body }}</p>
            </li>
          </ol>
        </CollapsibleContent>
      </Collapsible>
    </template>
  </section>
</template>

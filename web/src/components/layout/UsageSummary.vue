<script setup lang="ts">
/**
 * What this project has spent, in the bar that already carries its identity.
 *
 * The trigger is a hero number — one figure, always visible, because cost is
 * the thing you want to notice without going to look for it. The breakdown
 * behind it is a table rather than a chart: with a handful of models, exact
 * figures in aligned columns are read faster than any plot of them, and they
 * are the numbers people actually quote.
 */
import { computed, ref, watch } from 'vue'
import { api, type UsageTotal } from '@/lib/api'
import { latest } from '@/lib/latest'
import { Button } from '@/components/ui/button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

const props = defineProps<{ projectId: string | null; refreshKey?: number }>()

const byModel = ref<UsageTotal[]>([])
const byRole = ref<UsageTotal[]>([])
const failed = ref(false)

// Two requests per load, per project, with a switch able to happen between
// them: without this the previous project's totals can land last and be shown,
// to four decimal places, under the current project's name.
const newest = latest()

async function load() {
  const current = newest()
  if (!props.projectId) {
    byModel.value = []
    byRole.value = []
    return
  }
  try {
    const [m, r] = await Promise.all([
      api.usage(props.projectId, 'model'),
      api.usage(props.projectId, 'role'),
    ])
    if (!current()) return
    byModel.value = m
    byRole.value = r
    failed.value = false
  } catch {
    if (!current()) return
    failed.value = true
  }
}
watch(() => [props.projectId, props.refreshKey], load, { immediate: true })

const totals = computed(() => {
  const sum = (pick: (t: UsageTotal) => number) => byModel.value.reduce((a, t) => a + pick(t), 0)
  return {
    cost: sum((t) => t.costUsd),
    turns: sum((t) => t.turns),
    unpriced: sum((t) => t.unpricedTurns),
    subscription: sum((t) => t.subscriptionTurns),
    // Cache reads are ~0.1x and cache writes 1.25-2x, so they are shown
    // apart. A single "input" figure would misstate a cache-heavy turn by
    // roughly an order of magnitude.
    input: sum((t) => t.inputTokens),
    cacheRead: sum((t) => t.cacheReadTokens),
    cacheWrite: sum((t) => t.cacheWriteTokens),
    output: sum((t) => t.outputTokens),
  }
})

const tokens = computed(
  () => totals.value.input + totals.value.cacheRead + totals.value.cacheWrite + totals.value.output,
)

/** Every turn on a plan means the dollar figure is an estimate, not a bill. */
const allOnPlan = computed(
  () => totals.value.turns > 0 && totals.value.subscription === totals.value.turns,
)

const num = new Intl.NumberFormat()
function compact(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${Math.round(n / 1000)}k`
  return String(n)
}
function money(n: number): string {
  return n >= 1 ? `$${n.toFixed(2)}` : `$${n.toFixed(4)}`
}

/** A model with no name is a harness that did not report one — say so rather
 * than rendering an empty cell that reads like a rendering bug. */
function modelName(key: string): string {
  return key || 'not reported'
}

/** The cost ramp is ordinal and single-hue, ordered by unit price. */
const tiers = [
  { key: 'cacheRead', label: 'cache read', tier: 'var(--tier-1)', note: '~0.1×' },
  { key: 'input', label: 'input', tier: 'var(--tier-2)', note: '1×' },
  { key: 'cacheWrite', label: 'cache write', tier: 'var(--tier-3)', note: '1.25–2×' },
  { key: 'output', label: 'output', tier: 'var(--tier-4)', note: '5×' },
] as const
</script>

<template>
  <Popover v-if="projectId">
    <PopoverTrigger as-child>
      <!-- The figures alone. The word "usage" labelled what the numbers
           already say — a token count and a dollar amount are not mistakable
           for anything else — and the asterisk marked a caveat you could only
           resolve by opening the thing it was attached to. The caveat is still
           told, in the tooltip and in full inside the panel. -->
      <Button
        variant="ghost"
        size="sm"
        class="gap-2.5 tabular-nums"
        :disabled="!totals.turns"
        :title="
          allOnPlan
            ? 'Every turn ran on a plan, so this is what the tokens would have cost metered'
            : undefined
        "
      >
        <span v-if="totals.turns" class="text-[13px] font-semibold">
          {{ compact(tokens) }} tok
        </span>
        <span v-if="totals.turns" class="text-[13px] font-semibold">
          {{ money(totals.cost) }}
        </span>
        <span v-else class="text-muted-foreground text-[11px]">no usage yet</span>
      </Button>
    </PopoverTrigger>

    <PopoverContent class="w-[calc(100vw-1.5rem)] max-w-[26rem]" align="end">
      <p v-if="failed" class="text-destructive text-xs">Could not read usage.</p>

      <template v-else>
        <!-- Per model, which is what an operator changes when cost is wrong. -->
        <h3 class="mb-2 text-xs font-semibold">By model</h3>
        <table class="w-full text-[11px] tabular-nums">
          <thead class="text-muted-foreground">
            <tr class="hairline-b">
              <th class="py-1 text-left font-medium">model</th>
              <th class="py-1 text-right font-medium">turns</th>
              <th class="py-1 text-right font-medium">tokens</th>
              <th class="py-1 text-right font-medium">cost</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in byModel" :key="m.key">
              <td class="py-1" :class="m.key ? '' : 'text-muted-foreground italic'">
                {{ modelName(m.key) }}
              </td>
              <td class="py-1 text-right">{{ m.turns }}</td>
              <td class="py-1 text-right">
                {{
                  compact(m.inputTokens + m.cacheReadTokens + m.cacheWriteTokens + m.outputTokens)
                }}
              </td>
              <td class="py-1 text-right">{{ money(m.costUsd) }}</td>
            </tr>
          </tbody>
        </table>

        <h3 class="mt-3 mb-2 text-xs font-semibold">By role</h3>
        <table class="w-full text-[11px] tabular-nums">
          <tbody>
            <tr v-for="r in byRole" :key="r.key">
              <td class="py-1">{{ r.key }}</td>
              <td class="py-1 text-right">{{ r.turns }} turns</td>
              <td class="py-1 text-right">{{ money(r.costUsd) }}</td>
            </tr>
          </tbody>
        </table>

        <!-- The token split, ordered by unit price. One "input" figure would
             hide the difference between a cached turn and an uncached one. -->
        <h3 class="mt-3 mb-1.5 text-xs font-semibold">Where the tokens went</h3>
        <div class="flex h-2 w-full gap-[2px] overflow-hidden">
          <span
            v-for="t in tiers"
            :key="t.key"
            :style="{
              background: t.tier,
              width: `${tokens ? ((totals as any)[t.key] / tokens) * 100 : 0}%`,
            }"
            :title="`${t.label} ${num.format((totals as any)[t.key])}`"
          />
        </div>
        <ul class="mt-1.5 space-y-0.5">
          <li v-for="t in tiers" :key="t.key" class="flex items-center gap-1.5 text-[11px]">
            <span class="size-2 shrink-0" :style="{ background: t.tier }" aria-hidden="true" />
            <span>{{ t.label }}</span>
            <span class="text-muted-foreground">{{ t.note }}</span>
            <span class="ml-auto tabular-nums">{{ num.format((totals as any)[t.key]) }}</span>
          </li>
        </ul>

        <!-- Two things that would make the figure above a lie if left unsaid. -->
        <p v-if="allOnPlan" class="text-muted-foreground mt-3 text-[11px] leading-snug">
          <span class="font-semibold">*</span> Every turn ran on a plan, so this is what the tokens
          would have cost metered. The marginal cost was zero.
        </p>
        <p v-if="totals.unpriced" class="text-muted-foreground mt-1.5 text-[11px] leading-snug">
          {{ totals.unpriced }} of {{ totals.turns }} turns reported no cost. Their tokens are
          counted; their price is not, so the total above is the part that was priced.
        </p>
      </template>
    </PopoverContent>
  </Popover>
</template>

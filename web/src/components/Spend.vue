<script setup lang="ts">
/**
 * Where the money went.
 *
 * One read of `usage_turns` at four grains — window, role, provider, model —
 * and one request, because four requests about the same window can disagree:
 * a turn recorded between two of them appears in the roles and not in the
 * providers, and the columns stop adding up for reasons nobody can see.
 *
 * Two palettes, each doing the job its kind is for. The four token classes are
 * an *ordinal* scale: one hue, monotone lightness, ordered by unit price, so a
 * lighter segment is a more expensive one and the ordering carries meaning
 * without being read. Providers are *categorical*: distinct hues, fixed order,
 * never cycled. Both are the theme's own slots, validated against this
 * surface — see the note at the top of theme.css, and re-run the validator
 * rather than re-picking by eye.
 */
import { computed, ref, watch } from 'vue'
import { api, type RoleUsage, type Spend, type SpendRange } from '@/lib/api'
import { latest } from '@/lib/latest'
import { tokens as fmtTokens } from '@/lib/utils'

const props = defineProps<{ projectId: string | null }>()

/**
 * Ordered by unit price, cheapest first, which is also the order they stack.
 *
 * The multipliers are the whole argument for showing four numbers instead of
 * one: cache reads and output differ by ~50x, so a bar that summed them would
 * describe nothing.
 */
const CLASSES = [
  { key: 'cacheReadTokens', label: 'Cache read', mult: '0.1×', hue: 'var(--tier-1)' },
  { key: 'inputTokens', label: 'Input', mult: '1×', hue: 'var(--tier-2)' },
  { key: 'cacheWriteTokens', label: 'Cache write', mult: '1.25×', hue: 'var(--tier-3)' },
  { key: 'outputTokens', label: 'Output', mult: '5×', hue: 'var(--tier-4)' },
] as const

const RANGES: { value: SpendRange; label: string }[] = [
  { value: 'session', label: 'Session' },
  { value: '24h', label: '24 hours' },
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: 'all', label: 'All' },
]

const range = ref<SpendRange>('session')
const provider = ref<string>('')
const data = ref<Spend | null>(null)
const failed = ref('')
const loading = ref(false)

const newest = latest()

async function load() {
  if (!props.projectId) {
    data.value = null
    return
  }
  const current = newest()
  loading.value = true
  try {
    const d = await api.spend(props.projectId, range.value)
    if (!current()) return
    data.value = d
    failed.value = ''
    // A provider filter that survives a window change can select nothing and
    // read as "no spend" rather than "not in this window".
    if (provider.value && !d.providers.some((p) => p.key === provider.value)) provider.value = ''
  } catch (e) {
    if (!current()) return
    failed.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (current()) loading.value = false
  }
}
watch([() => props.projectId, range], load, { immediate: true })

/** Providers keep their hue whatever is filtered: colour follows the entity,
 *  never its rank, so narrowing the list must not repaint the survivors. */
const providerHue = computed(() => {
  const out = new Map<string, string>()
  ;(data.value?.providers ?? []).forEach((p, i) => {
    out.set(p.key, `var(--chart-${(i % 4) + 1})`)
  })
  return out
})

/** The rows the filter chips scope. */
const rows = computed<RoleUsage[]>(() => {
  const all = data.value?.roles ?? []
  if (!provider.value) return all
  return all.filter((r) => r.providers.includes(provider.value))
})

function classesOf(r: RoleUsage) {
  return CLASSES.map((c) => ({ ...c, value: r[c.key] as number }))
}
function tokensOf(r: RoleUsage): number {
  return r.cacheReadTokens + r.inputTokens + r.cacheWriteTokens + r.outputTokens
}
/** Cache reads as a share of everything that went in. Output is not input and
 *  is deliberately outside the denominator. */
function hitRate(r: { cacheReadTokens: number; inputTokens: number; cacheWriteTokens: number }): number {
  const den = r.cacheReadTokens + r.inputTokens + r.cacheWriteTokens
  return den ? Math.round((r.cacheReadTokens / den) * 100) : 0
}

/** Flagged roles, by name, so a row can mark itself. */
const flagged = computed(() => new Map((data.value?.flags ?? []).map((f) => [f.role, f])))

/**
 * How much more the same input costs at the fallen rate.
 *
 * A cache read is ~0.1x uncached input, not free, so the comparison is between
 * two blended prices rather than between the uncached fractions. Dividing those
 * fractions — 93% against 5% — reads as 18x and is simply wrong: it prices a
 * cache read at zero. At 95% the blend is 0.145, at 7% it is 0.937, which is
 * the 6x this actually is. An overstated number in a warning is worse than no
 * number, because the first time someone checks it the whole panel loses them.
 */
function inputMultiple(f: { recent: number; trailing: number }): number {
  const blended = (rate: number) => 1 - rate + rate * 0.1
  return Math.max(2, Math.round(blended(f.recent) / blended(f.trailing)))
}

/** "14 minutes ago", "3 hours ago" — the shape the cause is usually stated in. */
function ago(iso: string): string {
  const mins = Math.round((Date.now() - new Date(iso).getTime()) / 60000)
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins} minute${mins === 1 ? '' : 's'} ago`
  const hours = Math.round(mins / 60)
  return `${hours} hour${hours === 1 ? '' : 's'} ago`
}

/** The widest role sets the scale, so a row's length reads as volume rather
 *  than every row filling the track. */
const widest = computed(() => Math.max(1, ...rows.value.map(tokensOf)))

const totals = computed(() => {
  const t = {
    cost: 0, turns: 0, cacheReadTokens: 0, inputTokens: 0,
    cacheWriteTokens: 0, outputTokens: 0, subscription: 0, unpriced: 0,
    tasks: new Set<string>(), estimated: 0,
  }
  for (const r of rows.value) {
    t.cost += r.costUsd
    t.turns += r.turns
    t.cacheReadTokens += r.cacheReadTokens
    t.inputTokens += r.inputTokens
    t.cacheWriteTokens += r.cacheWriteTokens
    t.outputTokens += r.outputTokens
    t.subscription += r.subscriptionTurns
    t.unpriced += r.unpricedTurns
    if (r.subscriptionTurns > 0) t.estimated += r.costUsd
  }
  return t
})

const tokenTotal = computed(
  () =>
    totals.value.cacheReadTokens + totals.value.inputTokens +
    totals.value.cacheWriteTokens + totals.value.outputTokens,
)
/** Cards, not card-visits: a task worked by three roles is one card. */
const taskCount = computed(() => rows.value.reduce((n, r) => Math.max(n, 0) + r.tasks, 0))

function money(n: number): string {
  if (n === 0) return '$0'
  return n >= 1 ? `$${n.toFixed(2)}` : `$${n.toFixed(3)}`
}
function share(part: number, whole: number): number {
  return whole ? Math.round((part / whole) * 100) : 0
}

/** Hover and focus both raise it, so the reading is not mouse-only. */
const tip = ref<{ x: number; y: number; role: string; label: string; mult: string; value: number; pct: number } | null>(null)

function showTip(ev: MouseEvent | FocusEvent, r: RoleUsage, c: { label: string; mult: string; value: number }) {
  const el = ev.currentTarget as HTMLElement
  const box = el.getBoundingClientRect()
  tip.value = {
    x: box.left + box.width / 2,
    y: box.top - 8,
    role: r.role,
    label: c.label,
    mult: c.mult,
    value: c.value,
    pct: share(c.value, tokensOf(r)),
  }
}
</script>

<template>
  <div class="flex flex-col gap-4">
    <p v-if="failed" class="bg-destructive/10 text-destructive px-3 py-2 text-xs">{{ failed }}</p>

    <!-- One filter row, above everything it scopes. -->
    <div class="hairline-b flex flex-wrap items-center gap-2 pb-3">
      <span class="text-muted-foreground mr-1 text-[10px] font-bold tracking-[0.12em] uppercase">
        Range
      </span>
      <button
        v-for="r in RANGES"
        :key="r.value"
        type="button"
        :aria-pressed="range === r.value"
        :class="[
          'focus-visible:outline-ring border px-2.5 py-1 text-[11px] transition-colors focus-visible:outline-2',
          range === r.value
            ? 'border-primary/50 bg-primary/[0.12] text-foreground font-semibold'
            : 'hover:bg-muted text-muted-foreground border-transparent',
        ]"
        @click="range = r.value"
      >
        {{ r.label }}
      </button>

      <span v-if="(data?.providers.length ?? 0) > 1" class="bg-border mx-1 h-5 w-px" />
      <template v-if="(data?.providers.length ?? 0) > 1">
        <span class="text-muted-foreground mr-1 text-[10px] font-bold tracking-[0.12em] uppercase">
          Provider
        </span>
        <button
          type="button"
          :aria-pressed="provider === ''"
          :class="[
            'focus-visible:outline-ring border px-2.5 py-1 text-[11px] transition-colors focus-visible:outline-2',
            provider === ''
              ? 'border-primary/50 bg-primary/[0.12] text-foreground font-semibold'
              : 'hover:bg-muted text-muted-foreground border-transparent',
          ]"
          @click="provider = ''"
        >
          All
        </button>
        <button
          v-for="p in data?.providers ?? []"
          :key="p.key"
          type="button"
          :aria-pressed="provider === p.key"
          :class="[
            'focus-visible:outline-ring flex items-center gap-1.5 border px-2.5 py-1 text-[11px] transition-colors focus-visible:outline-2',
            provider === p.key
              ? 'border-primary/50 bg-primary/[0.12] text-foreground font-semibold'
              : 'hover:bg-muted text-muted-foreground border-transparent',
          ]"
          @click="provider = p.key"
        >
          <span class="size-2 shrink-0" :style="{ backgroundColor: providerHue.get(p.key) }" />
          {{ p.key || 'unnamed' }}
          <span class="text-muted-foreground tabular">{{ money(p.costUsd) }}</span>
        </button>
      </template>

      <span
        v-if="data && range === 'session' && !data.sessionStarted"
        class="text-muted-foreground ml-auto text-[11px]"
      >
        This project has not been started, so there is no session yet.
      </span>
    </div>

    <p v-if="loading && !data" class="text-muted-foreground text-xs">Reading the ledger…</p>

    <template v-else-if="rows.length">
      <!-- Four figures, each answering a question you would otherwise open
           something to ask. No plot, so no tooltip: a stat tile is the answer. -->
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <div class="bg-card hairline p-3.5">
          <div class="text-muted-foreground text-[10px] font-bold tracking-[0.12em] uppercase">
            Spend
          </div>
          <div class="mt-1.5 text-2xl leading-none font-semibold">{{ money(totals.cost) }}</div>
          <p class="text-muted-foreground mt-1.5 text-[11px]">
            <template v-if="totals.estimated">
              <span class="text-[var(--status-warning)]">{{ money(totals.estimated) }}</span>
              estimated at API rates
            </template>
            <template v-else>{{ totals.turns }} turns</template>
          </p>
        </div>

        <div class="bg-card hairline p-3.5">
          <div class="text-muted-foreground text-[10px] font-bold tracking-[0.12em] uppercase">
            Tokens
          </div>
          <div class="mt-1.5 text-2xl leading-none font-semibold">{{ fmtTokens(tokenTotal) }}</div>
          <p class="text-muted-foreground mt-1.5 text-[11px]">
            <b class="text-foreground font-medium">{{ fmtTokens(totals.outputTokens) }}</b>
            output · the priciest {{ share(totals.outputTokens, tokenTotal) }}%
          </p>
        </div>

        <div class="bg-card hairline p-3.5">
          <div class="text-muted-foreground text-[10px] font-bold tracking-[0.12em] uppercase">
            Cache hit rate
          </div>
          <div class="mt-1.5 text-2xl leading-none font-semibold">{{ hitRate(totals) }}%</div>
          <p class="text-muted-foreground mt-1.5 text-[11px]">
            of input served at <b class="text-foreground font-medium">~0.1×</b> price
          </p>
        </div>

        <div class="bg-card hairline p-3.5">
          <div class="text-muted-foreground text-[10px] font-bold tracking-[0.12em] uppercase">
            Cards worked
          </div>
          <div class="mt-1.5 text-2xl leading-none font-semibold">{{ taskCount }}</div>
          <p class="text-muted-foreground mt-1.5 text-[11px]">
            counted per role, so one card worked by two roles counts twice
          </p>
        </div>
      </div>

      <!-- The centrepiece. -->
      <section class="bg-card hairline p-4">
        <div class="mb-1 flex flex-wrap items-baseline gap-x-3">
          <h2 class="text-sm font-semibold">Tokens by role</h2>
          <span class="text-muted-foreground text-[11px]">
            {{ provider || 'all providers' }} · {{ rows.length }}
            {{ rows.length === 1 ? 'role' : 'roles' }}
          </span>
        </div>
        <p class="text-muted-foreground mb-3 max-w-[64ch] text-[11px] leading-snug">
          Segments run cheapest to dearest, so a lighter band is a more expensive one. Input is
          never one number: cached and uncached tokens differ by roughly 50× in price, and summing
          them hides the only lever you control.
        </p>

        <!-- A legend is always present for four series; the multipliers are
             what make the ordering mean something. -->
        <div class="mb-3.5 flex flex-wrap gap-x-4 gap-y-1.5">
          <span v-for="c in CLASSES" :key="c.key" class="flex items-center gap-1.5 text-[11px]">
            <span class="size-2.5 shrink-0" :style="{ backgroundColor: c.hue }" />
            {{ c.label }}
            <span class="text-muted-foreground tabular text-[10px]">{{ c.mult }}</span>
          </span>
        </div>

        <div class="flex flex-col gap-0.5">
          <div
            v-for="r in rows"
            :key="r.role"
            :class="[
              'grid grid-cols-[7.5rem_minmax(0,1fr)_5rem] items-center gap-3 px-1.5 py-1.5',
              flagged.has(r.role)
                ? 'bg-[var(--status-warning)]/[0.07] border-l-2 border-l-[var(--status-warning)]'
                : 'hover:bg-muted/40',
            ]"
          >
            <div class="min-w-0">
              <div class="truncate text-xs font-medium">{{ r.role }}</div>
              <div class="text-muted-foreground flex items-center gap-1.5 truncate text-[10px]">
                <span
                  v-if="r.providers.length"
                  class="size-1.5 shrink-0 rounded-full"
                  :style="{ backgroundColor: providerHue.get(r.providers[0]) }"
                />
                <span class="truncate">{{ r.models.join(', ') || '—' }}</span>
              </div>
            </div>

            <!-- 2px of surface between segments rather than a border: a border
                 adds ink that reads as data. -->
            <div class="flex h-5 gap-[2px]">
              <div
                v-for="c in classesOf(r)"
                :key="c.key"
                tabindex="0"
                role="img"
                :aria-label="`${r.role} ${c.label} ${fmtTokens(c.value)} tokens, ${share(c.value, tokensOf(r))}% of the role`"
                class="focus-visible:outline-ring min-w-[2px] transition-[filter] first:rounded-l-[3px] last:rounded-r-[3px] hover:brightness-125 focus-visible:outline-2"
                :style="{
                  backgroundColor: c.hue,
                  width: `${(c.value / tokensOf(r)) * (tokensOf(r) / widest) * 100}%`,
                }"
                @mouseenter="showTip($event, r, c)"
                @focus="showTip($event, r, c)"
                @mouseleave="tip = null"
                @blur="tip = null"
              />
            </div>

            <div class="tabular text-right text-xs font-medium">
              {{ money(r.costUsd) }}
              <span
                v-if="r.subscriptionTurns"
                class="block text-[9px] font-normal text-[var(--status-warning)]"
                >est</span
              >
            </div>
          </div>
        </div>
        <!-- The regression nothing else reports. Amber and worded, never
             colour alone: this is not an error, it is a number that moved, and
             the reader has to be told which number and by how much. -->
        <div
          v-for="f in data?.flags ?? []"
          :key="f.role"
          class="mt-3 flex gap-2.5 border border-[var(--status-warning)]/40 bg-[var(--status-warning)]/[0.07] p-3"
        >
          <span
            class="text-background mt-px grid size-4 shrink-0 place-items-center rounded-full bg-[var(--status-warning)] text-[10px] font-bold"
            aria-hidden="true"
            >!</span
          >
          <p class="text-[11px] leading-relaxed">
            <b class="font-semibold">{{ f.role }}'s cache hit rate fell to {{ Math.round(f.recent * 100) }}%</b>
            over its last {{ f.recentTurns }} turns, from
            {{ Math.round(f.trailing * 100) }}% across the {{ f.trailingTurns }} before them.
            <template v-if="f.editedAt">
              Its role was edited {{ ago(f.editedAt) }} — caching is a prefix match, so one changed
              byte in the composed system prompt invalidates everything after it, silently.
            </template>
            <template v-else>
              Caching is a prefix match over the composed system prompt, so one changed byte
              invalidates everything after it, silently.
            </template>
            Cache reads cost about a tenth of uncached input, so the same work now bills roughly
            {{ inputMultiple(f) }}× more on input.
          </p>
        </div>
      </section>

      <!-- Providers. -->
      <section v-if="(data?.providers.length ?? 0) > 1" class="bg-card hairline p-4">
        <div class="mb-3 flex flex-wrap items-baseline gap-x-3">
          <h2 class="text-sm font-semibold">Spend by provider</h2>
          <span class="text-muted-foreground text-[11px]">{{ money(totals.cost) }} in this window</span>
        </div>

        <div
          class="mb-3 flex h-6 gap-[2px]"
          role="img"
          :aria-label="
            (data?.providers ?? [])
              .map((p) => `${p.key} ${share(p.costUsd, totals.cost)} percent`)
              .join(', ')
          "
        >
          <div
            v-for="p in data?.providers ?? []"
            :key="p.key"
            class="first:rounded-l-[3px] last:rounded-r-[3px]"
            :style="{
              backgroundColor: providerHue.get(p.key),
              width: `${Math.max(share(p.costUsd, totals.cost), 1)}%`,
            }"
          />
        </div>

        <div class="flex flex-col">
          <div
            v-for="p in data?.providers ?? []"
            :key="p.key"
            class="hover:bg-muted/40 grid grid-cols-[0.75rem_1fr_auto_auto] items-center gap-3 px-1.5 py-1.5 text-xs"
          >
            <span class="size-2.5" :style="{ backgroundColor: providerHue.get(p.key) }" />
            <span class="truncate font-medium">{{ p.key || 'unnamed' }}</span>
            <span class="text-muted-foreground tabular text-[11px]">
              {{ fmtTokens(p.inputTokens + p.cacheReadTokens + p.cacheWriteTokens + p.outputTokens) }}
              tokens ·
              {{ p.subscriptionTurns === p.turns ? 'subscription' : 'metered' }}
            </span>
            <span class="tabular w-20 text-right">{{ money(p.costUsd) }}</span>
          </div>
        </div>
      </section>

      <!-- The table twin: every charted value, readable without colour. -->
      <section class="bg-card hairline p-4">
        <div class="mb-3 flex flex-wrap items-baseline gap-x-3">
          <h2 class="text-sm font-semibold">Per role</h2>
          <span class="text-muted-foreground text-[11px]">
            every charted value, readable without colour
          </span>
        </div>
        <div class="overflow-x-auto">
          <table class="w-full min-w-[42rem] text-[11px]">
            <thead>
              <tr class="text-muted-foreground hairline-b text-[10px] tracking-wide uppercase">
                <th class="py-1.5 pr-3 text-left font-bold">Role</th>
                <th class="py-1.5 pr-3 text-left font-bold">Model</th>
                <th class="py-1.5 pr-3 text-right font-bold">Cache read</th>
                <th class="py-1.5 pr-3 text-right font-bold">Input</th>
                <th class="py-1.5 pr-3 text-right font-bold">Cache write</th>
                <th class="py-1.5 pr-3 text-right font-bold">Output</th>
                <th class="py-1.5 pr-3 text-right font-bold">Hit rate</th>
                <th class="py-1.5 text-right font-bold">Cost</th>
              </tr>
            </thead>
            <tbody class="tabular">
              <tr v-for="r in rows" :key="r.role" class="hover:bg-muted/40 hairline-b">
                <td class="py-1.5 pr-3 font-medium">{{ r.role }}</td>
                <td class="text-muted-foreground max-w-[12rem] truncate py-1.5 pr-3">
                  {{ r.models.join(', ') || '—' }}
                </td>
                <td class="py-1.5 pr-3 text-right">{{ fmtTokens(r.cacheReadTokens) }}</td>
                <td class="py-1.5 pr-3 text-right">{{ fmtTokens(r.inputTokens) }}</td>
                <td class="py-1.5 pr-3 text-right">{{ fmtTokens(r.cacheWriteTokens) }}</td>
                <td class="py-1.5 pr-3 text-right">{{ fmtTokens(r.outputTokens) }}</td>
                <!-- A dot and a number, never colour alone. -->
                <td class="py-1.5 pr-3 text-right">
                  <span
                    class="inline-flex items-center gap-1"
                    :class="hitRate(r) < 50 ? 'text-[var(--status-warning)]' : 'text-[var(--status-good)]'"
                  >
                    <span class="size-1.5 rounded-full bg-current" />
                    {{ hitRate(r) }}%
                  </span>
                </td>
                <td class="py-1.5 text-right">
                  {{ money(r.costUsd) }}
                  <span v-if="r.subscriptionTurns" class="text-muted-foreground">est</span>
                </td>
              </tr>
            </tbody>
            <tfoot>
              <tr class="font-semibold">
                <td class="py-2 pr-3">Total</td>
                <td class="text-muted-foreground py-2 pr-3 font-normal">
                  {{ rows.length }} {{ rows.length === 1 ? 'role' : 'roles' }}
                </td>
                <td class="tabular py-2 pr-3 text-right">{{ fmtTokens(totals.cacheReadTokens) }}</td>
                <td class="tabular py-2 pr-3 text-right">{{ fmtTokens(totals.inputTokens) }}</td>
                <td class="tabular py-2 pr-3 text-right">{{ fmtTokens(totals.cacheWriteTokens) }}</td>
                <td class="tabular py-2 pr-3 text-right">{{ fmtTokens(totals.outputTokens) }}</td>
                <td class="tabular py-2 pr-3 text-right">{{ hitRate(totals) }}%</td>
                <td class="tabular py-2 text-right">{{ money(totals.cost) }}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      </section>

      <p class="text-muted-foreground max-w-[70ch] text-[11px] leading-snug">
        Tokens are always real; dollars sometimes are not. A role on a subscription is billed by
        plan, not per token, so its figure is what those tokens would have cost at API rates —
        useful for comparing roles against each other, useless as an invoice.
        <template v-if="totals.unpriced">
          {{ totals.unpriced }} of {{ totals.turns }} turns reported no cost at all; their tokens are
          counted and their price is not.
        </template>
      </p>
    </template>

    <p v-else-if="data" class="text-muted-foreground text-xs">
      Nothing spent in this window.
      <template v-if="range === 'session'">Try a wider range.</template>
    </p>

    <!-- Raised by hover and by focus, so the reading is not mouse-only. -->
    <div
      v-if="tip"
      class="bg-popover pointer-events-none fixed z-50 -translate-x-1/2 -translate-y-full border px-2.5 py-1.5 text-[11px] shadow-md"
      :style="{ left: `${tip.x}px`, top: `${tip.y}px` }"
      role="status"
      aria-live="polite"
    >
      <span class="text-muted-foreground">{{ tip.role }} · </span>{{ tip.label }}
      <span class="text-muted-foreground">{{ tip.mult }}</span>
      <br />
      <span class="tabular">{{ fmtTokens(tip.value) }}</span>
      <span class="text-muted-foreground"> tokens · {{ tip.pct }}% of role</span>
    </div>
  </div>
</template>

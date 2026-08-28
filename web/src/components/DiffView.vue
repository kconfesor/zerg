<script setup lang="ts">
/**
 * A unified diff, coloured.
 *
 * Rendered line by line rather than dumped into a <pre>, because at the final
 * gate this is the thing being decided and a wall of monospace with leading
 * plus and minus signs is a thing people skim rather than read. Additions and
 * removals get a ground; hunk headers get a rule; everything else stays quiet.
 *
 * Line numbers are tracked from the hunk headers, so a finding can be pointed
 * at rather than described — "the third line of the second block" is not a
 * useful thing to say to anyone.
 */
import { computed } from 'vue'

const props = defineProps<{
  diff: string
  /** Lines that already carry a review thread, marked so a reader can see
   *  where the conversation is without opening anything. */
  discussed?: number[]
}>()

/**
 * A line is where a remark points.
 *
 * The gutter is the target rather than the whole row: selecting the code to
 * copy it is the other thing people do here, and a row that opens a comment box
 * on every click fights that.
 */
const emit = defineEmits<{ comment: [line: number] }>()

const marked = computed(() => new Set(props.discussed ?? []))

/** The line a remark on this row would point at: the version that still exists
 *  after the change, falling back to the one it replaced. */
function anchor(r: Row): number {
  return r.newNo ?? r.oldNo ?? 0
}

interface Row {
  kind: 'add' | 'del' | 'ctx' | 'hunk' | 'meta'
  text: string
  oldNo?: number
  newNo?: number
}

const rows = computed<Row[]>(() => {
  const out: Row[] = []
  let oldNo = 0
  let newNo = 0

  for (const line of (props.diff ?? '').split('\n')) {
    if (line.startsWith('@@')) {
      // @@ -12,7 +12,9 @@ — the two starting line numbers.
      const m = line.match(/@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
      oldNo = m ? Number(m[1]) : 0
      newNo = m ? Number(m[2]) : 0
      out.push({ kind: 'hunk', text: line })
      continue
    }
    // The file headers repeat what the caller already shows above the diff.
    if (/^(diff --git|index |--- |\+\+\+ |new file|deleted file|similarity|rename )/.test(line)) {
      out.push({ kind: 'meta', text: line })
      continue
    }
    if (line.startsWith('+')) {
      out.push({ kind: 'add', text: line.slice(1), newNo: newNo++ })
    } else if (line.startsWith('-')) {
      out.push({ kind: 'del', text: line.slice(1), oldNo: oldNo++ })
    } else {
      out.push({ kind: 'ctx', text: line.slice(1), oldNo: oldNo++, newNo: newNo++ })
    }
  }
  return out
})

/** Added and removed counts, which is the shape of a change at a glance. */
const stat = computed(() => ({
  added: rows.value.filter((r) => r.kind === 'add').length,
  removed: rows.value.filter((r) => r.kind === 'del').length,
}))
</script>

<template>
  <div class="font-mono text-[10px] leading-relaxed">
    <p class="text-muted-foreground mb-1 px-2 tabular-nums">
      <span class="text-[var(--status-good)]">+{{ stat.added }}</span>
      <span class="text-destructive ml-1.5">−{{ stat.removed }}</span>
    </p>

    <div class="overflow-x-auto">
      <div
        v-for="(r, i) in rows"
        :key="i"
        :class="[
          'flex gap-2 px-2 whitespace-pre',
          r.kind === 'add' && 'bg-[var(--status-good)]/10',
          r.kind === 'del' && 'bg-destructive/10',
          r.kind === 'hunk' && 'text-muted-foreground bg-muted/60 my-1',
          r.kind === 'meta' && 'text-muted-foreground/60',
        ]"
      >
        <!-- Both sides, so a line can be cited by number in either version.
             The new-side number is also the button that starts a thread there:
             the gutter rather than the row, so selecting code to copy it still
             works. -->
        <span class="text-muted-foreground/50 w-8 shrink-0 text-right tabular-nums select-none">
          {{ r.oldNo ?? '' }}
        </span>
        <button
          v-if="anchor(r) && r.kind !== 'meta' && r.kind !== 'hunk'"
          type="button"
          :class="[
            'w-8 shrink-0 text-right tabular-nums select-none',
            marked.has(anchor(r))
              ? 'text-[var(--primary)] font-semibold'
              : 'text-muted-foreground/50 hover:text-foreground',
          ]"
          :title="marked.has(anchor(r)) ? 'This line is being discussed' : 'Comment on this line'"
          :aria-label="`Comment on line ${anchor(r)}`"
          @click="emit('comment', anchor(r))"
        >
          {{ r.newNo ?? r.oldNo }}
        </button>
        <span v-else class="text-muted-foreground/50 w-8 shrink-0 text-right tabular-nums select-none">
          {{ r.newNo ?? '' }}
        </span>
        <span
          class="w-2 shrink-0 select-none"
          :class="r.kind === 'add' ? 'text-[var(--status-good)]' : 'text-destructive'"
        >
          {{ r.kind === 'add' ? '+' : r.kind === 'del' ? '−' : '' }}
        </span>
        <span class="min-w-0">{{ r.text }}</span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
/**
 * The CLI's own mark: claude's coral logomark, pi's P, or an initial for
 * anything else this hasn't been taught to draw yet.
 *
 * One definition, since the board and the task detail header both need to
 * say which harness did the work and neither should carry its own copy of
 * the path data.
 */
import { computed } from 'vue'

const props = withDefaults(defineProps<{
  harness: string
  /** Overrides the default "Runs on {harness}" wording. */
  label?: string
  /** The same pulse the working badge's dot wears, for the harness actually
   *  spending tokens right now — not a second animation for the same fact. */
  pulse?: boolean
  /** Box size in px. Default matches the board card's icon rail; the detail
   *  header asks for a bigger one so it reads as level with the badges next
   *  to it rather than a stray small square between two pills. */
  size?: number
}>(), { label: undefined, size: 16 })

/** The glyph inside scales with the box, at the ratio the marks were drawn to. */
const glyph = computed(() => Math.round(props.size * 0.75))
</script>

<template>
  <span
    role="img"
    :aria-label="label ?? `Runs on ${harness}`"
    :title="label ?? `Runs on ${harness}`"
    :style="{ width: `${size}px`, height: `${size}px` }"
    :class="['grid shrink-0 place-items-center', pulse && 'pulse-dot rounded-full']"
  >
    <!-- pi's mark is a single shape with no brand colour of its own, so it
         takes the icon rail's muted grey like every other glyph on the card.
         Claude's keeps its own coral -- the only mono fill it ships with. -->
    <svg
      v-if="harness === 'pi'"
      viewBox="0 0 800 800"
      :width="glyph"
      :height="glyph"
      aria-hidden="true"
      class="text-muted-foreground"
    >
      <path
        fill="currentColor"
        fill-rule="evenodd"
        d="M165.29 165.29 H517.36 V400 H400 V517.36 H282.65 V634.72 H165.29 Z M282.65 282.65 V400 H400 V282.65 Z"
      />
      <path fill="currentColor" d="M517.36 400 H634.72 V634.72 H517.36 Z" />
    </svg>
    <svg v-else-if="harness === 'claude'" viewBox="0 0 24 24" :width="glyph" :height="glyph" aria-hidden="true">
      <path
        fill="#D97757"
        fill-rule="evenodd"
        d="M20.998 10.949H24v3.102h-3v3.028h-1.487V20H18v-2.921h-1.487V20H15v-2.921H9V20H7.488v-2.921H6V20H4.487v-2.921H3V14.05H0V10.95h3V5h17.998v5.949zM6 10.949h1.488V8.102H6v2.847zm10.51 0H18V8.102h-1.49v2.847z"
      />
    </svg>
    <!-- A harness added later and not drawn here yet, not silently claude's
         coral: its own first letter, until it earns a real mark. -->
    <span
      v-else
      class="text-muted-foreground leading-none font-semibold uppercase"
      :style="{ fontSize: `${Math.round(glyph * 0.75)}px` }"
    >
      {{ harness.slice(0, 1) }}
    </span>
  </span>
</template>

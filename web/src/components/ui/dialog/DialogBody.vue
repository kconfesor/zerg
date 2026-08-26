<script setup lang="ts">
/**
 * The scrolling part of a dialog, and the only part that scrolls.
 *
 * A tall dialog's obvious shape is overflow-y-auto on the panel itself, and it
 * is wrong in three ways at once: the title scrolls out of view, so a long
 * transcript loses the name of the card it belongs to; the panel's padding
 * scrolls with it, so text runs into the top edge as soon as you move; and the
 * close button, absolutely positioned against the panel, travels with the
 * scroll.
 *
 * Splitting it fixes all three. The panel becomes a flex column that clips, the
 * header stays put, and this is the piece that moves. min-h-0 is what makes it
 * work at all — a flex child's default min-height is its content, so without it
 * the body refuses to shrink and pushes the panel past the viewport instead of
 * scrolling inside it.
 */
import type { HTMLAttributes } from 'vue'
import { cn } from '@/lib/utils'

const props = defineProps<{ class?: HTMLAttributes['class'] }>()
</script>

<template>
  <div
    data-slot="dialog-body"
    :class="cn('min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-5 py-4', props.class)"
  >
    <slot />
  </div>
</template>

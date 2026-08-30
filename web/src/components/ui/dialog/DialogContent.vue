<script setup lang="ts">
import type { DialogContentEmits, DialogContentProps } from 'reka-ui'

import type { HTMLAttributes } from 'vue'
import { XIcon } from '@lucide/vue'
import { reactiveOmit } from '@vueuse/core'
import {
  DialogClose,
  DialogContent,
  DialogPortal,
  useForwardPropsEmits,
} from 'reka-ui'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import DialogOverlay from './DialogOverlay.vue'

defineOptions({
  inheritAttrs: false,
})

/**
 * `variant`:
 *   sheet   — the default. Full width on a phone, a centred panel from sm up.
 *             Right for anything with content in it: a diff, a task, a form.
 *   confirm — a centred card at every width. A question with two buttons under
 *             it does not need a screen, and taking one makes an "are you
 *             sure?" look like a place you have navigated to rather than a
 *             thing you can dismiss.
 */
const props = withDefaults(defineProps<DialogContentProps & {
  class?: HTMLAttributes['class']
  showCloseButton?: boolean
  variant?: 'sheet' | 'confirm'
}>(), {
  showCloseButton: true,
  variant: 'sheet',
})
const emits = defineEmits<DialogContentEmits>()

const delegatedProps = reactiveOmit(props, 'class')

const forwarded = useForwardPropsEmits(delegatedProps, emits)
</script>

<template>
  <DialogPortal>
    <DialogOverlay />
    <DialogContent
      data-slot="dialog-content"
      v-bind="{ ...$attrs, ...forwarded }"
      :class="cn(
        // A sheet on a phone, a panel on a desktop.
        //
        // Mobile first, and literally full width: a centred box inset by a
        // margin wastes the two dimensions a phone has least of, and a
        // transcript that has to scroll inside a 480px window inside a 560px
        // screen is the same problem twice. Spanning the safe-area insets
        // rather than the raw viewport keeps the close button out from under a
        // notch, and leaves the status bar reading as overlay rather than as
        // part of the dialog — which is also why the insets are positions and
        // not padding: padding here would fight whatever the call site sets,
        // and the split dialogs deliberately set p-0.
        'bg-popover text-popover-foreground data-open:animate-in data-closed:animate-out data-closed:fade-out-0 data-open:fade-in-0 data-closed:zoom-out-95 data-open:zoom-in-95 ring-foreground/10 fixed z-50 flex flex-col gap-4 p-5 text-xs/relaxed ring-1 duration-100 outline-none',
        // A sheet on a phone: full width, spanning the safe-area insets.
        variant === 'sheet' &&
          'inset-x-0 top-[env(safe-area-inset-top)] bottom-[env(safe-area-inset-bottom)] max-w-none rounded-none',
        // A confirmation is a card at every width, inset from the edges so it
        // reads as something over the page rather than a page of its own.
        // No corner radius: this app is square by design (--radius: 0), and
        // asking for one here only looks like an intention that never renders.
        variant === 'confirm' && 'inset-x-4 top-1/2 max-h-[85vh] -translate-y-1/2',
        // From sm up both are a centred panel, bounded so a long one scrolls
        // inside itself rather than running off the screen.
        'sm:inset-x-auto sm:top-1/2 sm:bottom-auto sm:left-1/2 sm:h-auto sm:max-h-[85vh] sm:w-full sm:max-w-sm sm:-translate-x-1/2 sm:-translate-y-1/2',
        props.class,
      )"
    >
      <slot />

      <DialogClose
        v-if="showCloseButton"
        data-slot="dialog-close"
        as-child
      >
        <Button variant="ghost" class="absolute top-2 right-2" size="icon-sm">
          <XIcon />
          <span class="sr-only">Close</span>
        </Button>
      </DialogClose>
    </DialogContent>
  </DialogPortal>
</template>

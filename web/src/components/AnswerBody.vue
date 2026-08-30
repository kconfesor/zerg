<script setup lang="ts">
/**
 * An agent's answer, with a copy control on every code block.
 *
 * The renderer produces HTML, so the buttons are added to it afterwards rather
 * than woven into the Markdown: the alternative is a second parser whose idea
 * of a fenced block has to agree with the first one's, and they would disagree
 * eventually. Added on mount and after each change, because a streamed answer
 * grows blocks as it is written.
 *
 * The button is a real element rather than a click handler on the container:
 * a code block is often scrolled sideways, and a target that moves with the
 * content is one people miss.
 */
import { onMounted, ref, watch } from 'vue'
import { renderMarkdown } from '@/lib/markdown'

const props = defineProps<{ text: string }>()
const host = ref<HTMLElement | null>(null)

/** How long a button says "copied" before going back. */
const SAID = 1200

function decorate() {
  const el = host.value
  if (!el) return
  for (const block of Array.from(el.querySelectorAll('pre'))) {
    if (block.querySelector('[data-copy]')) continue
    // The button is positioned against the block, so the block has to be the
    // positioning context. Set here rather than in CSS because the markup
    // comes from the renderer.
    block.style.position = 'relative'

    const button = document.createElement('button')
    button.type = 'button'
    button.dataset.copy = 'true'
    button.textContent = 'copy'
    button.setAttribute('aria-label', 'Copy this code')
    button.className =
      'absolute top-1 right-1 border bg-[var(--card)] px-1.5 py-0.5 text-[10px] ' +
      'text-muted-foreground hover:text-foreground focus-visible:outline-ring focus-visible:outline-2'
    button.addEventListener('click', async () => {
      const code = block.querySelector('code')?.textContent ?? block.textContent ?? ''
      try {
        await navigator.clipboard.writeText(code)
        button.textContent = 'copied'
      } catch {
        // Refused, which is ordinary on an insecure origin. Say so rather
        // than leaving the button looking as though it worked.
        button.textContent = 'cannot copy'
      }
      window.setTimeout(() => {
        button.textContent = 'copy'
      }, SAID)
    })
    block.appendChild(button)
  }
}

onMounted(decorate)
watch(() => props.text, () => queueMicrotask(decorate))
</script>

<template>
  <div ref="host" class="md leading-relaxed" v-html="renderMarkdown(props.text)" />
</template>

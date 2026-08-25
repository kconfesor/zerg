<script setup lang="ts">
/**
 * A Markdown editor with a formatting toolbar.
 *
 * Markdown rather than a WYSIWYG, because of where this text goes: straight to
 * an agent, as text. A rich-text editor would produce HTML, and the agent would
 * have to read past the tags to find the brief. Markdown is what these models
 * read natively — and it is what the reviewer's output already is, so the
 * whole system speaks one format.
 *
 * That also means no editor dependency: a textarea, a toolbar that inserts the
 * marks, and the renderer already used to display agent messages.
 */
import { computed, ref } from 'vue'
import { Bold, Code, Italic, List, ListOrdered } from '@lucide/vue'
import { renderMarkdown } from '@/lib/markdown'
import { Textarea } from '@/components/ui/textarea'

const model = defineModel<string>({ required: true })
const props = withDefaults(defineProps<{ rows?: number }>(), { rows: 8 })

const area = ref<InstanceType<typeof Textarea> | null>(null)
const tab = ref<'write' | 'preview'>('write')

function el(): HTMLTextAreaElement | null {
  // shadcn's Textarea wraps the element, so reach the real one to touch the
  // selection.
  const root = (area.value as unknown as { $el?: HTMLElement })?.$el
  return (root instanceof HTMLTextAreaElement ? root : root?.querySelector('textarea')) ?? null
}

/**
 * Wrap the selection, or insert the marks and put the caret between them.
 *
 * Selection is restored afterwards: an editor that formats your text and then
 * dumps the caret at the end makes you find your place again on every click.
 */
function wrap(mark: string) {
  const t = el()
  if (!t) return
  const { selectionStart: a, selectionEnd: b } = t
  const chosen = model.value.slice(a, b)
  model.value = model.value.slice(0, a) + mark + chosen + mark + model.value.slice(b)
  requestAnimationFrame(() => {
    t.focus()
    t.setSelectionRange(a + mark.length, a + mark.length + chosen.length)
  })
}

/** Prefix every selected line, which is what a list button has to do — one
 *  marker on the first line of a five-line selection is not a list. */
function prefixLines(make: (i: number) => string) {
  const t = el()
  if (!t) return
  const { selectionStart: a, selectionEnd: b } = t

  const start = model.value.lastIndexOf('\n', a - 1) + 1
  const end = model.value.indexOf('\n', b)
  const stop = end === -1 ? model.value.length : end

  const block = model.value.slice(start, stop) || ''
  const marked = block
    .split('\n')
    .map((line, i) => (line.trim() === '' ? line : make(i) + line))
    .join('\n')

  model.value = model.value.slice(0, start) + marked + model.value.slice(stop)
  requestAnimationFrame(() => {
    t.focus()
    t.setSelectionRange(start, start + marked.length)
  })
}

const tools = [
  { icon: Bold, label: 'Bold', run: () => wrap('**') },
  { icon: Italic, label: 'Italic', run: () => wrap('*') },
  { icon: Code, label: 'Code', run: () => wrap('`') },
  { icon: List, label: 'Bullet list', run: () => prefixLines(() => '- ') },
  { icon: ListOrdered, label: 'Numbered list', run: () => prefixLines((i) => `${i + 1}. `) },
]

/** Cmd/Ctrl shortcuts for the two people reach for without looking. */
function onKeydown(ev: KeyboardEvent) {
  if (!(ev.metaKey || ev.ctrlKey)) return
  const k = ev.key.toLowerCase()
  if (k === 'b') {
    ev.preventDefault()
    wrap('**')
  } else if (k === 'i') {
    ev.preventDefault()
    wrap('*')
  }
}

const empty = computed(() => model.value.trim() === '')
</script>

<template>
  <div class="flex flex-col">
    <div class="hairline-b flex items-center gap-0.5 px-1 py-1">
      <button
        v-for="t in tools"
        :key="t.label"
        type="button"
        :title="t.label"
        :aria-label="t.label"
        :disabled="tab === 'preview'"
        class="text-muted-foreground hover:bg-muted hover:text-foreground focus-visible:outline-ring grid size-7 place-items-center transition-colors focus-visible:outline-2 disabled:opacity-40"
        @click="t.run"
      >
        <component :is="t.icon" :size="14" aria-hidden="true" />
      </button>

      <div class="ml-auto flex items-center gap-0.5 text-[11px]">
        <button
          v-for="t in (['write', 'preview'] as const)"
          :key="t"
          type="button"
          :class="[
            'px-2 py-1 transition-colors',
            tab === t ? 'text-foreground font-semibold' : 'text-muted-foreground hover:text-foreground',
          ]"
          @click="tab = t"
        >
          {{ t }}
        </button>
      </div>
    </div>

    <Textarea
      v-show="tab === 'write'"
      ref="area"
      v-model="model"
      :rows="rows"
      class="rounded-none border-0 text-xs focus-visible:ring-0"
      @keydown="onKeydown"
    />

    <div
      v-show="tab === 'preview'"
      class="md min-h-32 px-3 py-2 text-xs leading-relaxed"
      :class="empty ? 'text-muted-foreground' : ''"
    >
      <template v-if="empty">Nothing to preview yet.</template>
      <div v-else v-html="renderMarkdown(model)" />
    </div>
  </div>
</template>

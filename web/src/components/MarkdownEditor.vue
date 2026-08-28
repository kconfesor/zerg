<script setup lang="ts">
/**
 * The brief editor: rich text on screen, Markdown in the database.
 *
 * What it replaced was a textarea with five buttons that inserted literal
 * asterisks. It failed at being an editor in ways that were not about missing
 * features: writing to the model directly meant the browser's undo stack never
 * saw a toolbar edit, so Cmd-Z could not undo a bold; pressing Bold twice wrote
 * `******like this******` rather than turning it off; Enter did not continue a
 * list and Tab left the field.
 *
 * TipTap is a ProseMirror document with a schema, so those are all properties
 * of the model rather than features to add: a mark toggles, history is real,
 * lists behave like lists.
 *
 * **The stored format is still Markdown**, and that is not negotiable — this
 * text goes to an agent as text, and Markdown is what the models read. So the
 * document is parsed from Markdown on the way in and serialised back on every
 * change. A rich-text editor that stored HTML would send the agent tags to read
 * past.
 *
 * Two consequences worth knowing:
 *
 *   - The round trip is lossy for anything the schema does not model, so the
 *     **Source** tab is not a nicety. It is the escape hatch: see exactly what
 *     will be sent, and edit it as text when the editor gets in the way.
 *   - `html: false` on the serialiser. Raw HTML in a brief stays literal text
 *     rather than becoming nodes, which keeps this consistent with the renderer
 *     used for agent output, where escaping first is a security property.
 *
 * `tiptap-markdown` is a thin wrapper over prosemirror-markdown and its author
 * has said he is not maintaining it further (TipTap's own Markdown conversion
 * is a paid extension). It is MIT and small; if it ever breaks against a TipTap
 * release, the replacement is prosemirror-markdown directly, which is what it
 * already delegates to.
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { EditorContent, useEditor } from '@tiptap/vue-3'
import StarterKit from '@tiptap/starter-kit'
import { CharacterCount, Placeholder } from '@tiptap/extensions'
import { Markdown } from 'tiptap-markdown'
import {
  Bold,
  Code,
  Heading1,
  Heading2,
  Heading3,
  Italic,
  Link2,
  List,
  ListOrdered,
  Minus,
  Quote,
  Redo2,
  SquareCode,
  Strikethrough,
  Undo2,
} from '@lucide/vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

const model = defineModel<string>({ required: true })
const props = withDefaults(
  defineProps<{ rows?: number; id?: string; placeholder?: string }>(),
  { rows: 8, id: undefined, placeholder: 'Write the brief…' },
)

const tab = ref<'write' | 'source'>('write')

/** Roughly the height the old textarea had, so dialogs keep their proportions. */
const minHeight = computed(() => `${Math.max(props.rows, 3) * 1.5 + 1}rem`)

const editor = useEditor({
  content: model.value,
  extensions: [
    StarterKit.configure({
      // The brief is a brief. Deeper headings than these are a document
      // structure nobody is going to read on a card.
      heading: { levels: [1, 2, 3] },
      link: { openOnClick: false, autolink: true },
    }),
    Markdown.configure({
      html: false,
      linkify: true,
      breaks: false,
      // Pasting Markdown into a rich-text editor and getting literal asterisks
      // is the thing people notice first, and everything upstream of this box —
      // an agent message, a spec file, another card — is Markdown.
      transformPastedText: true,
      transformCopiedText: true,
    }),
    Placeholder.configure({ placeholder: () => props.placeholder }),
    CharacterCount,
  ],
  editorProps: {
    attributes: {
      class: 'wysiwyg focus:outline-none',
      ...(props.id ? { id: props.id } : {}),
    },
  },
  onUpdate: ({ editor }) => {
    // Serialise on every change rather than on save: the model is the Markdown,
    // so anything reading it mid-edit — a dirty check, a draft — sees the truth.
    model.value = editor.storage.markdown.getMarkdown()
  },
})

// Only when the change came from outside. Without the comparison, serialising
// on update and re-parsing on watch is a loop that fights the caret on every
// keystroke.
watch(model, (v) => {
  const ed = editor.value
  if (!ed || tab.value === 'source') return
  if (v === ed.storage.markdown.getMarkdown()) return
  ed.commands.setContent(v, { emitUpdate: false })
})

// Leaving the source tab re-parses what was typed there.
watch(tab, (t) => {
  const ed = editor.value
  if (t !== 'write' || !ed) return
  if (model.value !== ed.storage.markdown.getMarkdown()) {
    ed.commands.setContent(model.value, { emitUpdate: false })
  }
})

onBeforeUnmount(() => editor.value?.destroy())

const words = computed(() => editor.value?.storage.characterCount.words() ?? 0)

/** A link needs a URL, and a URL needs somewhere to type it. */
const linking = ref(false)
const linkUrl = ref('')
const linkText = ref('')

function openLink() {
  const ed = editor.value
  if (!ed) return
  const { from, to } = ed.state.selection
  linkText.value = ed.state.doc.textBetween(from, to, ' ')
  linkUrl.value = ed.getAttributes('link').href ?? ''
  linking.value = true
}

function applyLink() {
  const ed = editor.value
  if (!ed) return
  const href = linkUrl.value.trim()
  if (!href) {
    ed.chain().focus().extendMarkRange('link').unsetLink().run()
  } else if (ed.state.selection.empty) {
    // No selection: insert the text as the link, falling back to the URL
    // itself, which is what every other editor does and what a paste expects.
    const text = linkText.value.trim() || href
    ed.chain().focus().insertContent({ type: 'text', text, marks: [{ type: 'link', attrs: { href } }] }).run()
  } else {
    ed.chain().focus().extendMarkRange('link').setLink({ href }).run()
  }
  linking.value = false
}

type Tool = {
  icon: unknown
  label: string
  run: () => void
  active?: () => boolean
  disabled?: () => boolean
  group?: boolean
}

/**
 * The marks worth a button, in the order a brief uses them.
 *
 * Everything here is also a keystroke that TipTap already binds — Cmd-B,
 * Cmd-I, `## ` at the start of a line, `- ` for a list. The toolbar is for
 * people who do not know that, and the shortcuts are for people who do.
 */
const tools = computed<Tool[]>(() => {
  const ed = editor.value
  const chain = () => ed!.chain().focus()
  return [
    { icon: Heading1, label: 'Heading 1', run: () => chain().toggleHeading({ level: 1 }).run(), active: () => !!ed?.isActive('heading', { level: 1 }) },
    { icon: Heading2, label: 'Heading 2', run: () => chain().toggleHeading({ level: 2 }).run(), active: () => !!ed?.isActive('heading', { level: 2 }) },
    { icon: Heading3, label: 'Heading 3', run: () => chain().toggleHeading({ level: 3 }).run(), active: () => !!ed?.isActive('heading', { level: 3 }), group: true },

    { icon: Bold, label: 'Bold', run: () => chain().toggleBold().run(), active: () => !!ed?.isActive('bold') },
    { icon: Italic, label: 'Italic', run: () => chain().toggleItalic().run(), active: () => !!ed?.isActive('italic') },
    { icon: Strikethrough, label: 'Strikethrough', run: () => chain().toggleStrike().run(), active: () => !!ed?.isActive('strike') },
    { icon: Code, label: 'Inline code', run: () => chain().toggleCode().run(), active: () => !!ed?.isActive('code') },
    { icon: Link2, label: 'Link', run: openLink, active: () => !!ed?.isActive('link'), group: true },

    { icon: List, label: 'Bullet list', run: () => chain().toggleBulletList().run(), active: () => !!ed?.isActive('bulletList') },
    { icon: ListOrdered, label: 'Numbered list', run: () => chain().toggleOrderedList().run(), active: () => !!ed?.isActive('orderedList') },
    { icon: Quote, label: 'Quote', run: () => chain().toggleBlockquote().run(), active: () => !!ed?.isActive('blockquote') },
    { icon: SquareCode, label: 'Code block', run: () => chain().toggleCodeBlock().run(), active: () => !!ed?.isActive('codeBlock') },
    { icon: Minus, label: 'Divider', run: () => chain().setHorizontalRule().run(), group: true },

    { icon: Undo2, label: 'Undo', run: () => chain().undo().run(), disabled: () => !ed?.can().undo() },
    { icon: Redo2, label: 'Redo', run: () => chain().redo().run(), disabled: () => !ed?.can().redo() },
  ]
})
</script>

<template>
  <div class="flex min-h-0 w-full min-w-0 flex-1 flex-col">
    <div class="hairline-b flex flex-wrap items-center gap-0.5 px-1 py-1">
      <template v-for="t in tools" :key="t.label">
        <button
          type="button"
          :title="t.label"
          :aria-label="t.label"
          :aria-pressed="t.active ? t.active() : undefined"
          :disabled="tab === 'source' || !editor || (t.disabled ? t.disabled() : false)"
          :class="[
            'focus-visible:outline-ring grid size-7 place-items-center transition-colors focus-visible:outline-2 disabled:opacity-40',
            t.active?.()
              ? 'bg-primary/[0.14] text-foreground'
              : 'text-muted-foreground hover:bg-muted hover:text-foreground',
          ]"
          @click="t.run"
        >
          <component :is="t.icon" :size="14" aria-hidden="true" />
        </button>
        <span v-if="t.group" class="bg-border mx-1 h-4 w-px shrink-0" aria-hidden="true" />
      </template>

      <div class="ml-auto flex items-center gap-2 pl-1 text-[11px]">
        <span v-if="tab === 'write'" class="text-muted-foreground tabular hidden sm:inline">
          {{ words }} word{{ words === 1 ? '' : 's' }}
        </span>
        <!-- What will actually be sent. The document is Markdown underneath, so
             this is the same text, not an export of it. -->
        <button
          v-for="t in (['write', 'source'] as const)"
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

    <!-- Scrolls itself rather than the dialog: the toolbar has to stay put
         while a long brief moves under it. -->
    <EditorContent
      v-show="tab === 'write'"
      :editor="editor"
      class="min-h-0 w-full min-w-0 flex-1 overflow-y-auto px-3 py-2 text-xs leading-relaxed"
      :style="{ minHeight }"
    />

    <Textarea
      v-show="tab === 'source'"
      v-model="model"
      :rows="rows"
      class="min-h-0 flex-1 resize-none rounded-none border-0 font-mono text-xs focus-visible:ring-0"
    />

    <Dialog v-model:open="linking">
      <DialogContent class="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Link</DialogTitle>
          <DialogDescription>
            Leave the address empty to remove the link from the selected text.
          </DialogDescription>
        </DialogHeader>
        <div class="flex flex-col gap-3 px-1">
          <Input
            v-if="editor?.state.selection.empty"
            v-model="linkText"
            placeholder="Text to show"
            @keydown.enter.prevent="applyLink"
          />
          <Input
            v-model="linkUrl"
            placeholder="https://…"
            autofocus
            @keydown.enter.prevent="applyLink"
          />
        </div>
        <DialogFooter>
          <Button variant="outline" @click="linking = false">Cancel</Button>
          <Button @click="applyLink">Apply</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>

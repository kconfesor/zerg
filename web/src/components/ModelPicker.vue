<script setup lang="ts">
/**
 * A model field: a text box that suggests, rather than a list that constrains.
 *
 * Typed, not picked. pi reports dozens of models and scrolling a dropdown to
 * find one is slower than typing three characters of it — and what is typed is
 * what is used, so a working model a catalog has not heard of still runs. The
 * list narrows; it never limits.
 *
 * The keyboard behaviour is the reason this is a component rather than two
 * copies of a div. Both copies were a text input with a stack of buttons under
 * it: no role, no announcement that a list had appeared, nothing to say which
 * option was current, and no way to reach one without a mouse — arrow keys
 * moved the caret through the text while the list sat there ignoring them. The
 * ARIA combobox pattern is what a screen reader and a keyboard both already
 * know, so it is what this implements: arrows move the active option, Enter
 * takes it, Escape closes without changing anything.
 */
import { computed, ref, useId, watch } from 'vue'
import type { Model } from '@/lib/api'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

const props = defineProps<{
  modelValue: string
  models: Model[]
  label?: string
  placeholder?: string
  inputClass?: string
}>()
const emit = defineEmits<{
  'update:modelValue': [value: string]
  /** The field is finished with: a choice was taken, or focus left it. */
  commit: [value: string]
}>()

const inputId = useId()
const listId = useId()

const open = ref(false)
const active = ref(-1)

/** The catalog narrowed to what has been typed, matched anywhere in the id so
 *  "sol" finds "openai-codex/gpt-5.6-sol". */
const matching = computed(() => {
  const q = props.modelValue.trim().toLowerCase()
  const hits = q ? props.models.filter((m) => m.ID.toLowerCase().includes(q)) : props.models
  // Capped: a list longer than the popover can show is a list nobody reads to
  // the end of, and the filter is the way through it.
  return hits.slice(0, 50)
})

// A filter that no longer contains the active option must not leave the
// announcement pointing at a row that is not there.
watch(matching, () => {
  if (active.value >= matching.value.length) active.value = -1
})

function optionId(i: number): string {
  return `${listId}-option-${i}`
}

function show() {
  open.value = true
}

function close() {
  open.value = false
  active.value = -1
}

function choose(id: string) {
  emit('update:modelValue', id)
  close()
  emit('commit', id)
}

function onInput(value: string) {
  emit('update:modelValue', value)
  active.value = -1
  show()
}

function move(by: number) {
  if (!open.value) {
    show()
    return
  }
  const n = matching.value.length
  if (n === 0) return
  active.value = (active.value + by + n + 1) % (n + 1) // one past the end is "back to the text"
  if (active.value === n) active.value = -1
}

function onEnter(ev: KeyboardEvent) {
  if (open.value && active.value >= 0) {
    ev.preventDefault()
    choose(matching.value[active.value].ID)
    return
  }
  close()
  emit('commit', props.modelValue)
}

function onBlur() {
  close()
  emit('commit', props.modelValue)
}
</script>

<template>
  <div class="relative flex flex-col gap-1.5">
    <Label v-if="label" :for="inputId" class="text-[10px]">{{ label }}</Label>
    <Input
      :id="inputId"
      :model-value="modelValue"
      :class="inputClass"
      autocomplete="off"
      role="combobox"
      aria-autocomplete="list"
      :aria-expanded="open && matching.length > 0"
      :aria-controls="listId"
      :aria-activedescendant="active >= 0 ? optionId(active) : undefined"
      :placeholder="placeholder ?? 'harness default'"
      @focus="show"
      @update:model-value="(v) => onInput(String(v))"
      @keydown.down.prevent="move(1)"
      @keydown.up.prevent="move(-1)"
      @keydown.enter="onEnter"
      @keydown.escape="close"
      @blur="onBlur"
    />
    <div
      v-show="open && matching.length"
      :id="listId"
      role="listbox"
      class="bg-popover absolute top-full z-50 mt-1 max-h-56 w-full overflow-y-auto border shadow-md"
    >
      <div
        v-for="(m, i) in matching"
        :id="optionId(i)"
        :key="m.ID"
        role="option"
        :aria-selected="m.ID === modelValue"
        :class="[
          'flex w-full cursor-pointer items-center gap-2 px-2 py-1.5 text-left text-xs',
          i === active ? 'bg-muted' : 'hover:bg-muted',
        ]"
        @mousedown.prevent="choose(m.ID)"
        @mouseenter="active = i"
      >
        <span class="min-w-0 flex-1 truncate font-mono">{{ m.ID }}</span>
        <span v-if="m.Provider" class="text-muted-foreground shrink-0 text-[10px]">
          {{ m.Provider }}
        </span>
      </div>
    </div>
  </div>
</template>

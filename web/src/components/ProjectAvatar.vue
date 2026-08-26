<script setup lang="ts">
/**
 * A project's mark: the one out of its own repository, or one derived from what
 * it is called.
 *
 * The repository almost always has the better answer — a favicon, a logo, an
 * app icon, drawn deliberately and already what its people recognise it by. So
 * that is what this shows when one has been picked.
 *
 * The derived form is the fallback, and the interesting half. A project with no
 * mark still has to be distinguishable at a glance in a switcher you pass over
 * fifty times a day, so: initials on a colour, both taken from the project's
 * id, which never changes. Deriving from the *name* would be prettier and
 * wrong — renaming a project would move its colour, and the thing a colour is
 * for is being the same tomorrow.
 *
 * The palette is the four categorical chart colours, which the theme already
 * holds to a contrast floor against both surfaces. Foreground is white at every
 * one of them — that is what the 62% lightness in the tokens is chosen for.
 *
 * One root element, and no comment above it: a top-level comment is a second
 * root node, which costs the component its attribute fallthrough — a parent's
 * class then has nowhere unambiguous to land. It is aria-hidden, because the
 * project's name is always rendered beside it and a screen reader announcing
 * "C A, calc2" says the same thing twice.
 */
import { computed, ref, watch } from 'vue'
import { projectIconURL, type Project } from '@/lib/api'

const props = withDefaults(
  defineProps<{ project: Project | null; size?: 'sm' | 'md' | 'lg' }>(),
  { size: 'md' },
)

/** Up to two characters: the initials of the first two words, or the first two
 *  letters of a single-word name. "calc2" reads as CA, "credix-platform" as CP. */
const initials = computed(() => {
  const name = props.project?.name?.trim() ?? ''
  if (!name) return '?'
  const words = name.split(/[\s\-_./]+/).filter(Boolean)
  if (words.length >= 2) return (words[0][0] + words[1][0]).toUpperCase()
  return name.slice(0, 2).toUpperCase()
})

/** Stable across restarts and renames: the id is the only part that is. */
const hue = computed(() => {
  const id = props.project?.id ?? ''
  let sum = 0
  for (let i = 0; i < id.length; i++) sum = (sum + id.charCodeAt(i)) % 4
  return `var(--chart-${sum + 1})`
})

/**
 * A mark that has been deleted from the repository since it was picked is not
 * an error worth reporting — it is a project that looks like it has no mark,
 * which is exactly what the initials are for. The flag resets when the source
 * changes, so fixing the file in the repository fixes the avatar on reload.
 */
const broken = ref(false)
const src = computed(() => (props.project?.icon ? projectIconURL(props.project) : ''))
watch(src, () => (broken.value = false))

const showImage = computed(() => !!src.value && !broken.value)

const box = computed(
  () => ({ sm: 'size-6', md: 'size-8', lg: 'size-10' })[props.size],
)
const type = computed(
  () => ({ sm: 'text-[9px]', md: 'text-[10px]', lg: 'text-xs' })[props.size],
)
</script>

<template>
  <img
    v-if="showImage"
    :src="src"
    alt=""
    aria-hidden="true"
    :class="['shrink-0 object-contain', box]"
    @error="broken = true"
  />
  <span
    v-else
    :class="['grid shrink-0 place-items-center font-bold tracking-tight text-white', box, type]"
    :style="{ backgroundColor: hue }"
    aria-hidden="true"
    >{{ initials }}</span
  >
</template>

<script setup lang="ts">
defineProps<{ title: string; subtitle?: string }>()
</script>

<template>
  <!-- Every view opens the same way: what this is, what it is telling you, and
       the action that belongs to it. Consistency here is what makes the app
       feel like one thing rather than three screens. -->
  <header class="hairline-b flex flex-wrap items-end gap-x-4 gap-y-2 pb-3">
    <div class="min-w-0">
      <h1 class="text-[17px] leading-none font-semibold tracking-tight">{{ title }}</h1>
      <p v-if="subtitle" class="text-muted-foreground mt-1.5 text-xs">{{ subtitle }}</p>
      <!-- Context that belongs to the view rather than the app: which
           repository this is, what it has cost. It sits under the count it
           qualifies instead of in the global bar, where it read as chrome. -->
      <div class="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1"><slot name="meta" /></div>
    </div>
    <!-- Its own row once the header wraps, rather than a right-aligned island.
         On a phone the toggle and the button dropped under the meta line and
         stayed pinned to the right edge, leaving 143px of empty gutter beside
         them: it read as something that had fallen off the title rather than
         the row of controls it is. Full width and spread apart, a wrap looks
         deliberate. Above sm it is the right-aligned group it always was. -->
    <div
      class="flex w-full items-center justify-between gap-2 sm:ml-auto sm:w-auto sm:justify-end"
    >
      <slot name="actions" />
    </div>
  </header>
</template>

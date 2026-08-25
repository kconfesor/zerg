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
    <div class="ml-auto flex items-center gap-2"><slot name="actions" /></div>
  </header>
</template>

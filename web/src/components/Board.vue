<script setup lang="ts">
import { computed } from 'vue'
import type { ResolvedRole, Task } from '@/lib/api'
import { duration } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'

const props = defineProps<{ team: ResolvedRole[]; tasks: Task[] }>()
const emit = defineEmits<{ open: [task: Task] }>()

/** Lanes are the enabled roles in pipeline order, then the Done well. */
const lanes = computed(() => {
  const roles = props.team.filter((r) => r.enabled).map((r) => r.name)
  return [...roles, 'done']
})

const byLane = computed(() => {
  const map = new Map<string, Task[]>()
  for (const lane of lanes.value) map.set(lane, [])
  for (const task of props.tasks) map.get(task.lane)?.push(task)
  return map
})
</script>

<template>
  <!-- Lanes share the width when there are few and scroll sideways when there
       are many. The page itself never scrolls horizontally. -->
  <div class="-mx-[var(--gutter)] overflow-x-auto px-[var(--gutter)] pb-2">
    <!-- Lanes sit side by side where there is room and stack where there is
         not. basis-full below sm rather than a width: flex-1 overrides width
         but respects basis, so without it lanes would silently share a phone
         row — tolerable at three roles, 45px each at eight. -->
    <div class="flex flex-wrap items-start gap-3">
      <section
        v-for="(lane, i) in lanes"
        :key="lane"
        class="rise flex min-w-0 flex-1 basis-full flex-col sm:min-w-56 sm:max-w-96 sm:basis-60"
        :style="{ animationDelay: `${i * 40}ms` }"
      >
        <!-- A lane header that reads as a column, not floating text. -->
        <div
          :class="[
            'flex items-baseline gap-2 border-b-2 px-1 pb-2',
            lane === 'done' ? 'border-[var(--status-good)]/50' : 'border-primary/45',
          ]"
        >
          <span class="text-xs font-semibold tracking-wide">{{ lane }}</span>
          <span class="tabular text-muted-foreground ml-auto text-[11px]">
            {{ byLane.get(lane)?.length ?? 0 }}
          </span>
        </div>

        <div class="flex flex-col gap-2 pt-2">
          <!-- A card is a button: the account of what happened is behind it,
               and a card that looks inert while holding the only record of a
               task teaches you not to click it. -->
          <button
            v-for="task in byLane.get(lane)"
            :key="task.id"
            type="button"
            :class="[
              'bg-card hover:border-primary/40 focus-visible:outline-ring w-full border p-2.5 text-left transition-colors focus-visible:outline-2',
              task.state === 'working' && 'border-primary/50 bg-primary/[0.06]',
            ]"
            @click="emit('open', task)"
          >
            <div class="mb-2 text-xs leading-snug font-medium break-words">{{ task.name }}</div>
            <div class="flex flex-wrap items-center gap-1.5">
              <!-- lane says who holds the card, state says whether they are
                   actually working it. Showing only the lane makes a card read
                   as claimed the instant it is delivered. -->
              <Badge :variant="task.state === 'working' ? 'default' : 'outline'">
                {{ task.state }}
              </Badge>
              <Badge v-if="task.reworkCount > 0" variant="secondary" :title="`sent backward ${task.reworkCount} times`">
                ↩ {{ task.reworkCount }}
              </Badge>
              <span v-if="task.activeMs > 0" class="tabular text-muted-foreground ml-auto text-[10px]">
                {{ duration(task.activeMs) }}
              </span>
            </div>
          </button>

          <!-- An empty lane is normal; it should read as quiet, not broken. -->
          <p
            v-if="!byLane.get(lane)?.length"
            class="text-muted-foreground/50 px-1 py-3 text-[11px]"
          >
            empty
          </p>
        </div>
      </section>
    </div>
  </div>
</template>

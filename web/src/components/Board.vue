<script setup lang="ts">
import { computed } from 'vue'
import type { ResolvedRole, Task } from '@/lib/api'
import { duration } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'

const props = defineProps<{ team: ResolvedRole[]; tasks: Task[] }>()

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
    <div class="flex items-start gap-3">
      <section
        v-for="(lane, i) in lanes"
        :key="lane"
        class="rise flex min-w-56 max-w-96 flex-1 basis-60 flex-col"
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
          <article
            v-for="task in byLane.get(lane)"
            :key="task.id"
            :class="[
              'bg-card hover:border-primary/40 border p-2.5 transition-colors',
              task.state === 'working' && 'border-primary/50 bg-primary/[0.06]',
            ]"
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
          </article>

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

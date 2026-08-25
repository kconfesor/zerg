<script setup lang="ts">
import { computed } from 'vue'
import type { ResolvedRole, Task } from '@/lib/api'
import { duration } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'

const props = defineProps<{ team: ResolvedRole[]; tasks: Task[] }>()

/** Lanes are the enabled roles in pipeline order, plus the Done well. */
const lanes = computed(() => [
  ...props.team.filter((r) => r.enabled).map((r) => r.name),
  'done',
])

const byLane = computed(() => {
  const map = new Map<string, Task[]>()
  for (const lane of lanes.value) map.set(lane, [])
  for (const task of props.tasks) map.get(task.lane)?.push(task)
  return map
})
</script>

<template>
  <div class="overflow-x-auto">
    <div class="flex min-w-max gap-3">
      <div v-for="lane in lanes" :key="lane" class="flex w-60 shrink-0 flex-col gap-2">
        <div class="flex items-baseline gap-2 border-b pb-1.5">
          <span class="text-xs font-semibold">{{ lane }}</span>
          <span class="text-muted-foreground text-xs">{{ byLane.get(lane)?.length ?? 0 }}</span>
        </div>

        <Card v-for="task in byLane.get(lane)" :key="task.id" class="py-0">
          <CardContent class="p-2.5">
            <div class="mb-1.5 text-xs font-medium break-words">{{ task.name }}</div>
            <div class="flex flex-wrap items-center gap-1.5">
              <!-- lane says who holds the card; state says whether they are
                   actually working it. Both, because the lane alone is what
                   made the predecessor's board dishonest. -->
              <Badge :variant="task.state === 'working' ? 'default' : 'outline'">
                {{ task.state }}
              </Badge>
              <Badge v-if="task.reworkCount > 0" variant="secondary">↩ {{ task.reworkCount }}</Badge>
              <span v-if="task.activeMs > 0" class="text-muted-foreground text-xs">
                {{ duration(task.activeMs) }} worked
              </span>
            </div>
          </CardContent>
        </Card>

        <p v-if="!byLane.get(lane)?.length" class="text-muted-foreground px-1 py-2 text-xs">—</p>
      </div>
    </div>
  </div>
</template>

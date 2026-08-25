<script setup lang="ts">
import type { Readiness } from '@/lib/api'
import { Badge } from '@/components/ui/badge'

defineProps<{ readiness: Readiness | null }>()

const mark = { ok: '✓', warn: '!', blocked: '✗' } as const
const variant = { ok: 'outline', warn: 'secondary', blocked: 'destructive' } as const
const ink = {
  ok: 'text-[var(--status-good)]',
  warn: 'text-[var(--status-warning)]',
  blocked: 'text-destructive',
} as const
</script>

<template>
  <div v-if="readiness" class="flex flex-col gap-3">
    <!-- A blocked role reads as a blocked role with a remedy. It never reads as
         a role that is simply idle. -->
    <div v-for="role in readiness.roles" :key="role.role" class="border">
      <div class="flex items-center gap-2.5 border-b px-3 py-2">
        <span class="text-sm font-semibold">{{ role.role }}</span>
        <span class="text-muted-foreground text-xs">
          {{ role.harness }} · {{ role.model || 'default' }}
        </span>
        <Badge :variant="variant[role.status]" class="ml-auto uppercase">{{ role.status }}</Badge>
      </div>
      <ul class="divide-y">
        <li
          v-for="check in role.checks"
          :key="check.name"
          class="flex flex-wrap items-baseline gap-x-2.5 gap-y-1 px-3 py-1.5 text-xs"
        >
          <span :class="['w-3 shrink-0 text-center font-bold', ink[check.status]]">
            {{ mark[check.status] }}
          </span>
          <span class="text-muted-foreground w-40 shrink-0">{{ check.name }}</span>
          <span class="min-w-0 flex-1 break-words">{{ check.detail || check.reason }}</span>
          <span
            v-if="check.remedy"
            class="w-full pl-[calc(0.75rem+10rem)] text-xs text-[var(--status-warning)]"
          >
            → {{ check.remedy }}
          </span>
        </li>
      </ul>
    </div>

    <p v-if="!readiness.roles.length" class="text-muted-foreground text-xs">
      This project has no enabled roles. Select at least one in Team before starting.
    </p>
  </div>
</template>

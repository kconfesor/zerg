<script setup lang="ts">
import { ref, shallowRef, watch } from 'vue'
import { api, type ChangedFile } from '@/lib/api'
import { latest } from '@/lib/latest'
import { renderMarkdown } from '@/lib/markdown'
import DiffView from '@/components/DiffView.vue'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'

const props = defineProps<{ featureId: string; head: string; evidence?: string }>()
const open = shallowRef(false)
const files = ref<ChangedFile[] | null>(null)
const base = shallowRef('')
const failed = shallowRef('')
const newest = latest()

watch(() => [props.featureId, props.head, props.evidence], () => {
  newest()
  open.value = false
  files.value = null
  failed.value = ''
})

async function load(on: boolean) {
  open.value = on
  if (!on || files.value) return
  const current = newest()
  failed.value = ''
  try {
    const result = await api.featureFiles(props.featureId, props.head, props.evidence)
    if (!current()) return
    files.value = result.files
    base.value = result.base
  } catch (e) {
    if (current()) failed.value = e instanceof Error ? e.message : String(e)
  }
}

async function loadFile(file: ChangedFile) {
  const id = props.featureId
  const head = props.head
  const evidence = props.evidence
  const current = () => id === props.featureId && head === props.head && evidence === props.evidence
  try {
    const loaded = await api.featureFile(id, head, base.value, file.path, evidence)
    if (!current()) return
    files.value = files.value?.map((f) => f.path === file.path ? loaded : f) ?? null
  } catch (e) {
    if (current()) failed.value = e instanceof Error ? e.message : String(e)
  }
}
</script>

<template>
  <Collapsible :open="open" class="min-w-0" @update:open="load">
    <CollapsibleTrigger as-child>
      <Button variant="outline" size="sm" class="max-w-full">
        {{ open ? 'Hide' : 'Read' }} {{ evidence ? 'evidence' : 'whole change' }}
        <span class="font-mono text-[10px]">{{ head.slice(0, 12) }}</span>
      </Button>
    </CollapsibleTrigger>
    <CollapsibleContent class="mt-2 min-w-0 space-y-2">
      <p v-if="failed" role="alert" class="text-destructive text-xs">{{ failed }}</p>
      <p v-else-if="!files" class="text-muted-foreground text-xs">Reading the commit…</p>
      <p v-else-if="!files.length" class="text-muted-foreground text-xs">No changed files.</p>
      <article v-for="file in files ?? []" :key="file.path" class="min-w-0 border p-2">
        <p class="mb-2 break-all font-mono text-xs">{{ file.path }}</p>
        <Button v-if="file.deferred" variant="outline" size="sm" @click="loadFile(file)">Read file</Button>
        <p v-else-if="file.binary || file.tooLarge" class="text-muted-foreground text-xs">
          {{ file.binary ? 'Binary file' : 'File exceeds the preview limit' }} — inspect this file in git before deciding.
        </p>
        <div v-else-if="evidence && file.path.endsWith('.md')" class="md min-w-0 break-words text-xs" v-html="renderMarkdown(file.content ?? '')" />
        <DiffView v-else :diff="file.diff ?? ''" read-only />
      </article>
    </CollapsibleContent>
  </Collapsible>
</template>

<script setup lang="ts">
/**
 * The Path field for adding a project, with a folder picker beside it.
 *
 * The daemon lists its own filesystem because the repositories live on its
 * disk, not the browser's: the cockpit is served from the daemon and read over
 * the tailnet, so a native directory picker would show the viewer's machine and
 * hand back no usable server path. Typing a path still works — Browse is the way
 * out of having to.
 *
 * One component rather than the same block in two dialogs: adding a project has
 * two entry points, the empty state and the Projects screen, and a picker that
 * differed between them would be a bug waiting to happen.
 */
import { ref } from 'vue'
import { ArrowUp, Folder, FolderGit2 } from '@lucide/vue'
import { api, type BrowseDir } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

defineProps<{ inputId?: string; autofocus?: boolean }>()
const model = defineModel<string>({ required: true })
// Enter in the field submits the dialog it sits in, where the parent wants it —
// the empty-state add has always added on Enter.
const emit = defineEmits<{ submit: [] }>()

const browsing = ref(false)
const dir = ref<BrowseDir | null>(null)
const busy = ref(false)
const error = ref('')

async function browseTo(path: string) {
  busy.value = true
  error.value = ''
  try {
    dir.value = await api.browse(path)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    dir.value = null
  } finally {
    busy.value = false
  }
}

function open() {
  browsing.value = true
  // Start where the typed path points, if anything is typed; the daemon reads
  // an empty path as the operator's home, which is the better default.
  browseTo(model.value.trim())
}

function pick(path: string) {
  model.value = path
  browsing.value = false
}

// A repository is a leaf you pick; a plain folder is a branch you walk into.
// Clicking follows that: a flagged repo fills the field, anything else descends.
function enter(e: { path: string; isRepo: boolean }) {
  if (e.isRepo) pick(e.path)
  else browseTo(e.path)
}
</script>

<template>
  <div class="flex flex-col gap-2">
    <div class="flex gap-2">
      <Input
        :id="inputId"
        v-model="model"
        placeholder="/Users/you/source/your-repo"
        class="flex-1"
        :autofocus="autofocus"
        @keyup.enter="emit('submit')"
      />
      <Button type="button" variant="outline" class="shrink-0 gap-1.5" @click="open">
        <Folder :size="14" aria-hidden="true" />
        Browse
      </Button>
    </div>

    <!-- The picker, inline. It belongs to the field above, not to a dialog of
         its own, so it lists in place. -->
    <div v-if="browsing" class="flex flex-col border">
      <div class="bg-muted/40 flex items-center gap-2 border-b px-2.5 py-2">
        <span class="min-w-0 flex-1 truncate font-mono text-[11px]" :title="dir?.path">
          {{ dir?.path ?? '…' }}
        </span>
        <Button
          type="button"
          size="sm"
          variant="outline"
          class="h-7 shrink-0"
          :disabled="busy || !dir"
          @click="pick(dir!.path)"
        >
          Use this folder
        </Button>
      </div>
      <div class="max-h-64 overflow-y-auto">
        <button
          v-if="dir?.parent"
          type="button"
          class="hover:bg-muted flex w-full items-center gap-2 px-2.5 py-1.5 text-left text-xs"
          :disabled="busy"
          @click="browseTo(dir!.parent)"
        >
          <ArrowUp :size="14" aria-hidden="true" class="text-muted-foreground shrink-0" />
          <span class="text-muted-foreground">Up</span>
        </button>
        <button
          v-for="e in dir?.entries ?? []"
          :key="e.path"
          type="button"
          class="hover:bg-muted flex w-full items-center gap-2 px-2.5 py-1.5 text-left text-xs"
          :disabled="busy"
          @click="enter(e)"
        >
          <component
            :is="e.isRepo ? FolderGit2 : Folder"
            :size="14"
            aria-hidden="true"
            :class="['shrink-0', e.isRepo ? 'text-primary' : 'text-muted-foreground']"
          />
          <span class="min-w-0 flex-1 truncate">{{ e.name }}</span>
          <span v-if="e.isRepo" class="text-primary/70 shrink-0 text-[10px]">repo</span>
        </button>
        <p
          v-if="dir && !dir.entries.length && !busy"
          class="text-muted-foreground px-2.5 py-3 text-[11px]"
        >
          No subfolders here. Use this folder if it is the repository, or go up.
        </p>
      </div>
      <p v-if="error" class="text-destructive px-2.5 py-2 text-[11px]">{{ error }}</p>
    </div>
  </div>
</template>

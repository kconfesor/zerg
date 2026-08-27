<script setup lang="ts">
/**
 * The Path field for adding a project, with a folder picker beside it.
 *
 * The daemon lists its own filesystem because the repositories live on its
 * disk, not the browser's: the cockpit is served from the daemon and read over
 * the tailnet, so a native directory picker would show the viewer's machine and
 * hand back no usable server path. Typing a path still works, and Browse is the
 * way out of having to.
 *
 * One component rather than the same block in two dialogs: adding a project has
 * two entry points, the empty state and the Projects screen, and a picker that
 * differed between them would be a bug waiting to happen.
 */
import { ref } from 'vue'
import { ArrowLeft, ArrowUp, Folder, FolderGit2 } from '@lucide/vue'
import { api, type BrowseDir } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

defineProps<{ inputId?: string; autofocus?: boolean }>()
const model = defineModel<string>({ required: true })
// Enter in the field submits the dialog it sits in, where the parent wants it:
// the empty-state add has always added on Enter.
const emit = defineEmits<{ submit: [] }>()

const browsing = ref(false)
const dir = ref<BrowseDir | null>(null)
const busy = ref(false)
const error = ref('')

/**
 * Where the picker has been, so Back returns there.
 *
 * Up and Back are different questions and only one of them was answerable.
 * Up goes to the parent, which is no help after picking a repository three
 * folders away from where you started: the way back was closing the dialog and
 * beginning again. This is the browser's Back, not its parent link.
 */
const history = ref<string[]>([])

async function browseTo(path: string, remember = true) {
  if (remember && dir.value) history.value.push(dir.value.path)
  busy.value = true
  error.value = ''
  try {
    dir.value = await api.browse(path)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    // The listing that failed is not where we are, so a failed step does not
    // become a step to go back to.
    if (remember) history.value.pop()
  } finally {
    busy.value = false
  }
}

function back() {
  const previous = history.value.pop()
  if (previous) browseTo(previous, false)
}

function open() {
  browsing.value = !browsing.value
  if (!browsing.value) return

  // Reopening resumes where you were, which is the point of Back surviving a
  // close. Unless the field has since been pointed somewhere else, in which
  // case the typed path is the more recent instruction and wins. An empty
  // field means the operator's home, which is what the daemon reads it as.
  const typed = model.value.trim()
  if (!dir.value || (typed && typed !== dir.value.path)) browseTo(typed, dir.value !== null)
}

function pick(path: string) {
  model.value = path
}

// Clicking a row descends, whatever it is. Picking is the button beside it.
//
// Clicking a repository used to fill the field and close the picker, which made
// one wrong click cost the whole session: the field was set, the picker was
// gone, and reopening landed inside the repository rather than back in the
// folder it was chosen from. It also made a repository a place you could not
// look inside, which is wrong for anyone whose repositories contain
// repositories.
function enter(e: { path: string; isRepo: boolean }) {
  browseTo(e.path)
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
        <Button
          type="button"
          size="icon-xs"
          variant="ghost"
          class="shrink-0"
          :disabled="busy || !history.length"
          :title="history.length ? 'Back to ' + history[history.length - 1] : 'Nowhere to go back to'"
          aria-label="Back"
          @click="back"
        >
          <ArrowLeft :size="14" />
        </Button>
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
        <div
          v-for="e in dir?.entries ?? []"
          :key="e.path"
          class="hover:bg-muted flex items-center gap-2 pr-1.5"
        >
          <button
            type="button"
            class="flex min-w-0 flex-1 items-center gap-2 px-2.5 py-1.5 text-left text-xs"
            :disabled="busy"
            :title="'Open ' + e.name"
            @click="enter(e)"
          >
            <component
              :is="e.isRepo ? FolderGit2 : Folder"
              :size="14"
              aria-hidden="true"
              :class="['shrink-0', e.isRepo ? 'text-primary' : 'text-muted-foreground']"
            />
            <span class="min-w-0 flex-1 truncate">{{ e.name }}</span>
          </button>
          <!-- Choosing is a button of its own. As a click on the row it cost a
               misclick the whole session, and made a repository somewhere you
               could not look inside. -->
          <Button
            v-if="e.isRepo"
            type="button"
            size="xs"
            variant="outline"
            class="h-6 shrink-0"
            :disabled="busy"
            @click="pick(e.path)"
          >
            Use
          </Button>
        </div>
        <p
          v-if="dir?.truncated"
          class="text-muted-foreground border-t px-2.5 py-2 text-[11px]"
        >
          Showing the first {{ dir.entries.length }} folders here. Type the path if the one you want
          is not among them.
        </p>
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

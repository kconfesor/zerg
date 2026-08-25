<script setup lang="ts">
/**
 * The daemon's own settings.
 *
 * Grouped by when a change takes effect, because that is the thing people get
 * wrong: network settings need a restart, everything else is live. Saying so
 * next to the fields beats a paragraph nobody reads.
 */
import { computed, ref, watch } from 'vue'
import { api, type DaemonConfig, type SettingsResponse } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Checkbox } from '@/components/ui/checkbox'
import { Textarea } from '@/components/ui/textarea'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

const props = defineProps<{ projectId: string | null }>()

const data = ref<SettingsResponse | null>(null)
const form = ref<DaemonConfig | null>(null)
const saving = ref(false)
const note = ref<{ tone: 'ok' | 'bad'; text: string } | null>(null)
const sweeping = ref(false)
const tab = ref('network')

/** The protocol document every role is given, on top of its own prompt. It had
 *  an endpoint and no UI, so until now it could only be edited with curl. */
const instructions = ref('')
const savingInstructions = ref(false)

async function load() {
  try {
    data.value = await api.settings()
    form.value = { ...data.value.config }
    instructions.value = (await api.sharedInstructions()).text
  } catch (e) {
    note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
  }
}
load()

async function save() {
  if (!form.value) return
  saving.value = true
  note.value = null
  try {
    data.value = await api.setSettings(form.value)
    form.value = { ...data.value.config }
    note.value = data.value.restartNeeded
      ? { tone: 'ok', text: 'Saved. The address and TLS take effect when the daemon restarts.' }
      : { tone: 'ok', text: 'Saved.' }
  } catch (e) {
    note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
  } finally {
    saving.value = false
  }
}

async function sweepNow() {
  if (!props.projectId) return
  sweeping.value = true
  note.value = null
  try {
    const r = await api.sweep(props.projectId)
    const mb = (r.bytesFreed / 1048576).toFixed(1)
    const branches = r.branchesPruned?.length ?? 0
    note.value = {
      tone: 'ok',
      text:
        r.bytesFreed === 0 && !branches
          ? 'Nothing to reclaim — the worktrees are already clean.'
          : `Freed ${mb} MB${branches ? `, pruned ${branches} branch${branches > 1 ? 'es' : ''}` : ''}.`,
    }
  } catch (e) {
    note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
  } finally {
    sweeping.value = false
  }
}

async function saveInstructions() {
  savingInstructions.value = true
  note.value = null
  try {
    await api.setSharedInstructions(instructions.value)
    note.value = {
      tone: 'ok',
      text: 'Shared instructions saved. Roles pick them up the next time they spawn.',
    }
  } catch (e) {
    note.value = { tone: 'bad', text: e instanceof Error ? e.message : String(e) }
  } finally {
    savingInstructions.value = false
  }
}

const ts = computed(() => data.value?.tailnet)

/** Suggest the tailnet address, since typing an IP from memory is how you end
 *  up bound to nothing. */
function useTailnet() {
  if (!form.value || !ts.value?.ips?.length) return
  const port = form.value.addr.split(':').pop() || '7717'
  form.value.addr = `${ts.value.ips[0]}:${port}`
  form.value.tlsMode = ts.value.httpsEnabled ? 'tailscale' : form.value.tlsMode
}

watch(
  () => form.value?.tlsMode,
  (mode) => {
    // A name is only meaningful for a tailscale certificate.
    if (form.value && mode !== 'tailscale') form.value.tailnetHost = ''
  },
)

const loopback = computed(() => {
  const host = form.value?.addr.split(':').slice(0, -1).join(':') ?? ''
  return ['127.0.0.1', '::1', 'localhost', ''].includes(host)
})
</script>

<template>
  <div v-if="form" class="flex w-full flex-col gap-4">
    <p
      v-if="note"
      :class="[
        'px-3 py-2 text-xs',
        note.tone === 'bad'
          ? 'bg-destructive/10 text-destructive'
          : 'bg-[var(--status-good)]/10 text-[var(--status-good)]',
      ]"
    >
      {{ note.text }}
    </p>

    <!-- default-value, not v-model. v-model makes this controlled, and if the
         update never reaches the ref the tabs are pinned to their initial value
         with no visible error — which is exactly what happened. Uncontrolled
         plus a listener keeps reka-ui in charge of the switching and mirrors it
         out for the save button. -->
    <Tabs default-value="network" @update:model-value="(v) => (tab = String(v))">
      <TabsList>
        <TabsTrigger value="network">Network</TabsTrigger>
        <TabsTrigger value="disk">Disk</TabsTrigger>
        <TabsTrigger value="instructions">Instructions</TabsTrigger>
      </TabsList>

      <!-- ── Network ──────────────────────────────────────────────────── -->
      <TabsContent value="network" class="pt-4">
        <Card>
          <CardHeader>
            <CardTitle class="flex items-center gap-2 text-sm">
              How the cockpit is served
              <Badge variant="outline">applies on restart</Badge>
            </CardTitle>
            <CardDescription class="text-[11px]">
              Where it listens, and whether the connection is encrypted.
            </CardDescription>
          </CardHeader>
          <CardContent class="flex flex-col gap-4">

      <div class="flex flex-col gap-1.5">
        <Label for="addr">Listen on</Label>
        <div class="flex flex-wrap items-center gap-2">
          <Input id="addr" v-model="form.addr" class="max-w-64 font-mono text-xs" />
          <Button
            v-if="ts?.available"
            size="sm"
            variant="outline"
            :disabled="!ts.ips?.length"
            @click="useTailnet"
          >
            Use tailnet address
          </Button>
        </div>
        <p class="text-muted-foreground text-[11px] leading-snug">
          <template v-if="loopback">
            Only this machine can reach the cockpit. There is no authentication, so any other
            address hands whatever can route to it the ability to start agents and read every
            transcript.
          </template>
          <template v-else>
            Reachable beyond this machine, and <strong>there is no authentication</strong> — treat
            anything that can route to it as trusted. A tailnet address means your own devices;
            <code>0.0.0.0</code> also means whatever else is on the local network.
          </template>
        </p>
      </div>

      <div class="flex flex-col gap-1.5">
        <Label for="tls">TLS</Label>
        <Select id="tls" v-model="form.tlsMode">
          <SelectTrigger size="sm" class="max-w-64"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value="off">Off — plain HTTP</SelectItem>
            <SelectItem value="tailscale">Tailscale certificate</SelectItem>
            <SelectItem value="files">Certificate and key files</SelectItem>
          </SelectContent>
        </Select>

        <template v-if="form.tlsMode === 'tailscale'">
          <p v-if="!ts?.available" class="text-destructive text-[11px]">
            {{ ts?.reason ?? 'Tailscale is not available on this machine.' }}
          </p>
          <p v-else-if="!ts.httpsEnabled" class="text-[11px] leading-snug text-[var(--status-warning)]">
            HTTPS certificates are switched off for this tailnet, so a certificate cannot be issued
            yet. Turn it on under <strong>DNS → HTTPS Certificates</strong> in the Tailscale admin
            console, then restart.
          </p>
          <p v-else class="text-muted-foreground text-[11px]">
            A real certificate for <code>{{ ts.dnsName }}</code> — no browser warning on your phone.
          </p>
        </template>

        <div v-else-if="form.tlsMode === 'files'" class="flex flex-col gap-1.5 pt-1">
          <Input v-model="form.certFile" placeholder="/path/to/cert.pem" class="font-mono text-xs" />
          <Input v-model="form.keyFile" placeholder="/path/to/key.pem" class="font-mono text-xs" />
        </div>
      </div>

      <label v-if="!loopback" class="flex items-start gap-2 text-xs">
        <Checkbox
          :model-value="form.localAccess"
          class="mt-0.5"
          @update:model-value="(v) => (form!.localAccess = !!v)"
        />
        <span>
          Also serve <code>http://localhost</code> on the same port
          <span class="text-muted-foreground block text-[11px] leading-snug">
            Plain HTTP, because a certificate issued for a tailnet name does not match
            <code>localhost</code>. Loopback is already the same trust boundary as the shell that
            started the daemon — and it is the way back in if a setting here breaks the other
            listener.
          </span>
        </span>
      </label>

      <div v-if="ts?.available" class="text-muted-foreground text-[11px]">
        This machine on the tailnet: <code>{{ ts.dnsName }}</code>
        <span v-if="ts.ips?.length"> · {{ ts.ips[0] }}</span>
      </div>
          </CardContent>
        </Card>
      </TabsContent>


      <!-- ── Disk ─────────────────────────────────────────────────────── -->
      <TabsContent value="disk" class="pt-4">
        <Card>
          <CardHeader>
            <CardTitle class="flex items-center gap-2 text-sm">
              Reclaiming disk
              <Badge variant="outline">applies immediately</Badge>
            </CardTitle>
            <CardDescription class="text-[11px]">
              Each role gets its own worktree, and build output dominates them.
            </CardDescription>
          </CardHeader>
          <CardContent class="flex flex-col gap-4">
      <p class="text-muted-foreground text-[11px] leading-snug">
        Each role gets its own worktree, and build output dominates them: a Rust calculator is 256 KB
        of source against 45 MB of <code>target/</code>, per role. Sweeping removes only files the
        project's own <code>.gitignore</code> already calls disposable — never untracked work.
      </p>

      <div class="flex flex-col gap-1.5">
        <Label for="clean">Sweep worktrees</Label>
        <Select id="clean" v-model="form.cleanPolicy">
          <SelectTrigger size="sm" class="max-w-64"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value="never">Never</SelectItem>
            <SelectItem value="on_done">When a task reaches Done</SelectItem>
            <SelectItem value="on_start">When the daemon starts</SelectItem>
          </SelectContent>
        </Select>
      </div>

      <label class="flex items-center gap-2 text-xs">
        <Checkbox
          :model-value="form.pruneMergedBranches"
          @update:model-value="(v) => (form!.pruneMergedBranches = !!v)"
        />
        Delete <code>zerg-*</code> branches already merged into the base branch
      </label>

      <div class="flex flex-col gap-1.5">
        <Label for="retention">Keep transcripts for</Label>
        <div class="flex items-center gap-2">
          <Input
            id="retention"
            v-model.number="form.eventRetentionDays"
            type="number"
            min="1"
            max="3650"
            class="max-w-24 text-xs"
          />
          <span class="text-muted-foreground text-[11px]">days</span>
        </div>
        <p class="text-muted-foreground text-[11px] leading-snug">
          Only the transcript. What a task cost, how long it took and how it ended are kept
          indefinitely.
        </p>
      </div>

      <div>
        <Button size="sm" variant="outline" :disabled="!projectId || sweeping" @click="sweepNow">
          {{ sweeping ? 'Sweeping…' : 'Sweep this project now' }}
        </Button>
      </div>
          </CardContent>
        </Card>
      </TabsContent>


      <!-- ── Shared instructions ──────────────────────────────────────── -->
      <TabsContent value="instructions" class="pt-4">
        <Card>
          <CardHeader>
            <CardTitle class="flex items-center gap-2 text-sm">
              Shared instructions
              <Badge variant="outline">applies on next spawn</Badge>
            </CardTitle>
            <CardDescription class="text-[11px]">
              Given to every role on top of its own prompt.
            </CardDescription>
          </CardHeader>
          <CardContent class="flex flex-col gap-3">
      <p class="text-muted-foreground text-[11px] leading-snug">
        Given to every role on top of its own prompt. Role prompts cover the job; this covers the
        protocol — claiming work, handing it on, asking a question — so the two cannot drift apart
        and a protocol change is one edit.
      </p>
      <Textarea v-model="instructions" rows="14" class="font-mono text-[11px]" />
      <div>
        <Button size="sm" variant="outline" :disabled="savingInstructions" @click="saveInstructions">
          {{ savingInstructions ? 'Saving…' : 'Save instructions' }}
        </Button>
      </div>
          </CardContent>
        </Card>
      </TabsContent>
    </Tabs>


    <!-- Only for the tabs it saves. Instructions are stored separately and have
         their own button; a second one that quietly ignored what you just typed
         would be worse than no button at all. -->
    <div v-if="tab !== 'instructions'" class="flex items-center gap-3">
      <Button :disabled="saving" @click="save">{{ saving ? 'Saving…' : 'Save settings' }}</Button>
      <span v-if="data?.restartNeeded" class="text-[11px] text-[var(--status-warning)]">
        Serving {{ data.applied }} until restart.
      </span>
    </div>
  </div>
</template>

/**
 * The cockpit's view of the daemon.
 *
 * Commands are ordinary REST calls with status codes, which is why they are not
 * on the event stream: a rejected start is a 409 with a readiness report
 * attached, not a hand-rolled error frame.
 */

export interface RoleTemplate {
  id: string
  name: string
  harness: string
  model: string
  args: string[]
  receive: 'task' | 'batch'
  batchMaxItems: number
  batchMaxAgeSec: number
  prompt: string
  gate: 'none' | 'approval'
  builtin: boolean
}

export interface RoleOverrides {
  harnessOverride?: string | null
  modelOverride?: string | null
  /** null inherits; [] explicitly removes every argument. */
  argsOverride: string[] | null
  receiveOverride?: 'task' | 'batch' | null
  batchMaxItemsOverride?: number | null
  batchMaxAgeSecOverride?: number | null
  promptOverride?: string | null
  gateOverride?: 'none' | 'approval' | null
}

export interface ResolvedRole extends RoleTemplate, RoleOverrides {
  position: number
  enabled: boolean
  overridden: boolean
  terminal: boolean
}

export interface ProjectRole extends RoleOverrides {
  templateId: string
  position?: number
  enabled: boolean
}

export interface TeamPresetRole extends RoleOverrides {
  templateId: string
  position: number
  enabled: boolean
}

export interface TeamPreset {
  id: string
  name: string
  builtin: boolean
  roles: TeamPresetRole[]
  createdAt: string
  updatedAt: string
}

export interface ProjectTeam {
  presetId: string | null
  topologyOverride: boolean
  roles: ResolvedRole[]
}

export interface ProjectTeamUpdate {
  presetId: string | null
  topologyOverride: boolean
  roles: ProjectRole[]
}

export type Integration = 'merge' | 'branch' | 'pr'

export interface Project {
  id: string
  path: string
  name: string
  baseBranch: string
  /** How finished work reaches the base branch. */
  integration: Integration
  /** Only used when integration is pr. */
  prDraft: boolean
  /** The reusable team this project follows, or empty for a standalone team. */
  teamPresetId?: string
  teamTopologyOverride?: boolean
  /** What answers questions in Chat. Empty inherits the terminal role. */
  chatHarness?: string
  chatModel?: string
  /** A path inside the repository to its own logo or favicon, or empty for the
   *  mark derived from the project instead. */
  icon?: string
  createdAt: string
  lastOpenedAt?: string
}

export interface Task {
  id: string
  projectId: string
  name: string
  body: string
  lane: string
  state: 'queued' | 'working' | 'done' | 'rejected'
  createdAt: string
  /** When a role first picked it up, which is when work actually began. */
  firstClaimedAt?: string
  completedAt?: string
  activeMs: number
  reworkCount: number
  /** Total tokens and cost across every role and every lap. */
  tokens: number
  costUsd: number
  /** The most recent thing an agent did on this card. */
  doing?: string
  /** Put away by a person. Finished work that is still finished. */
  hidden?: boolean
}

export interface CheckResult {
  name: string
  status: 'ok' | 'warn' | 'blocked'
  detail?: string
  reason?: string
  remedy?: string
}

export interface RoleReport {
  role: string
  harness: string
  model: string
  status: 'ok' | 'warn' | 'blocked'
  checks: CheckResult[]
}

export interface Readiness {
  projectId: string
  ready: boolean
  roles: RoleReport[]
  checkedAt: string
}

export interface RoleStatus {
  role: string
  harness: string
  model: string
  state: string
  lastError?: string
  restarts: number
  terminal: boolean
  /** When a spent provider quota is expected to lift. Only while throttled. */
  throttledUntil?: string
}

export interface Workspace {
  worktrees: { role: string; bytes: number }[]
  bytes: number
}

export interface QuotaReport {
  /** Whose account these windows belong to: "openai-codex", "anthropic". */
  provider: string
  plan?: string
  windows: QuotaWindow[]
  /** When this was last learned, so a stale gauge can say so. */
  seenAt: string
}

export interface QuotaWindow {
  /** "5h", "7d" — the window's length, which is what both providers agree on. */
  label: string
  /** 0..1 */
  used: number
  resetsAt?: string
}

export interface SwarmStatus {
  running: boolean
  roles: RoleStatus[]
  /** Subscription headroom, keyed by provider — one account, many roles. */
  quotas?: Record<string, QuotaReport>
}

export interface Approval {
  id: string
  messageId: string
  state: string
  taskName?: string
  taskId?: string
  fromRole?: string
  /** What the role wrote when it handed the work on. */
  body?: string
  commit?: string
  /** The approval that performs the merge, rather than one between roles. */
  terminal?: boolean
  createdAt: string
}

export interface Clarification {
  id: string
  /** The card this question is about, so the board can mark it. */
  taskId?: string
  role: string
  question: string
  state: string
  createdAt: string
}

export interface Attention {
  approvals: Approval[]
  clarifications: Clarification[]
  rework: { threshold: number; tasks: Task[] }
}

export interface Model {
  ID: string
  Label: string
  Provider: string
  Context: number
}

/** ApiError carries the status so a caller can treat 409 differently from 500. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly body?: unknown,
  ) {
    super(message)
  }
}

/**
 * Where the browser fetches a project's mark from.
 *
 * A URL rather than a fetch: this goes in an <img src>, so the browser handles
 * caching, revalidation and the failure — a mark that has been deleted from the
 * repository fires an error event and the avatar quietly falls back to
 * initials, with no request for the view to make or handle itself.
 *
 * The stored path is in the query string so that editing which file is used
 * changes the URL, and the browser does not show the previous project's mark
 * out of cache while the new one loads.
 */
export function projectIconURL(project: { id: string; icon?: string }): string {
  return `/api/projects/${project.id}/icon?p=${encodeURIComponent(project.icon ?? '')}`
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })
  if (res.status === 204) return undefined as T

  const text = await res.text()
  const body = text ? JSON.parse(text) : undefined
  if (!res.ok) {
    const message =
      body && typeof body === 'object' && 'error' in body
        ? String((body as { error: unknown }).error)
        : res.statusText
    throw new ApiError(message, res.status, body)
  }
  return body as T
}

export const api = {
  health: () => call<{ status: string }>('/health'),

  harnesses: () => call<string[]>('/harnesses'),
  models: (harness: string) => call<Model[]>(`/harnesses/${harness}/models`),

  roles: () => call<RoleTemplate[]>('/roles'),
  teamPresets: () => call<TeamPreset[]>('/team-presets'),
  createTeamPreset: (p: Pick<TeamPreset, 'name' | 'roles'>) =>
    call<TeamPreset>('/team-presets', { method: 'POST', body: JSON.stringify(p) }),
  updateTeamPreset: (p: TeamPreset) =>
    call<TeamPreset>(`/team-presets/${p.id}`, { method: 'PUT', body: JSON.stringify(p) }),
  deleteTeamPreset: (id: string) =>
    call<void>(`/team-presets/${id}`, { method: 'DELETE' }),
  createRole: (r: Partial<RoleTemplate>) =>
    call<RoleTemplate>('/roles', { method: 'POST', body: JSON.stringify(r) }),
  updateRole: (r: RoleTemplate) =>
    call<RoleTemplate>(`/roles/${r.id}`, { method: 'PUT', body: JSON.stringify(r) }),
  deleteRole: (id: string) => call<void>(`/roles/${id}`, { method: 'DELETE' }),

  projects: () => call<Project[]>('/projects'),
  createProject: (path: string, baseBranch: string) =>
    call<Project>('/projects', { method: 'POST', body: JSON.stringify({ path, baseBranch }) }),
  openProject: (id: string) => call<Project>(`/projects/${id}/open`, { method: 'POST' }),
  deleteProject: (id: string) => call<void>(`/projects/${id}`, { method: 'DELETE' }),

  team: (id: string) => call<ProjectTeam>(`/projects/${id}/team`),
  setTeam: (id: string, team: ProjectTeamUpdate) =>
    call<ProjectTeam>(`/projects/${id}/team`, { method: 'PUT', body: JSON.stringify(team) }),

  readiness: (id: string) => call<Readiness>(`/projects/${id}/readiness`),
  status: (id: string) => call<SwarmStatus>(`/projects/${id}/status`),
  start: (id: string) => call<SwarmStatus>(`/projects/${id}/start`, { method: 'POST' }),
  stop: (id: string) => call<{ status: string }>(`/projects/${id}/stop`, { method: 'POST' }),

  tasks: (id: string) => call<Task[]>(`/projects/${id}/tasks`),
  newTask: (id: string, name: string, body: string) =>
    call<Task>(`/projects/${id}/tasks`, { method: 'POST', body: JSON.stringify({ name, body }) }),

  attention: (id: string) => call<Attention>(`/projects/${id}/attention`),
  approve: (id: string) => call<void>(`/approvals/${id}/approve`, { method: 'POST' }),
  reject: (id: string, note: string) =>
    call<void>(`/approvals/${id}/reject`, { method: 'POST', body: JSON.stringify({ note }) }),
  answer: (id: string, answer: string) =>
    call<void>(`/clarifications/${id}/answer`, { method: 'POST', body: JSON.stringify({ answer }) }),

  chat: (id: string, message: string) =>
    call<{ status: string }>(`/projects/${id}/chat`, {
      method: 'POST',
      body: JSON.stringify({ message }),
    }),

  taskDetail: (id: string) => call<TaskDetail>(`/tasks/${id}`),
  workspace: (projectId: string) => call<Workspace>(`/projects/${projectId}/workspace`),
  resetChat: (projectId: string) =>
    call<void>(`/projects/${projectId}/chat`, { method: 'DELETE' }),
  setChatAgent: (projectId: string, harness: string, model: string) =>
    call<Project>(`/projects/${projectId}/chat-agent`, {
      method: 'PUT',
      body: JSON.stringify({ harness, model }),
    }),
  stopTask: (id: string) => call<Task>(`/tasks/${id}/stop`, { method: 'POST' }),
  deleteTask: (id: string) => call<void>(`/tasks/${id}`, { method: 'DELETE' }),
  setTaskHidden: (id: string, hidden: boolean) =>
    call<Task>(`/tasks/${id}/hidden`, { method: 'PUT', body: JSON.stringify({ hidden }) }),
  approvalDiff: (id: string) =>
    call<{ files: ChangedFile[]; range: boolean; base: string }>(`/approvals/${id}/diff`),
  renameProject: (id: string, name: string) =>
    call<Project>(`/projects/${id}/name`, {
      method: 'PUT',
      body: JSON.stringify({ name }),
    }),

  /** The images in a project that look like its mark, best guesses first. */
  projectIcons: (id: string) =>
    call<{ candidates: { path: string; bytes: number }[] }>(`/projects/${id}/icons`),

  setProjectIcon: (id: string, icon: string) =>
    call<Project>(`/projects/${id}/icon`, {
      method: 'PUT',
      body: JSON.stringify({ icon }),
    }),

  setIntegration: (id: string, integration: Integration, prDraft: boolean) =>
    call<Project>(`/projects/${id}/integration`, {
      method: 'PUT',
      body: JSON.stringify({ integration, prDraft }),
    }),

  settings: () => call<SettingsResponse>('/settings'),
  setSettings: (cfg: DaemonConfig) =>
    call<SettingsResponse>('/settings', { method: 'PUT', body: JSON.stringify(cfg) }),
  sweep: (id: string) =>
    call<{ bytesFreed: number; branchesPruned: string[] | null }>(`/projects/${id}/sweep`, {
      method: 'POST',
    }),

  usage: (id: string, by: 'role' | 'provider' | 'model' = 'role') =>
    call<UsageTotal[]>(`/projects/${id}/usage?by=${by}`),

  sharedInstructions: () => call<{ text: string }>('/settings/shared-instructions'),
  setSharedInstructions: (text: string) =>
    call<{ text: string }>('/settings/shared-instructions', {
      method: 'PUT',
      body: JSON.stringify({ text }),
    }),
}

// ── activity ────────────────────────────────────────────────────────────────

/** One thing an agent did. */
export interface ActivityEvent {
  id: string
  projectId: string
  taskId?: string
  role: string
  kind:
    | 'ready'
    | 'thinking'
    | 'message'
    | 'tool_call'
    | 'tool_done'
    | 'usage'
    | 'turn_end'
    | 'error'
  at: string
  text?: string
  tool?: string
  data?: Record<string, unknown>
  fatal?: boolean
}

export interface UsageTotal {
  key: string
  turns: number
  inputTokens: number
  cacheWriteTokens: number
  cacheReadTokens: number
  outputTokens: number
  costUsd: number
  /** Turns on a plan: real tokens, no marginal dollar cost. */
  subscriptionTurns: number
  /** Turns whose harness reported no cost. Their tokens count; their price
   *  does not, so costUsd is the priced part rather than the whole. */
  unpricedTurns: number
}

export interface ActivityStream {
  close(): void
}

/** Where a stream currently is, so the UI can say so rather than guess. */
export type StreamState = 'connecting' | 'live' | 'reconnecting' | 'error'

/**
 * Subscribe to a project's activity: history first, then live.
 *
 * A WebSocket rather than SSE, so the same connection carries messages back
 * when terminal takeover lands. The cost is that everything EventSource did
 * for free is done here instead, and all of it matters:
 *
 *  - **Resume.** Every event id is a monotonic ULID, and the last one seen is
 *    sent as the cursor on every (re)connect. A drop loses nothing, and the
 *    server sends only what came after.
 *  - **Backoff.** Reconnecting immediately and forever turns a restarting
 *    daemon into a request flood. Delay doubles to a ceiling, with jitter so
 *    several open tabs do not retry in lockstep.
 *  - **Intent.** A socket closed by close() must stay closed; only an
 *    unexpected drop reconnects.
 */
export function streamActivity(
  projectId: string,
  handlers: {
    onEvent: (e: ActivityEvent) => void
    onCaughtUp?: (replayed: number) => void
    onState?: (state: StreamState) => void
    /** The server reported a problem in-band. It stays connected long enough
     *  to say so, which is why this is not the same as a dropped socket. */
    onError?: (message: string) => void
  },
  opts: { role?: string; limit?: number; task?: string } = {},
): ActivityStream {
  const RETRY_MIN = 500
  const RETRY_MAX = 15_000

  let socket: WebSocket | null = null
  let retry = RETRY_MIN
  let timer: number | undefined
  let closed = false

  // The cursor. Kept across reconnects — it is the entire reason a drop is
  // invisible rather than a hole in the transcript.
  let after: string | undefined

  const url = () => {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    return `${proto}//${location.host}/api/projects/${projectId}/stream`
  }

  function connect() {
    if (closed) return
    handlers.onState?.(after ? 'reconnecting' : 'connecting')

    const ws = new WebSocket(url())
    socket = ws

    ws.onopen = () => {
      // Subscribing is an explicit frame rather than query parameters so that
      // a resumed connection states its cursor, and so later message types
      // share one envelope.
      ws.send(
        JSON.stringify({
          type: 'subscribe',
          after,
          role: opts.role,
          task: opts.task,
          limit: opts.limit,
        }),
      )
    }

    ws.onmessage = (raw) => {
      let frame: { type: string; event?: ActivityEvent; replayed?: number; message?: string }
      try {
        frame = JSON.parse(raw.data)
      } catch {
        return // one malformed frame is not a reason to drop the connection
      }
      if (frame.type === 'activity' && frame.event) {
        after = frame.event.id
        handlers.onEvent(frame.event)
      } else if (frame.type === 'caught-up') {
        // Only now is the connection known good, so the backoff resets here
        // rather than on open — a socket that opens and immediately fails
        // would otherwise never back off at all.
        retry = RETRY_MIN
        handlers.onState?.('live')
        handlers.onCaughtUp?.(frame.replayed ?? 0)
      } else if (frame.type === 'error') {
        // The server says the replay query failed, and will not send
        // caught-up. Unhandled, the view sat on "connecting" for ever — a
        // failure that looked exactly like a slow connection. Say so, then
        // close so the existing backoff retries it.
        handlers.onState?.('error')
        handlers.onError?.(frame.message ?? 'the server could not read history')
        ws.close()
      }
    }

    const reconnect = () => {
      if (closed || socket !== ws) return
      socket = null
      handlers.onState?.('reconnecting')
      // Jitter keeps several tabs from retrying on the same tick.
      const wait = retry + Math.random() * retry * 0.3
      retry = Math.min(retry * 2, RETRY_MAX)
      timer = window.setTimeout(connect, wait)
    }
    ws.onclose = reconnect
    ws.onerror = () => ws.close()
  }

  connect()

  return {
    close() {
      closed = true
      window.clearTimeout(timer)
      socket?.close()
      socket = null
    },
  }
}

// ── settings ────────────────────────────────────────────────────────────────

/** The daemon's own configuration, as the settings form edits it. */
export interface DaemonConfig {
  addr: string
  tlsMode: 'off' | 'tailscale' | 'files'
  certFile?: string
  keyFile?: string
  tailnetHost?: string
  localAccess: boolean
  harness?: Record<string, { flags: string[] }>
  eventRetentionDays: number
  cleanPolicy: 'never' | 'on_done' | 'on_start'
  cleanIgnored: boolean
  pruneMergedBranches: boolean
}

/** What the local tailscaled reports. Absent Tailscale is a normal state, not
 *  an error, so `available` is false with a reason rather than a failed call. */
export interface TailnetStatus {
  available: boolean
  reason?: string
  dnsName?: string
  ips?: string[]
  httpsEnabled: boolean
}

export interface SettingsResponse {
  config: DaemonConfig
  tailnet: TailnetStatus
  /** The address actually being served, which differs from config.addr when
   *  settings have been saved but the daemon has not restarted. */
  applied: string
  restartNeeded: boolean
}

// ── task detail ─────────────────────────────────────────────────────────────

/** One step of a task's history: who handed what to whom, and what they said. */
export interface TaskStep {
  from: string
  to?: string
  kind: string
  commit?: string
  body: string
  at: string
  final?: boolean
  /** First line of the commit message. */
  subject?: string
}

export interface TaskDetail {
  task: Task
  history: TaskStep[]
  usage: UsageTotal
}

/** One file a commit touched, with both its content and its diff. */
export interface ChangedFile {
  path: string
  /** git's letter: A added, M modified, D deleted. */
  status: 'A' | 'M' | 'D' | string
  content?: string
  diff?: string
}

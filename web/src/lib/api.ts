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

export interface ResolvedRole extends RoleTemplate {
  position: number
  enabled: boolean
  overridden: boolean
  terminal: boolean
}

export interface ProjectRole {
  templateId: string
  position?: number
  enabled: boolean
  modelOverride?: string | null
  argsOverride?: string[] | null
}

export interface Project {
  id: string
  path: string
  name: string
  baseBranch: string
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
  completedAt?: string
  activeMs: number
  reworkCount: number
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
}

export interface SwarmStatus {
  running: boolean
  roles: RoleStatus[]
}

export interface Approval {
  id: string
  messageId: string
  state: string
  taskName?: string
  fromRole?: string
  createdAt: string
}

export interface Clarification {
  id: string
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
  createRole: (r: Partial<RoleTemplate>) =>
    call<RoleTemplate>('/roles', { method: 'POST', body: JSON.stringify(r) }),
  updateRole: (r: RoleTemplate) =>
    call<RoleTemplate>(`/roles/${r.id}`, { method: 'PUT', body: JSON.stringify(r) }),
  deleteRole: (id: string) => call<void>(`/roles/${id}`, { method: 'DELETE' }),

  projects: () => call<Project[]>('/projects'),
  createProject: (path: string, baseBranch: string) =>
    call<Project>('/projects', { method: 'POST', body: JSON.stringify({ path, baseBranch }) }),
  deleteProject: (id: string) => call<void>(`/projects/${id}`, { method: 'DELETE' }),

  team: (id: string) => call<ResolvedRole[]>(`/projects/${id}/team`),
  setTeam: (id: string, roles: ProjectRole[]) =>
    call<ResolvedRole[]>(`/projects/${id}/team`, { method: 'PUT', body: JSON.stringify(roles) }),

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
export type StreamState = 'connecting' | 'live' | 'reconnecting'

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
  },
  opts: { role?: string; limit?: number } = {},
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
        JSON.stringify({ type: 'subscribe', after, role: opts.role, limit: opts.limit }),
      )
    }

    ws.onmessage = (raw) => {
      let frame: { type: string; event?: ActivityEvent; replayed?: number }
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

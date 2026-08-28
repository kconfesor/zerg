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
  /** How hard the harness reasons, in that harness's own word for it: claude
   *  spends it as --effort, pi as --thinking. Empty leaves its default. */
  thinking: string
  receive: 'task' | 'batch'
  batchMaxItems: number
  batchMaxAgeSec: number
  prompt: string
  gate: 'none' | 'approval'
  /** A role that ends a pipeline wherever it appears, like a reviewer or a
   *  cleaner. Added to a team it goes last, and roles added after it go in
   *  front, so the pipeline keeps delivering through the same role as it
   *  grows. Not the same as ResolvedRole.terminal, which is the role that is
   *  finishing this particular pipeline. */
  finisher: boolean
  builtin: boolean
}

export interface RoleOverrides {
  harnessOverride?: string | null
  modelOverride?: string | null
  /** null inherits; [] explicitly removes every argument. */
  argsOverride: string[] | null
  thinkingOverride?: string | null
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

/** One role's settings for one project, over whatever its team says. Shape is
 *  the team's: a project that wants its own runs a team of its own. */
export interface ProjectRole extends RoleOverrides {
  templateId: string
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
  /** The project this team belongs to, or null when every project can use it.
   *  A team carries prompts, models and arguments chosen for one repository as
   *  often as not, and those have no business in another repository's picker. */
  projectId: string | null
  roles: TeamPresetRole[]
  createdAt: string
  updatedAt: string
}

export interface ProjectTeam {
  presetId: string | null
  roles: ResolvedRole[]
}

export interface ProjectTeamUpdate {
  presetId: string | null
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
  /** The team this project runs. */
  teamPresetId?: string
  /** What answers questions in Chat. Empty inherits the terminal role. */
  chatHarness?: string
  chatModel?: string
  /** A path inside the repository to its own logo or favicon, or empty for the
   *  mark derived from the project instead. */
  icon?: string
  createdAt: string
  lastOpenedAt?: string
}

/** One subdirectory the folder picker can descend into or select. */
export interface BrowseEntry {
  name: string
  path: string
  /** Holds a .git, so it is a repository the create endpoint will accept. */
  isRepo: boolean
}

/** A directory listing from the folder picker. */
export interface BrowseDir {
  path: string
  /** The directory above, or empty at the filesystem root. */
  parent: string
  entries: BrowseEntry[]
  /** True when the listing was cut short, so the picker can say so rather than
   *  hiding the folder somebody is looking for. */
  truncated?: boolean
}

/**
 * One role's spend over a window, with what it ran on.
 *
 * The four token classes are separate because they are priced roughly 50x
 * apart — a single "input" figure misstates the bill by an order of magnitude
 * and hides the one lever anybody controls.
 */
export interface RoleUsage {
  role: string
  turns: number
  inputTokens: number
  cacheWriteTokens: number
  cacheReadTokens: number
  outputTokens: number
  costUsd: number
  /** How much of costUsd is an estimate at API rates rather than a bill. */
  subscriptionTurns: number
  /** Turns whose harness reported no cost: real tokens, unknown price. */
  unpricedTurns: number
  /** Every model this role ran on in the window, busiest first. */
  models: string[]
  providers: string[]
  /** Distinct cards this spend is attributed to; 0 for chat, which has none. */
  tasks: number
  lastAt: string
}

/**
 * A role whose cache hit rate has fallen against its own past.
 *
 * The regression nothing else reports: caching is a prefix match, so one
 * changed byte in the composed system prompt invalidates everything after it —
 * silently, with the bill as the only symptom.
 */
export interface CacheFlag {
  role: string
  /** Hit rates, 0..1, over the newest turns and everything before them. */
  recent: number
  trailing: number
  recentTurns: number
  trailingTurns: number
  /** When the role's library entry last changed, if recent enough to be the
   *  likely cause. Any edit moves it, not the prompt specifically. */
  editedAt?: string
}

export type SpendRange = 'session' | '24h' | '7d' | '30d' | 'all'

export interface Spend {
  range: SpendRange
  /** When the window opens; absent means all of recorded history. */
  from?: string
  /** False when "this session" resolved to nothing because none has run. */
  sessionStarted: boolean
  roles: RoleUsage[]
  providers: UsageTotal[]
  models: UsageTotal[]
  flags: CacheFlag[]
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
  /** When a person parked it. `rejected` covers both a role's verdict and a
   *  person's decision — this is what tells them apart. */
  stoppedAt?: string
  activeMs: number
  reworkCount: number
  /** Total tokens and cost across every role and every lap. */
  tokens: number
  costUsd: number
  /** The most recent thing an agent did on this card. */
  doing?: string
  /** Put away by a person. Finished work that is still finished. */
  hidden?: boolean
  /** Kept past the retention window: events are swept because they are the
   *  expensive tier, and the card worth reading in six months is usually the
   *  one that went wrong. */
  pinned?: boolean
  /** What happened to the work when the last role finished: it was merged, a
   *  pull request was opened, or it was left on its branch. Empty on a card
   *  still being worked, and on one that ended before the daemon recorded
   *  this. `outcomeRef` is the commit or the pull request's URL. */
  outcome?: 'merged' | 'pr' | 'branch'
  outcomeRef?: string
}

/** One worked task as the history screen reads it. */
export interface HistoryEntry extends Task {
  /** The roles that sent a handoff on this task, in the order they first did. */
  roles: string[]
  /** Whether this card's events are still here. Asked of the table rather than
   *  worked out from the retention window, which a sweep that has not run yet
   *  makes wrong. */
  hasTranscript: boolean
}

export interface HistoryPage {
  entries: HistoryEntry[]
  /** A position to ask for the next page with, empty on the last one. */
  next: string
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

/**
 * A preset always arrives with a roles array.
 *
 * A team with no roles is an ordinary state — unchecking the last role produces
 * it — and a Go nil slice marshals as `"roles": null`. The view dereferences
 * roles in a dozen places, so one such preset anywhere in the list threw a
 * TypeError and took the whole Team page down, for every project. The daemon no
 * longer emits null; this is the second half of that, for a cached bundle
 * talking to an older daemon or the reverse.
 */
function withRoles(p: TeamPreset): TeamPreset {
  return { ...p, roles: p.roles ?? [] }
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
  /** The reasoning levels this harness accepts, weakest first. Empty means it
   *  has no such control, and the field is not offered for it. */
  thinking: (harness: string) => call<string[]>(`/harnesses/${harness}/thinking`),

  roles: () => call<RoleTemplate[]>('/roles'),
  /** The teams a project may use: the shared ones and its own. Without an id
   *  this is every team, which is not a list to show under one project. */
  teamPresets: async (projectId?: string) =>
    (
      await call<TeamPreset[]>(
        `/team-presets${projectId ? `?project=${encodeURIComponent(projectId)}` : ''}`,
      )
    ).map(withRoles),
  createTeamPreset: (p: Pick<TeamPreset, 'name' | 'roles'> & { projectId?: string | null }) =>
    call<TeamPreset>('/team-presets', { method: 'POST', body: JSON.stringify(p) }).then(withRoles),
  updateTeamPreset: (p: TeamPreset) =>
    call<TeamPreset>(`/team-presets/${p.id}`, { method: 'PUT', body: JSON.stringify(p) }).then(withRoles),
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

  /**
   * The subdirectories of one directory on the daemon's machine, for picking a
   * project without typing its path. Empty path starts at the operator's home.
   * The daemon lists it because the repositories are on its disk, not the
   * browser's.
   */
  browse: (path: string) =>
    call<BrowseDir>(`/browse?path=${encodeURIComponent(path)}`),
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
  /** Worked tasks, newest first. `before` is the `next` of the page before. */
  history: (
    id: string,
    opts: { before?: string; outcome?: string; role?: string; q?: string; limit?: number } = {},
  ) => {
    const query = new URLSearchParams()
    for (const [key, value] of Object.entries(opts)) {
      if (value) query.set(key, String(value))
    }
    const suffix = query.toString()
    return call<HistoryPage>(`/projects/${id}/history${suffix ? '?' + suffix : ''}`)
  },
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
  /**
   * One step's transcript: what a role did while it held the work.
   *
   * Bounded by that step's window rather than the card's whole history, which
   * is the question being asked. Events are the tier that ages out, so an
   * empty answer is ordinary and means the transcript is no longer kept.
   */
  taskEvents: (
    id: string,
    opts: { role?: string; from?: string; until?: string; limit?: number } = {},
  ) => {
    const query = new URLSearchParams()
    for (const [key, value] of Object.entries(opts)) {
      if (value) query.set(key, String(value))
    }
    const suffix = query.toString()
    return call<{ events: ActivityEvent[]; truncated: boolean }>(
      `/tasks/${id}/events${suffix ? '?' + suffix : ''}`,
    )
  },
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
  /** Keep this card's transcript past the retention window, or let it go. */
  setTaskPinned: (id: string, pinned: boolean) =>
    call<Task>(`/tasks/${id}/pinned`, { method: 'PUT', body: JSON.stringify({ pinned }) }),
  /** The conversation about a card's code. */
  review: (taskId: string) => call<ReviewThread[]>(`/tasks/${taskId}/review`),
  /** Start a thread on a line. Line 0 is the file as a whole. */
  openReviewThread: (
    taskId: string,
    body: { approvalId?: string; commitSha?: string; file?: string; line: number; body: string },
  ) => call<ReviewThread>(`/tasks/${taskId}/review`, { method: 'POST', body: JSON.stringify(body) }),
  addReviewComment: (threadId: string, body: string, author?: string) =>
    call<ReviewThread>(`/review-threads/${threadId}/comments`, {
      method: 'POST',
      body: JSON.stringify({ body, author }),
    }),
  /**
   * Put a question to the project's agent about a hunk, with the code attached.
   *
   * The answer lands in the thread as a comment, written when the agent
   * finishes: the reply is not waited for here, because an agent turn outlives
   * a request held open on a phone. The thread comes back at once so the
   * question is on screen while it is being answered.
   */
  askAboutChange: (
    taskId: string,
    body: {
      threadId?: string
      approvalId?: string
      commitSha?: string
      base?: string
      file?: string
      line?: number
      hunk?: string
      question: string
    },
  ) => call<ReviewThread>(`/tasks/${taskId}/review/ask`, { method: 'POST', body: JSON.stringify(body) }),
  /** Turn a question into a remark: what was learned has to be dealt with. */
  raiseReviewThread: (threadId: string) =>
    call<ReviewThread>(`/review-threads/${threadId}/raise`, { method: 'POST', body: '{}' }),
  /** Settle a thread, or open it again. A person only: the gate reads this. */
  resolveReviewThread: (threadId: string, resolved: boolean) =>
    call<ReviewThread>(`/review-threads/${threadId}/resolved`, {
      method: 'PUT',
      body: JSON.stringify({ resolved }),
    }),
  approvalDiff: (id: string) =>
    call<{ files: ChangedFile[]; range: boolean; base: string; seen: string[] }>(
      `/approvals/${id}/diff`,
    ),
  /** One file of a change, for the ones a large diff left unread. */
  approvalFile: (approvalId: string, path: string) =>
    call<ChangedFile>(`/approvals/${approvalId}/file?path=${encodeURIComponent(path)}`),
  /** The agent's orientation for a change: what it is for, what each file
   *  contributes, where to start. It describes; it never decides. */
  approvalGuide: (id: string) => call<ReviewGuide>(`/approvals/${id}/guide`),
  requestGuide: (id: string) =>
    call<{ status: string }>(`/approvals/${id}/guide`, { method: 'POST' }),
  /** Record where a reader got to, so a review interrupted on one device
   *  resumes on the next. */
  markFileSeen: (approvalId: string, file: string, seen: boolean) =>
    call<{ seen: string[] }>(`/approvals/${approvalId}/seen`, {
      method: 'PUT',
      body: JSON.stringify({ file, seen }),
    }),
  /** Whether this work still merges into the base, and how far the base has
   *  moved since it was written. The merge happens in memory, so asking costs
   *  nothing and leaves nothing behind. */
  approvalMergeable: (id: string) => call<Mergeable>(`/approvals/${id}/mergeable`),
  spend: (id: string, range: SpendRange) =>
    call<Spend>(`/projects/${id}/spend?range=${range}`),

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

/**
 * One step of a task's history: who handed what to whom, what they said, and
 * the numbers belonging to that step rather than to the whole card.
 */
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
  /** When the role took the work. Absent for a step with no lease behind it,
   *  which is what the operator's own first message is. */
  startedAt?: string
  /** The span this step's turns were summed over, so anything else reading the
   *  step reads the same one. `windowEnd` is exclusive and absent on a role's
   *  last step, which runs to the end of the card. */
  windowStart?: string
  windowEnd?: string
  /** How long that role held it. Zero when there is nothing to measure from. */
  durationMs: number
  tokens: number
  costUsd: number
  /** The approval this handoff waited on, if it had one. */
  gate?: TaskGate
  /** Questions the role raised while it held the work. */
  clarifications?: Clarification[]
}

/** An approval as the trail reads it: what was decided, and how long the work
 *  sat waiting for a person to decide it. */
export interface TaskGate {
  id: string
  state: 'pending' | 'approved' | 'rejected' | string
  note?: string
  createdAt: string
  decidedAt?: string
  /** How long it was pending. This is where wall time goes when active time is
   *  short, and no per-task total shows it. */
  waitedMs: number
}

export interface TaskDetail {
  task: Task
  history: TaskStep[]
  usage: UsageTotal
}

/** One remark on a card's code, and everything said about it. */
export interface ReviewThread {
  id: string
  /** A remark holds the gate until it is settled; a question never does. */
  kind: 'remark' | 'question'
  projectId: string
  taskId: string
  approvalId?: string
  commitSha?: string
  file?: string
  /** 0 means the file as a whole rather than a line within it. */
  line: number
  state: 'open' | 'resolved'
  createdAt: string
  resolvedAt?: string
  comments: ReviewComment[]
}

export interface ReviewComment {
  id: string
  threadId: string
  /** The operator, a role, or the agent that answered a question about a hunk. */
  author: string
  body: string
  createdAt: string
}

/** Whether approving would land the work, and what stands in the way. */
export interface Mergeable {
  clean: boolean
  /** The paths git could not merge, when it could not. */
  conflicts?: string[]
  /** Commits the base has taken since this work left it. A diff read against a
   *  base that has moved is not a diff of what will land. */
  baseAhead: number
  /** The landing fast-forwards and this commit is no longer on top of the base,
   *  so it will not land however clean the merge would be. A rebase fixes it,
   *  not a resolution. */
  diverged?: boolean
}

/** The agent's orientation for one approval. */
export interface ReviewGuide {
  approvalId: string
  commitSha: string
  body: string
  createdAt: string
}

/** One file a commit touched, with both its content and its diff. */
export interface ChangedFile {
  path: string
  /** git's letter: A added, M modified, D deleted. */
  status: 'A' | 'M' | 'D' | string
  content?: string
  diff?: string
  /** Line counts, which git reports without reading the file. */
  added: number
  removed: number
  /** A file git will not diff: an image, a font, a compiled thing. Named and
   *  counted rather than fetched. */
  binary?: boolean
  /** Past the byte cap. Said rather than left empty, since a file with no diff
   *  is otherwise indistinguishable from one that did not change. */
  tooLarge?: boolean
  /** Listed but not read, because the change is large. Fetched when opened. */
  deferred?: boolean
}

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

  sharedInstructions: () => call<{ text: string }>('/settings/shared-instructions'),
  setSharedInstructions: (text: string) =>
    call<{ text: string }>('/settings/shared-instructions', {
      method: 'PUT',
      body: JSON.stringify({ text }),
    }),
}

import type {
  BrowseResult,
  Job,
  JobPayload,
  MountErrorKind,
  PromptAction,
  Run,
  RunDetail,
  RunEvent,
  RunPlan,
  SessionState,
  Target,
  TargetPayload,
  TestResult,
} from './types'

/** The single error envelope the API uses — internal/api/api.go, errorBody. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly detail?: string
  /** Set only on mount failures, which is what makes a target error legible. */
  readonly kind?: MountErrorKind

  constructor(status: number, code: string, message: string, detail?: string, kind?: MountErrorKind) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.detail = detail
    this.kind = kind
  }

  get isUnauthenticated(): boolean {
    return this.status === 401 && this.code === 'unauthenticated'
  }
}

/** Notified whenever a request comes back 401, so the app can drop to the
 *  login screen from anywhere without every caller handling it. */
type UnauthorizedHandler = () => void
let onUnauthorized: UnauthorizedHandler = () => {}
export function setUnauthorizedHandler(fn: UnauthorizedHandler): void {
  onUnauthorized = fn
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    // Same-origin: no CORS headers are set server-side, so the SPA is either
    // served by the Go process or reached through the dev proxy.
    credentials: 'same-origin',
    headers: body === undefined ? {} : { 'content-type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })

  if (!res.ok) {
    const err = await toApiError(res)
    // The session guard rejects with 401 unauthenticated; a wrong password on
    // the login form is also 401 but carries invalid_password, and must not
    // bounce the user out of a screen they are already on.
    if (err.isUnauthenticated) onUnauthorized()
    throw err
  }

  // 204 on DELETE, and logout returns an empty body on some paths.
  if (res.status === 204 || res.headers.get('content-length') === '0') {
    return undefined as T
  }
  return (await res.json()) as T
}

async function toApiError(res: Response): Promise<ApiError> {
  try {
    const body = (await res.json()) as {
      error?: { code?: string; message?: string; detail?: string; kind?: MountErrorKind }
    }
    const e = body.error
    if (e?.message) {
      return new ApiError(res.status, e.code ?? 'unknown', e.message, e.detail, e.kind)
    }
  } catch {
    // Not JSON — fall through to a status-based message.
  }
  return new ApiError(res.status, 'unknown', `Request failed with status ${res.status}.`)
}

const get = <T,>(path: string) => request<T>('GET', path)
const post = <T,>(path: string, body?: unknown) => request<T>('POST', path, body)
const patch = <T,>(path: string, body?: unknown) => request<T>('PATCH', path, body)
const del = (path: string) => request<void>('DELETE', path)

function query(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') q.set(k, String(v))
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

export const api = {
  session: {
    get: () => get<SessionState>('/api/auth/session'),
    setup: (password: string) => post<SessionState>('/api/auth/setup', { password }),
    login: (password: string) => post<SessionState>('/api/auth/login', { password }),
    logout: () => post<SessionState>('/api/auth/logout'),
  },

  targets: {
    list: () => get<{ targets: Target[] }>('/api/targets').then((r) => r.targets ?? []),
    get: (id: string) => get<Target>(`/api/targets/${id}`),
    create: (payload: TargetPayload) => post<Target>('/api/targets', payload),
    update: (id: string, payload: TargetPayload) => patch<Target>(`/api/targets/${id}`, payload),
    remove: (id: string) => del(`/api/targets/${id}`),
    test: (id: string) => post<TestResult>(`/api/targets/${id}/test`),
  },

  jobs: {
    list: () => get<{ jobs: Job[] }>('/api/jobs').then((r) => r.jobs ?? []),
    get: (id: string) => get<Job>(`/api/jobs/${id}`),
    create: (payload: JobPayload) => post<Job>('/api/jobs', payload),
    /** Full replace — send the complete job, including destinations and filters. */
    update: (id: string, payload: JobPayload) => patch<Job>(`/api/jobs/${id}`, payload),
    remove: (id: string) => del(`/api/jobs/${id}`),
    run: (id: string, preview = false) => post<Run>(`/api/jobs/${id}/run`, { preview }),
    confirm: (id: string) => post<{ status: string; run_id: string }>(`/api/jobs/${id}/confirm`),
  },

  runs: {
    list: (params: { job_id?: string; status?: string; limit?: number } = {}) =>
      get<{ runs: Run[] }>(`/api/runs${query(params)}`).then((r) => r.runs ?? []),
    get: (id: string) => get<RunDetail>(`/api/runs/${id}`),
    events: (id: string, params: { level?: string; dest?: string; limit?: number; offset?: number } = {}) =>
      get<{ events: RunEvent[] }>(`/api/runs/${id}/events${query(params)}`).then((r) => r.events ?? []),
    cancel: (id: string) => post<{ status: string }>(`/api/runs/${id}/cancel`),
    /** The individual paths a run intends to act on. Only available while the
     *  runner still holds the run — a finished run 409s with no_plan. */
    plan: (id: string) => get<RunPlan>(`/api/runs/${id}/plan`),
    prompt: (id: string, destTargetId: string, action: PromptAction) =>
      post<{ status: string }>(`/api/runs/${id}/prompt`, {
        dest_target_id: destTargetId,
        action,
      }),
  },

  logs: (params: {
    job_id?: string
    run_id?: string
    level?: string
    since?: string
    limit?: number
    offset?: number
  } = {}) => get<{ events: RunEvent[] }>(`/api/logs${query(params)}`).then((r) => r.events ?? []),

  browse: (targetId: string, path = '') =>
    get<BrowseResult>(`/api/browse${query({ target_id: targetId, path })}`),
}

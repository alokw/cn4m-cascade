// Hand-written mirrors of the Go structs. The API was frozen at the end of
// Phase 4a; these types are the contract, and every literal union below is
// copied from a Go const block, cited so the two can be diffed by hand.

/** internal/store/runs.go — RunStatus. */
export type RunStatus =
  | 'running'
  | 'awaiting_confirmation'
  | 'success'
  | 'partial'
  | 'failed'
  | 'cancelled'

/** awaiting_confirmation is neither running nor terminal — Run.Terminal(). */
export const TERMINAL_RUN_STATUSES: readonly RunStatus[] = [
  'success',
  'partial',
  'failed',
  'cancelled',
]

export function isTerminal(status: RunStatus): boolean {
  return TERMINAL_RUN_STATUSES.includes(status)
}

/** internal/store/runs.go — DestStatus. */
export type DestStatus =
  | 'pending'
  | 'running'
  | 'success'
  | 'failed'
  | 'cancelled'
  | 'skipped_unavailable'
  | 'awaiting_prompt'

/** internal/engine/progress.go — Phase. */
export type Phase = 'scanning' | 'planning' | 'copying' | 'deleting' | 'done'

/** internal/mountmgr/errors.go — the kind attached to a mount failure. */
export type MountErrorKind =
  | 'auth'
  | 'share_missing'
  | 'unreachable'
  | 'timeout'
  | 'dialect'
  | 'permission'
  | 'unknown'

export type HealthState = 'unknown' | 'healthy' | 'unhealthy'

export interface Health {
  state: HealthState
  /** Always present. See isZeroTime — Go's omitempty does nothing for
   *  time.Time, so an unset timestamp arrives as "0001-01-01T00:00:00Z"
   *  rather than being omitted. */
  checked_at: string
  message?: string
}

export type TargetType = 'smb' | 'local'

export interface Target {
  id: string
  name: string
  type: TargetType
  host?: string
  share?: string
  port?: number
  username?: string
  domain?: string
  mount_opts_override?: string
  multichannel: boolean
  negotiated_vers?: string
  local_path?: string
  subpath?: string
  created_at: string
  updated_at: string
  has_password: boolean
  health: Health
}

/** Every field optional: the Go payload is all-pointers, so PATCH is a true
 *  partial merge and an omitted password keeps the stored one. Sending an
 *  empty string would erase it. */
export interface TargetPayload {
  name?: string
  type?: TargetType
  host?: string
  share?: string
  subpath?: string
  local_path?: string
  port?: number
  username?: string
  password?: string
  domain?: string
  mount_opts_override?: string
  multichannel?: boolean
}

export interface DirEntry {
  name: string
  is_dir: boolean
  size: number
  mod_time: string
}

/** internal/storage/storage.go — TestResult. */
export interface TestResult {
  ok: boolean
  root: string
  negotiated_vers?: string
  capacity_bytes?: number
  free_bytes?: number
  entries: DirEntry[] | null
  truncated: boolean
  elapsed_ms: number
}

export type SyncMode = 'mirror' | 'update'
export type CompareMethod = 'fast'
export type OnError = 'skip' | 'abort'
export type DeletePolicy = 'skip_deletes' | 'proceed'
export type UnavailablePolicy = 'skip' | 'abort' | 'prompt'
export type PromptFallback = 'skip' | 'abort'

export type FilterScope = 'job' | 'target'
export type FilterDirection = 'include' | 'exclude'
export type FilterSource = 'inline' | 'listfile' | 'jsonfile'
export type FilterOnError = 'fail_run' | 'ignore_rule'

export interface FilterRule {
  id?: string
  job_id?: string
  scope: FilterScope
  scope_target_id?: string
  direction: FilterDirection
  source: FilterSource
  patterns?: string[]
  file_path?: string
  json_key?: string
  case_sensitive: boolean
  on_error: FilterOnError
  position?: number
}

export interface JobDestination {
  id?: string
  job_id?: string
  dest_target_id: string
  dest_subpath?: string
  /** Server-assigned from array order. internal/api's destPayload does not
   *  read it, so sending one is ignored — order the array instead. */
  position?: number
}

export interface Job {
  id: string
  name: string
  source_target_id: string
  source_subpath?: string
  mode: SyncMode
  compare: CompareMethod
  compare_tolerance_sec?: number
  ignore_dst_hour: boolean
  workers: number
  log_every_file: boolean
  on_error: OnError
  delete_policy: DeletePolicy
  unavailable_policy: UnavailablePolicy
  prompt_timeout_sec: number
  prompt_fallback: PromptFallback
  parallel_destinations: boolean
  destinations: JobDestination[]
  filters?: FilterRule[]
  created_at: string
  updated_at: string
}

/** PATCH /api/jobs/{id} is a full replace, not a merge: store.UpdateJob
 *  deletes and reinserts every destination and filter row.
 *
 *  `filters` is deliberately **required** here even though it is optional on
 *  Job. Omitting it does not leave the existing rules alone — it deletes every
 *  one of them, and an exclude rule that vanishes is a subtree that gets
 *  copied, or in mirror mode deleted, on the next run. The type is what stops
 *  an editor that never opened the Filters tab from wiping them. */
export type JobPayload = Omit<Job, 'id' | 'created_at' | 'updated_at' | 'filters'> & {
  filters: FilterRule[]
}

export interface RunDestination {
  run_id: string
  dest_target_id: string
  status: DestStatus
  files_total: number
  files_done: number
  files_deleted: number
  bytes_total: number
  bytes_done: number
  started_at?: string
  finished_at?: string
  error_summary?: string
}

export interface Run {
  id: string
  job_id: string
  trigger: 'manual'
  status: RunStatus
  started_at: string
  finished_at?: string
  files_scanned: number
  bytes_total: number
  error_summary?: string
  destinations: RunDestination[]
}

/** internal/engine/progress.go — FileInFlight. */
export interface FileInFlight {
  relpath: string
  bytes_done: number
  bytes_total: number
  percent: number
  throughput_bps: number
  eta_sec: number
}

/** engine.ETAUnknown. An ETA of -1 means "not computable yet", not "instant". */
export const ETA_UNKNOWN = -1

export interface DestSnapshot {
  dest_target_id: string
  status: DestStatus
  /** Meaningful only while status is 'awaiting_prompt'. Always present in the
   *  payload — guard with isZeroTime, not with a truthiness check, or you get
   *  a countdown from the year 1. */
  prompt_deadline: string
  prompt_reason?: string
  // engine.Snapshot, embedded and therefore flattened into this object.
  phase: Phase
  files_total: number
  files_done: number
  files_deleted: number
  files_failed: number
  bytes_total: number
  bytes_done: number
  files_found: number
  dirs_found: number
  throughput_bps: number
  eta_sec: number
  elapsed_sec: number
  /** null — not absent — whenever nothing is in flight: the Go field is built
   *  by append from a nil slice and carries no omitempty. */
  in_flight: FileInFlight[] | null
}

/** internal/runner/progress.go — DestPlan. What a preview asks you to confirm. */
export interface DestPlan {
  dest_target_id: string
  mkdirs: number
  copies: number
  deletes: number
  rmdirs: number
  copy_bytes: number
  /** Removals that clear something of the wrong type out of the way. NOT
   *  included in `deletes`, which counts only the trailing delete pass. This
   *  is what the replaces list checks itself against. */
  replaces: number
  /** Copies that replace an existing destination file. An overwrite destroys
   *  the destination's version as permanently as a delete. */
  overwrites: number
  /** Distinct from "nothing to delete": the engine refused. Always surface it. */
  deletions_blocked?: boolean
  blocked_reason?: string
  /** How many removals the guard discarded. The actions themselves are gone,
   *  so this is the only record of the refusal's scale — "refusing to delete
   *  412 files" rather than a bare "deletions blocked". */
  withheld_deletes?: number
  withheld_rmdirs?: number
  conflicts?: string[]
}

/** One path a run intends to act on — GET /api/runs/{id}/plan. */
export interface PlannedAction {
  relpath: string
  size?: number
  reason?: string
  overwrite?: boolean
}

/** The per-path detail behind a DestPlan's counts. Served by its own endpoint
 *  rather than on the progress payload, which is broadcast every second. */
export interface DestActions {
  dest_target_id: string
  mkdirs: string[]
  copies: PlannedAction[]
  /** The trailing delete pass only. */
  deletes: PlannedAction[]
  rmdirs: string[]
  /** Removals that clear something of the wrong type out of the way of a copy
   *  or mkdir. They destroy data too, so they are shown — but separately, as
   *  DestPlan.deletes deliberately does not count them. */
  replaces: PlannedAction[]
  /** At least one list was capped; the true totals are on the DestPlan. */
  truncated: boolean
}

export interface RunPlan {
  destinations: DestActions[]
}

export interface RunSnapshot {
  phase: Phase
  files_total: number
  files_done: number
  files_deleted: number
  files_failed: number
  bytes_total: number
  bytes_done: number
  throughput_bps: number
  eta_sec: number
  elapsed_sec: number
  pending_destinations: number
  estimated_pending_bytes: number
  /** Meaningful only while the run is awaiting_confirmation. Always present —
   *  guard with isZeroTime. */
  confirm_deadline: string
  destinations: DestSnapshot[]
  in_flight: FileInFlight[] | null
  plans?: DestPlan[]
}

export type LogLevel = 'info' | 'warn' | 'error'

export interface RunEvent {
  id: string
  run_id: string
  ts: string
  level: LogLevel
  dest_target_id?: string
  relpath?: string
  message: string
}

export interface RunDetail extends Run {
  /** Present only while the runner still holds the run in memory. The DB rows
   *  lag ~1s behind, so this is authoritative while a run is in flight. */
  progress?: RunSnapshot
  event_counts?: Record<LogLevel, number>
}

export interface SessionState {
  authenticated: boolean
  setup_required: boolean
}

export interface BrowseEntry {
  name: string
  path: string
  is_dir: boolean
  size?: number
}

export interface BrowseResult {
  target_id: string
  path: string
  parent?: string
  entries: BrowseEntry[]
  /** The directory held more than the server's cap; entries is the first page.
   *  Show it, or a partial listing reads as the whole directory. */
  truncated: boolean
  total: number
}

export type PromptAction = 'skip' | 'retry' | 'abort'

/** internal/api/hub.go — the three event names the WS feed emits. */
export type WsEventName = 'run_progress' | 'run_finished' | 'target_unavailable_prompt'

export interface WsEvent {
  event: WsEventName
  ts: string
  run_id?: string
  job_id?: string
  run?: Run
  /** Absent on run_finished: the runner handle is gone by then. */
  progress?: RunSnapshot
}

/** Go serialises an unset time.Time as year 1 rather than omitting it, because
 *  encoding/json's omitempty has no effect on struct types. Every optional
 *  timestamp from this API therefore arrives as a string that is present but
 *  meaningless, and a truthiness check on it is always true. */
export function isZeroTime(iso: string | undefined): boolean {
  return !iso || iso.startsWith('0001-01-01')
}

/** How a target reads in a list: an SMB share as //host/share, a local target
 *  as its path. Mirrors store.Target.UNCPath/Describe. */
export function targetRoot(t: Target): string {
  const base = t.type === 'smb' ? `//${t.host ?? ''}/${t.share ?? ''}` : (t.local_path ?? '')
  return t.subpath ? `${base}/${t.subpath}` : base
}

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ApiError, api } from '../api/client'
import type {
  DestSnapshot,
  DestStatus,
  FileInFlight,
  LogLevel,
  RunDetail as RunDetailPayload,
  RunEvent,
  RunStatus,
  Target,
} from '../api/types'
import { isTerminal } from '../api/types'
import { Bar } from '../components/Bar'
import { ConfirmPlan } from '../components/ConfirmPlan'
import { EventList, LEVELS } from '../components/EventList'
import { PromptModal } from '../components/PromptModal'
import { bytes, duration, eta, rate, timestamp } from '../format'
import { useEvents } from '../hooks/useEvents'

export function RunDetail() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const feed = useEvents()

  // Whether this run is parked waiting to be confirmed, read at unmount.
  //
  // A preview holds the job — Run answers "already in progress" — until it is
  // confirmed or its deadline expires, which can be ten minutes of a job that
  // looks stuck for no visible reason. Leaving the page having been shown the
  // plan is a clear enough "no" to act on.
  //
  // Set only from an effect, never during render. StrictMode mounts, unmounts
  // and remounts in development; that simulated unmount happens before the
  // first fetch resolves, so this is still false and the cancel does not fire.
  const parked = useRef(false)

  const [run, setRun] = useState<RunDetailPayload | null>(null)
  const [targets, setTargets] = useState<Target[]>([])
  const [error, setError] = useState<string | null>(null)
  const [rerunning, setRerunning] = useState(false)

  const load = useCallback(async () => {
    try {
      setRun(await api.runs.get(id))
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not load the run.')
    }
  }, [id])

  useEffect(() => {
    void load()
    void api.targets.list().then(setTargets).catch(() => {})
  }, [load])

  // Re-running navigates to the new run, but this is the same route with a
  // different param, so the component is reused and `rerunning` would stay
  // true — leaving both buttons greyed out for good on the run you just
  // landed on.
  useEffect(() => {
    setRerunning(false)
  }, [id])

  // The WS feed is authoritative while the run is live: the DB rows it would
  // otherwise be read from lag about a second behind.
  const live = feed.runs.get(id)
  const status = live?.run.status ?? run?.status
  const progress = live?.progress ?? run?.progress

  // Refetch on transition to a terminal state so the final DB record — error
  // summary, finished_at — replaces the last in-memory snapshot.
  useEffect(() => {
    if (status && isTerminal(status)) void load()
  }, [status, load])

  useEffect(() => {
    parked.current = status === 'awaiting_confirmation'
  }, [status])

  // Starts the same job again and follows the new run, so a failed run does
  // not have to be chased back through the jobs list.
  const rerun = useCallback(
    async (preview: boolean) => {
      if (!run) return
      setRerunning(true)
      setError(null)
      try {
        const next = await api.jobs.run(run.job_id, preview)
        navigate(`/runs/${next.id}`)
      } catch (err) {
        setError(
          err instanceof ApiError ? err.message : 'Could not start the run again.',
        )
        setRerunning(false)
      }
    },
    [run, navigate],
  )

  useEffect(() => {
    return () => {
      if (parked.current) {
        // Best effort: the run cancels itself on its deadline anyway, so a
        // failure here costs a wait rather than a stuck job.
        void api.runs.cancel(id).catch(() => {})
      }
    }
  }, [id])

  const nameFor = useCallback(
    (targetID: string) => targets.find((t) => t.id === targetID)?.name ?? targetID,
    [targets],
  )

  // A finished run never has a live prompt. useEvents deliberately retains the
  // last snapshot after run_finished, so without the terminal check a final
  // frame that still showed awaiting_prompt would leave a modal — which has no
  // dismiss control — stranded over a completed run.
  const prompting = useMemo(
    () =>
      status && isTerminal(status)
        ? undefined
        : progress?.destinations?.find((d) => d.status === 'awaiting_prompt'),
    [progress, status],
  )

  if (error) return <p className="error">{error}</p>
  if (!run) return <p className="muted">Loading…</p>

  const finished = !!status && isTerminal(status)
  const dests = progress?.destinations ?? []

  // Once the run is over the database is authoritative, not the last live
  // snapshot. useEvents deliberately retains that snapshot so a finished run
  // still shows its final numbers — but the snapshot was taken *before* the
  // run resolved, so a destination that was parked on a prompt when the
  // fallback fired stays rendered as "awaiting_prompt" over a run that has
  // already ended, contradicting the summary right above it.
  const panels = finished
    ? run.destinations.map((d) => ({
        dest_target_id: d.dest_target_id,
        status: d.status,
        files_done: d.files_done,
        files_total: d.files_total,
        files_deleted: d.files_deleted,
        bytes_done: d.bytes_done,
        bytes_total: d.bytes_total,
        error_summary: d.error_summary,
      }))
    : dests.map((d) => ({
        dest_target_id: d.dest_target_id,
        status: d.status,
        files_done: d.files_done,
        files_total: d.files_total,
        files_deleted: d.files_deleted,
        bytes_done: d.bytes_done,
        bytes_total: d.bytes_total,
        // Throughput, ETA and in-flight files describe work happening now, so
        // they are live-only. Carrying them past the end would show a rate for
        // a run that stopped.
        throughput_bps: d.throughput_bps,
        eta_sec: d.eta_sec,
        files_failed: d.files_failed,
        in_flight: d.in_flight,
      }))

  return (
    <section>
      <div className="page-head">
        <h1>Run</h1>
        <div className="actions">
          {status && isTerminal(status) && (
            <>
              <button className="link" disabled={rerunning} onClick={() => void rerun(true)}>
                Preview again
              </button>
              <button disabled={rerunning} onClick={() => void rerun(false)}>
                {rerunning ? 'Starting…' : 'Run again'}
              </button>
            </>
          )}
          {status && !isTerminal(status) && (
            <button className="link danger" onClick={() => void api.runs.cancel(id).catch(() => {})}>
              Cancel run
            </button>
          )}
        </div>
      </div>

      <p className="muted">
        <span className={`badge status-${status}`}>{status}</span> started {timestamp(run.started_at)}
        {run.finished_at ? ` · finished ${timestamp(run.finished_at)}` : ''}
        {progress ? ` · ${duration(progress.elapsed_sec)} elapsed` : ''}
      </p>
      {run.error_summary && (
        <p className={summaryClass(status)}>{run.error_summary}</p>
      )}

      {status === 'awaiting_confirmation' && progress?.plans && (
        <ConfirmPlan
          runID={id}
          jobID={run.job_id}
          plans={progress.plans}
          confirmDeadline={progress.confirm_deadline}
          targets={targets}
          onConfirmed={load}
        />
      )}

      {progress && status === 'running' && (
        <div className="card">
          <Bar done={progress.bytes_done} total={progress.bytes_total} />
          <p className="muted">
            {progress.files_done}/{progress.files_total} files · {bytes(progress.bytes_done)} of{' '}
            {bytes(progress.bytes_total)} · {rate(progress.throughput_bps)} · ETA{' '}
            {eta(progress.eta_sec)}
          </p>
        </div>
      )}

      {panels.map((d) => (
        <DestPanel key={d.dest_target_id} dest={d} name={nameFor(d.dest_target_id)} />
      ))}

      <EventLog
        runID={id}
        destinations={dests}
        nameFor={nameFor}
        live={!!status && !isTerminal(status)}
      />

      {prompting && (
        <PromptModal
          runID={id}
          dest={prompting}
          targetName={nameFor(prompting.dest_target_id)}
          onAnswered={load}
        />
      )}
    </section>
  )
}

/** A destination panel, live or final. The live-only fields are optional: a
 *  finished run has no throughput or ETA, and printing one would be a lie. */
interface PanelDest {
  dest_target_id: string
  status: DestStatus
  files_done: number
  files_total: number
  files_deleted: number
  bytes_done: number
  bytes_total: number
  files_failed?: number
  throughput_bps?: number
  eta_sec?: number
  in_flight?: FileInFlight[] | null
  error_summary?: string
}

function DestPanel({ dest, name }: { dest: PanelDest; name: string }) {
  const running = dest.throughput_bps !== undefined
  return (
    <div className="card dest-panel">
      <div className="target-head">
        <strong>{name}</strong>
        <span className={`badge status-${dest.status}`}>{dest.status}</span>
      </div>

      <Bar done={dest.bytes_done} total={dest.bytes_total} />
      <p className="muted">
        {dest.files_done}/{dest.files_total} files
        {dest.files_deleted > 0 ? ` · ${dest.files_deleted} deleted` : ''}
        {dest.files_failed ? ` · ${dest.files_failed} failed` : ''}
        {running ? ` · ${rate(dest.throughput_bps)} · ETA ${eta(dest.eta_sec)}` : ''}
      </p>
      {dest.error_summary && <p className="muted">{dest.error_summary}</p>}

      {/* in_flight arrives as null, not absent, whenever nothing is copying. */}
      {dest.in_flight && dest.in_flight.length > 0 && (
        <ul className="in-flight">
          {dest.in_flight.map((f) => (
            <li key={f.relpath}>
              <code>{f.relpath}</code>
              <span className="muted">
                {' '}
                {f.percent}% · {bytes(f.bytes_done)}/{bytes(f.bytes_total)} · {rate(f.throughput_bps)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

const EVENT_POLL_MS = 3000

function EventLog({
  runID,
  destinations,
  nameFor,
  live,
}: {
  runID: string
  destinations: DestSnapshot[]
  nameFor: (id: string) => string
  live: boolean
}) {
  const [events, setEvents] = useState<RunEvent[]>([])
  const [level, setLevel] = useState<LogLevel | ''>('')
  const [dest, setDest] = useState('')
  const [loading, setLoading] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setEvents(await api.runs.events(runID, { level: level || undefined, dest: dest || undefined, limit: 200 }))
    } catch {
      // The log is secondary to the run itself; a failure here should not
      // replace the whole screen with an error.
    } finally {
      setLoading(false)
    }
  }, [runID, level, dest])

  // A log that describes a run in progress has to follow it. Loading once on
  // mount meant opening the page as a run started showed an empty log for the
  // whole run — the events were there, nothing asked for them again. The
  // effect re-runs when `live` flips, which also refetches once on completion
  // so the final events land without the user pressing Refresh.
  useEffect(() => {
    void load()
    if (!live) return
    const timer = window.setInterval(() => void load(), EVENT_POLL_MS)
    return () => window.clearInterval(timer)
  }, [load, live])

  return (
    <div className="card">
      <div className="page-head">
        <h2>Log</h2>
        <div className="filters">
          <select value={level} onChange={(e) => setLevel(e.target.value as LogLevel | '')}>
            <option value="">All levels</option>
            {LEVELS.map((l) => (
              <option key={l} value={l}>
                {l}
              </option>
            ))}
          </select>
          <select value={dest} onChange={(e) => setDest(e.target.value)}>
            <option value="">All destinations</option>
            {destinations.map((d) => (
              <option key={d.dest_target_id} value={d.dest_target_id}>
                {nameFor(d.dest_target_id)}
              </option>
            ))}
          </select>
          <button className="link" onClick={() => setLevel('error')}>
            Errors only
          </button>
          <button className="link" onClick={() => void load()}>
            Refresh
          </button>
        </div>
      </div>

      <EventList events={events} loading={loading} />
    </div>
  )
}

/** The run summary is written for every outcome, not only failures, so it must
 *  not always be red — a successful run reading "1 succeeded, 0 failed" in
 *  error styling looks like something went wrong. */
function summaryClass(status: RunStatus | undefined): string {
  switch (status) {
    case 'success':
      return 'ok'
    case 'failed':
      return 'error'
    case 'partial':
    case 'cancelled':
      return 'warn'
    default:
      return 'muted'
  }
}

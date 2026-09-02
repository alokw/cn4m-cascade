import { useCallback, useEffect, useMemo, useState } from 'react'
import { useParams } from 'react-router-dom'
import { ApiError, api } from '../api/client'
import type {
  DestSnapshot,
  DestStatus,
  FileInFlight,
  LogLevel,
  RunDetail as RunDetailPayload,
  RunEvent,
  Target,
} from '../api/types'
import { isTerminal } from '../api/types'
import { ConfirmPlan } from '../components/ConfirmPlan'
import { PromptModal } from '../components/PromptModal'
import { bytes, duration, eta, percent, rate, timestamp } from '../format'
import { useEvents } from '../hooks/useEvents'

export function RunDetail() {
  const { id = '' } = useParams()
  const feed = useEvents()

  const [run, setRun] = useState<RunDetailPayload | null>(null)
  const [targets, setTargets] = useState<Target[]>([])
  const [error, setError] = useState<string | null>(null)

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
        {status && !isTerminal(status) && (
          <button className="link danger" onClick={() => void api.runs.cancel(id).catch(() => {})}>
            Cancel run
          </button>
        )}
      </div>

      <p className="muted">
        <span className={`badge status-${status}`}>{status}</span> started {timestamp(run.started_at)}
        {run.finished_at ? ` · finished ${timestamp(run.finished_at)}` : ''}
        {progress ? ` · ${duration(progress.elapsed_sec)} elapsed` : ''}
      </p>
      {run.error_summary && <p className="error">{run.error_summary}</p>}

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

function Bar({ done, total }: { done: number; total: number }) {
  const pct = percent(done, total)
  return (
    <div className="bar" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
      <div className="bar-fill" style={{ width: `${pct}%` }} />
    </div>
  )
}

const LEVELS: LogLevel[] = ['info', 'warn', 'error']

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

      {loading && <p className="muted">Loading…</p>}
      {!loading && events.length === 0 && <p className="muted">Nothing logged yet.</p>}

      <ul className="events">
        {events.map((e) => (
          <li key={e.id} className={e.level}>
            <span className="muted">{timestamp(e.ts)}</span> <span className="lvl">{e.level}</span>{' '}
            {e.relpath && <code>{e.relpath}</code>} {e.message}
          </li>
        ))}
      </ul>
    </div>
  )
}

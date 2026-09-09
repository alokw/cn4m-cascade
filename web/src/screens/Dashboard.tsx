import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ApiError, api } from '../api/client'
import type { Job, Run, RunSnapshot, Target } from '../api/types'
import { ETA_UNKNOWN, isTerminal } from '../api/types'
import { Bar } from '../components/Bar'
import { bytes, eta, rate, timestamp } from '../format'
import { useEvents } from '../hooks/useEvents'

/** One shared page of runs covers most installations; the per-job top-ups
 *  below cover the rest. 500 is the ceiling ListRuns honours — ask for more
 *  and it silently gives you 100. */
const RUN_PAGE = 500

/**
 * The front door (SPEC.md §9): every job, its last outcome, and anything
 * happening right now.
 *
 * This is the only screen someone opens without already knowing what they are
 * looking for, which makes a *stale* card worse than no card at all — a job
 * that failed last night reading "success" is how a backup goes unnoticed for
 * a week. So the live feed always wins over the fetched list, and a run
 * finishing triggers a refetch rather than leaving the card on its last frame.
 */
export function Dashboard() {
  const navigate = useNavigate()
  const feed = useEvents()

  const [jobs, setJobs] = useState<Job[]>([])
  const [targets, setTargets] = useState<Target[]>([])
  const [latest, setLatest] = useState<Map<string, Run>>(new Map())
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [starting, setStarting] = useState<string | null>(null)

  // Jobs proven to have no runs at all. Without this, every job that has never
  // run is missing from the shared page forever, so the top-up below asks
  // about it on every refresh and gets the same empty answer — forty
  // configured-but-unrun jobs would mean forty extra requests each time.
  // A job that *does* run appears in the shared page from then on, so nothing
  // is missed by never asking again.
  const neverRun = useRef<Set<string>>(new Set())

  const load = useCallback(async () => {
    try {
      const [j, t, runs] = await Promise.all([
        api.jobs.list(),
        api.targets.list(),
        api.runs.list({ limit: RUN_PAGE }),
      ])

      // ListRuns returns newest first, so the first run seen for a job is its
      // most recent one.
      const byJob = new Map<string, Run>()
      for (const r of runs) if (!byJob.has(r.job_id)) byJob.set(r.job_id, r)

      // Run listing has no offset (store.RunFilter), so a job whose runs all
      // fell off that one page is not merely truncated — it is unreachable,
      // and its card would claim it had never run. Ask for those directly.
      const missing = j.filter((job) => !byJob.has(job.id) && !neverRun.current.has(job.id))
      if (missing.length > 0) {
        const found = await Promise.all(
          missing.map((job) => api.runs.list({ job_id: job.id, limit: 1 }).catch(() => [] as Run[])),
        )
        found.forEach((rs, i) => {
          const run = rs[0]
          if (run) byJob.set(run.job_id, run)
          else {
            const job = missing[i]
            if (job) neverRun.current.add(job.id)
          }
        })
      }

      setJobs(j)
      setTargets(t)
      setLatest(byJob)
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not load the dashboard.')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // The newest live run per job, which outranks whatever the list returned.
  const liveByJob = new Map<string, { run: Run; progress?: RunSnapshot }>()
  for (const live of feed.runs.values()) {
    const held = liveByJob.get(live.run.job_id)
    if (!held || live.run.started_at >= held.run.started_at) liveByJob.set(live.run.job_id, live)
  }

  // Refetch when a run completes. Not for the completed run itself — the feed
  // already carries a fresh database row for it (hub.tick re-reads via
  // ListRuns) and the live copy outranks the fetched one just above. What this
  // catches is everything *around* it: the polling path, a job added or
  // renamed in another tab, a target renamed since the page loaded.
  //
  // The trigger is a *transition*, not the presence of finished runs. The feed
  // arrives already populated — when the socket is down its first poll merges
  // the newest fifty runs, nearly all long finished — and reacting to those
  // would refetch the whole page immediately after load() had just fetched it.
  // So only a run this page watched while it was still going counts.
  const watching = useRef<Set<string>>(new Set())
  const feedStates = [...feed.runs.values()]
    .map((r) => `${r.run.id}:${r.run.status}`)
    .sort()
    .join(',')
  useEffect(() => {
    let completed = false
    for (const live of feed.runs.values()) {
      if (!isTerminal(live.run.status)) watching.current.add(live.run.id)
      else if (watching.current.delete(live.run.id)) completed = true
    }
    if (completed) void load()
    // feed.runs is a fresh Map on every frame the hub sends; feedStates is
    // what actually changes when a run does.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [feedStates, load])

  const nameFor = (id: string) => targets.find((t) => t.id === id)?.name ?? id

  async function start(job: Job, preview: boolean) {
    setStarting(job.id)
    setError(null)
    try {
      const run = await api.jobs.run(job.id, preview)
      navigate(`/runs/${run.id}`)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not start the run.')
      setStarting(null)
    }
  }

  return (
    <section>
      <div className="page-head">
        <h1>Dashboard</h1>
        <Link className="link" to="/jobs/new">
          New job
        </Link>
      </div>

      {error && <p className="error">{error}</p>}
      {loading && <p className="muted">Loading…</p>}
      {!loading && jobs.length === 0 && (
        <p className="muted">
          No jobs yet. A job syncs one source to one or more destinations —{' '}
          <Link to="/jobs/new">create one</Link>.
        </p>
      )}

      <ul className="target-list">
        {jobs.map((job) => {
          const live = liveByJob.get(job.id)
          const fetched = latest.get(job.id)
          // Prefer the live copy whenever it is at least as recent; the list
          // can only ever be older.
          const run =
            live && (!fetched || live.run.started_at >= fetched.started_at) ? live.run : fetched
          const progress = run && live?.run.id === run.id ? live.progress : undefined

          return (
            <li key={job.id} className="card">
              <div className="target-head">
                <strong>{job.name}</strong>
                <span className="badge">{job.mode}</span>
                {run ? (
                  <Link className={`badge status-${run.status}`} to={`/runs/${run.id}`}>
                    {run.status}
                  </Link>
                ) : (
                  <span className="badge">never run</span>
                )}
                {run && <span className="muted">{timestamp(run.started_at)}</span>}
              </div>

              <p className="muted">
                {nameFor(job.source_target_id)}
                {job.source_subpath ? `/${job.source_subpath}` : ''} →{' '}
                {job.destinations
                  .map(
                    (d) => nameFor(d.dest_target_id) + (d.dest_subpath ? `/${d.dest_subpath}` : ''),
                  )
                  .join(', ')}
              </p>

              {run?.status === 'running' && <LiveProgress progress={progress} />}
              {/* A run waiting on a person is the one thing a dashboard must
                  not render as just another badge: it makes no progress and
                  answers itself on a timeout if nobody looks. A prompt is a
                  *destination* status, so the run itself still reads
                  'running' — only the snapshot knows. */}
              {run && (run.status === 'awaiting_confirmation' || waitingOnAnswer(progress)) && (
                <p className="warn">
                  Waiting for you — <Link to={`/runs/${run.id}`}>open the run</Link> to answer.
                </p>
              )}
              {run?.error_summary && <p className="muted">{run.error_summary}</p>}

              <div className="actions">
                <Link className="link" to={`/jobs/${job.id}`}>
                  Edit
                </Link>
                <Link className="link" to={`/logs?job=${job.id}`}>
                  Log
                </Link>
                <button
                  className="link"
                  disabled={starting === job.id}
                  onClick={() => void start(job, true)}
                >
                  Preview
                </button>
                <button disabled={starting === job.id} onClick={() => void start(job, false)}>
                  {starting === job.id ? 'Starting…' : 'Run'}
                </button>
              </div>
            </li>
          )
        })}
      </ul>
    </section>
  )
}

/** Whether any destination has stopped to ask something. */
function waitingOnAnswer(progress?: RunSnapshot): boolean {
  return progress?.destinations?.some((d) => d.status === 'awaiting_prompt') ?? false
}

/** What a run in flight is doing, at card scale. The detail page is one click
 *  away for the rest. */
function LiveProgress({ progress }: { progress?: RunSnapshot }) {
  if (!progress) {
    // Running per the database, but no frame has arrived yet — after a reload
    // the feed only knows runs it has since heard about.
    return <p className="muted">Running…</p>
  }

  return (
    <div className="run-progress">
      <Bar done={progress.bytes_done} total={progress.bytes_total} />
      <p className="muted">
        {progress.phase} · {progress.files_done}/{progress.files_total} files ·{' '}
        {bytes(progress.bytes_done)} of {bytes(progress.bytes_total)}
        {progress.throughput_bps > 0 && ` · ${rate(progress.throughput_bps)}`}
        {progress.eta_sec !== ETA_UNKNOWN && ` · ${eta(progress.eta_sec)} left`}
      </p>
    </div>
  )
}

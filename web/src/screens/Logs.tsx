import { useCallback, useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { ApiError, api } from '../api/client'
import type { Job, RunEvent, Target } from '../api/types'
import { EventList, LEVELS } from '../components/EventList'
import { useEvents } from '../hooks/useEvents'

/** store.ListEvents' own default, and a page size chosen for the reader rather
 *  than the query: 1000 is honoured by the server, but rendering a thousand
 *  rows nobody scrolls to costs more than the extra round trip saves. Anything
 *  above 1000 is silently downgraded to 200, so that is the hard ceiling. */
const PAGE = 200
const REFRESH_MS = 5000

/** The `since` presets. Anything longer than a week is what the filters are
 *  for — an unbounded scroll through months of events helps nobody. */
const WINDOWS: { label: string; hours: number }[] = [
  { label: 'Last hour', hours: 1 },
  { label: 'Last 24 hours', hours: 24 },
  { label: 'Last 7 days', hours: 24 * 7 },
]

/**
 * The log across every run (SPEC.md §9).
 *
 * A run's own log answers "what did this run do". This one answers the
 * questions that span runs — has this job been failing every night, is one
 * destination responsible for every error — which no per-run view can, because
 * the evidence is one row in each of fifty runs.
 */
export function Logs() {
  const [params, setParams] = useSearchParams()
  const feed = useEvents()

  const [jobs, setJobs] = useState<Job[]>([])
  const [targets, setTargets] = useState<Target[]>([])
  const [events, setEvents] = useState<RunEvent[]>([])
  const [loading, setLoading] = useState(true)
  const [more, setMore] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Filters live in the URL so a filtered view can be linked to — the
  // dashboard's per-job "Log" link is exactly that.
  const level = params.get('level') ?? ''
  const job = params.get('job') ?? ''
  const dest = params.get('dest') ?? ''
  const hours = params.get('hours') ?? ''

  const setFilter = (key: string, value: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next, { replace: true })
  }

  useEffect(() => {
    void Promise.all([api.jobs.list(), api.targets.list()])
      .then(([j, t]) => {
        setJobs(j)
        setTargets(t)
      })
      // The filter dropdowns are a convenience; failing to name a job should
      // not take the log itself down with it.
      .catch(() => {})
  }, [])

  /** Fetches one page. `offset` 0 replaces the list, anything else appends. */
  const fetchPage = useCallback(
    async (offset: number) => {
      setLoading(true)
      try {
        // Computed per fetch rather than held in state, so a window that says
        // "last hour" keeps meaning the last hour as the page stays open.
        const since = hours
          ? new Date(Date.now() - Number(hours) * 3600_000).toISOString()
          : undefined

        const page = await api.logs({
          level: level || undefined,
          job_id: job || undefined,
          dest: dest || undefined,
          since,
          limit: PAGE,
          offset,
        })

        setEvents((prev) => {
          if (offset === 0) return page
          // Offsets are counted from the newest event, so anything written
          // while the page was open shifts the window and hands back rows
          // already on screen. Duplicates are not merely untidy — React keys
          // collide and the list stops updating correctly.
          const held = new Set(prev.map((e) => e.id))
          return [...prev, ...page.filter((e) => !held.has(e.id))]
        })
        // No total is returned, so a short page is the only end-of-list
        // signal there is.
        setMore(page.length === PAGE)
        setError(null)
      } catch (err) {
        setError(err instanceof ApiError ? err.message : 'Could not read the log.')
      } finally {
        setLoading(false)
      }
    },
    [level, job, dest, hours],
  )

  // Any filter change starts over at the first page; keeping the old offset
  // would page into a list that no longer exists.
  useEffect(() => {
    void fetchPage(0)
  }, [fetchPage])

  // This is a screen people leave open while something runs. Refresh only the
  // first page, and only while a run is in flight: re-fetching page 0 under a
  // list that has been paged past would silently drop everything below it.
  const running = [...feed.runs.values()].some((r) => r.run.status === 'running')
  const paged = events.length > PAGE
  useEffect(() => {
    if (!running || paged) return
    const timer = window.setInterval(() => void fetchPage(0), REFRESH_MS)
    return () => window.clearInterval(timer)
  }, [running, paged, fetchPage])

  return (
    <section>
      <div className="page-head">
        <h1>Logs</h1>
        <div className="filters">
          <select value={level} onChange={(e) => setFilter('level', e.target.value)}>
            <option value="">All levels</option>
            {LEVELS.map((l) => (
              <option key={l} value={l}>
                {l}
              </option>
            ))}
          </select>

          <select value={job} onChange={(e) => setFilter('job', e.target.value)}>
            <option value="">All jobs</option>
            {jobs.map((j) => (
              <option key={j.id} value={j.id}>
                {j.name}
              </option>
            ))}
          </select>

          <select value={dest} onChange={(e) => setFilter('dest', e.target.value)}>
            <option value="">All destinations</option>
            {targets.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>

          <select value={hours} onChange={(e) => setFilter('hours', e.target.value)}>
            <option value="">All time</option>
            {WINDOWS.map((w) => (
              <option key={w.hours} value={String(w.hours)}>
                {w.label}
              </option>
            ))}
          </select>

          <button className="link" onClick={() => setFilter('level', 'error')}>
            Errors only
          </button>
          <button className="link" onClick={() => void fetchPage(0)}>
            Refresh
          </button>
        </div>
      </div>

      {error && <p className="error">{error}</p>}

      <div className="card">
        <EventList
          events={events}
          loading={loading}
          linkRuns
          empty={
            level || job || dest || hours
              ? 'Nothing matches these filters.'
              : 'Nothing logged yet. Run a job and its events land here.'
          }
        />

        {more && (
          <div className="actions">
            <button className="link" disabled={loading} onClick={() => void fetchPage(events.length)}>
              {loading ? 'Loading…' : 'Load more'}
            </button>
          </div>
        )}
      </div>
    </section>
  )
}

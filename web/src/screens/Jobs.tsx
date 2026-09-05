import { useCallback, useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ApiError, api } from '../api/client'
import type { Job, Target } from '../api/types'
import { toJobPayload } from '../api/types'

export function Jobs() {
  const navigate = useNavigate()
  const [jobs, setJobs] = useState<Job[]>([])
  const [targets, setTargets] = useState<Target[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [starting, setStarting] = useState<string | null>(null)

  const load = useCallback(async () => {
    try {
      const [j, t] = await Promise.all([api.jobs.list(), api.targets.list()])
      setJobs(j)
      setTargets(t)
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not load jobs.')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

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

  // Jobs differ mostly in one destination or a subpath, so copying one beats
  // retyping every filter rule. The copy opens in the editor rather than being
  // left in the list, because the thing you almost certainly want to change is
  // whatever makes it a different job.
  async function duplicate(job: Job) {
    setError(null)
    setStarting(job.id)
    try {
      // Refetched, not taken from the list: the list's copy is enough to
      // render a card, but a duplicate has to carry the filter rules too.
      const full = await api.jobs.get(job.id)
      const payload = toJobPayload(full)
      const created = await api.jobs.create({ ...payload, name: `${payload.name} (copy)` })
      navigate(`/jobs/${created.id}`)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not duplicate the job.')
      setStarting(null)
    }
  }

  async function remove(job: Job) {
    // Say what is actually destroyed. Deleting a job takes its run history
    // with it — runs reference the job, and an orphaned run is a log entry
    // pointing at something that no longer exists.
    if (
      !window.confirm(
        `Delete job "${job.name}"?\n\nThis also deletes its run history and logs. ` +
          `The files it synced are not touched.`,
      )
    )
      return
    try {
      await api.jobs.remove(job.id)
      void load()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not delete the job.')
    }
  }

  return (
    <section>
      <div className="page-head">
        <h1>Jobs</h1>
        <button onClick={() => navigate('/jobs/new')}>New job</button>
      </div>

      {error && <p className="error">{error}</p>}
      {loading && <p className="muted">Loading…</p>}
      {!loading && jobs.length === 0 && (
        <p className="muted">No jobs yet. A job syncs one source to one or more destinations.</p>
      )}

      <ul className="target-list">
        {jobs.map((job) => (
          <li key={job.id} className="card">
            <div className="target-head">
              <strong>{job.name}</strong>
              <span className="badge">{job.mode}</span>
              {job.filters && job.filters.length > 0 && (
                <span className="badge">
                  {job.filters.length} filter{job.filters.length === 1 ? '' : 's'}
                </span>
              )}
            </div>
            <p className="muted">
              {nameFor(job.source_target_id)}
              {job.source_subpath ? `/${job.source_subpath}` : ''} →{' '}
              {job.destinations
                .map((d) => nameFor(d.dest_target_id) + (d.dest_subpath ? `/${d.dest_subpath}` : ''))
                .join(', ')}
            </p>

            <div className="actions">
              <Link className="link" to={`/jobs/${job.id}`}>
                Edit
              </Link>
              <button className="link" disabled={starting === job.id} onClick={() => void duplicate(job)}>
                Duplicate
              </button>
              <button className="link" disabled={starting === job.id} onClick={() => void start(job, true)}>
                Preview
              </button>
              <button disabled={starting === job.id} onClick={() => void start(job, false)}>
                {starting === job.id ? 'Starting…' : 'Run'}
              </button>
              <button className="link danger" onClick={() => void remove(job)}>
                Delete
              </button>
            </div>
          </li>
        ))}
      </ul>
    </section>
  )
}

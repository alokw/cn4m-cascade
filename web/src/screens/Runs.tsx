import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import type { Run } from '../api/types'
import { timestamp } from '../format'
import { useEvents } from '../hooks/useEvents'

/** A minimal run list, so run detail is reachable before the dashboard lands
 *  in 4b-2b. The dashboard replaces this as the front door. */
export function Runs() {
  const [runs, setRuns] = useState<Run[]>([])
  const feed = useEvents()

  useEffect(() => {
    void api.runs.list({ limit: 50 }).then(setRuns).catch(() => {})
  }, [])

  // Merge the live feed over the fetched list so a running job advances here.
  const merged = new Map(runs.map((r) => [r.id, r]))
  for (const [id, live] of feed.runs) merged.set(id, live.run)
  const rows = [...merged.values()].sort((a, b) => b.started_at.localeCompare(a.started_at))

  return (
    <section>
      <div className="page-head">
        <h1>Runs</h1>
      </div>
      {rows.length === 0 && <p className="muted">No runs yet.</p>}
      <ul className="target-list">
        {rows.map((r) => (
          <li key={r.id} className="card">
            <div className="target-head">
              <span className={`badge status-${r.status}`}>{r.status}</span>
              <Link to={`/runs/${r.id}`}>{r.id.slice(0, 12)}</Link>
              <span className="muted">{timestamp(r.started_at)}</span>
            </div>
            {r.error_summary && <p className="muted">{r.error_summary}</p>}
          </li>
        ))}
      </ul>
    </section>
  )
}

import { Link } from 'react-router-dom'
import type { LogLevel, RunEvent } from '../api/types'
import { timestamp } from '../format'

/** The levels a filter dropdown offers, in severity order. */
export const LEVELS: LogLevel[] = ['info', 'warn', 'error']

/**
 * The rows of a log, shared by a run's own log and the global one.
 *
 * Only the rows. The two callers disagree about almost everything else — one
 * reads oldest-first and filters by this run's destinations, the other reads
 * newest-first and filters by job and time — and folding both into one
 * component would mean a prop for every difference. What is worth sharing is
 * the part that would otherwise drift silently: how a level is styled, how a
 * path is set apart from the message, and what an empty log says.
 */
export function EventList({
  events,
  loading,
  empty = 'Nothing logged yet.',
  linkRuns = false,
}: {
  events: RunEvent[]
  loading?: boolean
  /** Shown when there is nothing to show and nothing in flight. */
  empty?: string
  /** Adds a link to the run each event belongs to. Off inside a run's own log,
   *  where every row links to the page you are already on. */
  linkRuns?: boolean
}) {
  if (loading && events.length === 0) return <p className="muted">Loading…</p>
  if (events.length === 0) return <p className="muted">{empty}</p>

  return (
    <ul className="events">
      {events.map((e) => (
        <li key={e.id} className={e.level}>
          <span className="muted">{timestamp(e.ts)}</span> <span className="lvl">{e.level}</span>{' '}
          {linkRuns && (
            <Link className="muted" to={`/runs/${e.run_id}`}>
              {e.run_id.slice(0, 8)}
            </Link>
          )}{' '}
          {e.relpath && <code>{e.relpath}</code>} {e.message}
        </li>
      ))}
    </ul>
  )
}

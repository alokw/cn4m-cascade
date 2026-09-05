import { useEffect, useState } from 'react'
import { ApiError, api } from '../api/client'
import type { DestSnapshot, PromptAction } from '../api/types'
import { isZeroTime } from '../api/types'
import { duration } from '../format'

interface Props {
  runID: string
  dest: DestSnapshot
  targetName: string
  onAnswered: () => void
}

/**
 * The target-unavailable prompt (SPEC.md §9). Blocking, because the run is
 * waiting on it, with a visible countdown to the fallback action so nobody is
 * surprised when it answers itself.
 */
export function PromptModal({ runID, dest, targetName, onAnswered }: Props) {
  const [busy, setBusy] = useState<PromptAction | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [now, setNow] = useState(() => Date.now())

  // The deadline is a fixed instant; re-render once a second to count down.
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(id)
  }, [])

  // prompt_deadline is always present in the payload — Go's omitempty does
  // nothing for time.Time — so a truthiness check would count down from the
  // year 1. isZeroTime is the guard.
  const canCreate = dest.prompt_can_create === true
  const deadline = isZeroTime(dest.prompt_deadline) ? null : new Date(dest.prompt_deadline).getTime()
  const remaining = deadline === null ? null : Math.max(0, (deadline - now) / 1000)

  async function answer(action: PromptAction) {
    setBusy(action)
    setError(null)
    try {
      await api.runs.prompt(runID, dest.dest_target_id, action)
      onAnswered()
    } catch (err) {
      // Both 409s mean the same thing to a user: this modal is stale. The
      // prompt was answered elsewhere or timed out (no_such_prompt), or the
      // run finished underneath it (not_running). Neither is a failure they
      // can act on, so close rather than showing an error over a dead run.
      if (err instanceof ApiError && (err.code === 'no_such_prompt' || err.code === 'not_running')) {
        onAnswered()
        return
      }
      setError(err instanceof ApiError ? err.message : 'Could not send the answer.')
      setBusy(null)
    }
  }

  return (
    <div className="backdrop">
      <div className="card modal prompt">
        <h2>{canCreate ? 'Destination folder does not exist' : 'Destination unavailable'}</h2>
        <p>
          <strong>{targetName}</strong>{' '}
          {canCreate ? 'resolved, but the folder this job writes to is not there.' : 'could not be reached.'}
        </p>
        {dest.prompt_reason && <p className="muted">{dest.prompt_reason}</p>}
        {canCreate && (
          <p className="warn">
            Check the path before creating it. A mistyped folder will be created and synced into,
            which looks like success — the files simply go somewhere you did not mean.
          </p>
        )}

        {remaining !== null && (
          <p className={remaining < 30 ? 'warn' : 'muted'}>
            Falling back automatically in <strong>{duration(remaining)}</strong> if nobody answers.
          </p>
        )}

        {error && <p className="error">{error}</p>}

        <div className="actions">
          {canCreate ? (
            <button disabled={busy !== null} onClick={() => void answer('create')}>
              {busy === 'create' ? 'Creating…' : 'Create the folder'}
            </button>
          ) : (
            <button className="link" disabled={busy !== null} onClick={() => void answer('retry')}>
              {busy === 'retry' ? 'Retrying…' : 'Retry'}
            </button>
          )}
          <button className="link" disabled={busy !== null} onClick={() => void answer('skip')}>
            {busy === 'skip' ? 'Skipping…' : 'Skip and continue'}
          </button>
          <button className="danger-btn" disabled={busy !== null} onClick={() => void answer('abort')}>
            {busy === 'abort' ? 'Aborting…' : 'Abort run'}
          </button>
        </div>
      </div>
    </div>
  )
}

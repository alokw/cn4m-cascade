import { useEffect, useState } from 'react'
import { ApiError, api } from '../api/client'
import type { DestActions, DestPlan, PlannedAction, Target } from '../api/types'
import { isZeroTime } from '../api/types'
import { bytes, duration } from '../format'

interface Props {
  runID: string
  jobID: string
  plans: DestPlan[]
  confirmDeadline: string | undefined
  targets: Target[]
  onConfirmed: () => void
}

/**
 * The preview gate (SPEC.md §6.1 step 6). A confirmed preview executes the
 * plan it showed, so this screen has to show the plan honestly:
 *
 * - Deletions are listed **individually**, never summarised (CLAUDE.md). The
 *   paths come from GET /api/runs/{id}/plan rather than the progress payload.
 * - "Refusing to delete" is rendered distinctly from "nothing to delete".
 *   They look identical if you only read the counts, and they mean opposite
 *   things.
 */
export function ConfirmPlan({ runID, jobID, plans, confirmDeadline, targets, onConfirmed }: Props) {
  const [actions, setActions] = useState<DestActions[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(id)
  }, [])

  useEffect(() => {
    let cancelled = false
    api.runs
      .plan(runID)
      .then((p) => {
        if (!cancelled) setActions(p.destinations)
      })
      .catch((err) => {
        if (cancelled) return
        // no_plan means the run moved on while this screen was open. Say so
        // plainly rather than presenting it as a failure.
        setLoadError(
          err instanceof ApiError && err.code === 'no_plan'
            ? 'This run has finished, so its plan is no longer available.'
            : 'Could not load the plan.',
        )
      })
    return () => {
      cancelled = true
    }
  }, [runID])

  const deadline = isZeroTime(confirmDeadline) ? null : new Date(confirmDeadline!).getTime()
  const remaining = deadline === null ? null : Math.max(0, (deadline - now) / 1000)

  const nameFor = (id: string) => targets.find((t) => t.id === id)?.name ?? id

  // Every destination planning destructive work must have its paths loaded.
  // Confirming executes the held plan verbatim, so approving one whose
  // removals were never displayed defeats the point of the screen.
  const safeToConfirm =
    actions !== null &&
    plans.every(
      (p) =>
        p.deletes + p.rmdirs + p.replaces === 0 ||
        actions.some((a) => a.dest_target_id === p.dest_target_id),
    )

  async function confirm() {
    setBusy(true)
    setError(null)
    try {
      await api.jobs.confirm(jobID)
      onConfirmed()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not confirm the run.')
      setBusy(false)
    }
  }

  return (
    <section className="card confirm">
      <h2>Confirm this run</h2>
      <p className="muted">
        Nothing has been written yet. What you confirm here is executed as shown — it is not
        recomputed, so a destination that changes in the meantime is not re-checked.
      </p>
      {remaining !== null && (
        <p className={remaining < 60 ? 'warn' : 'muted'}>
          Cancels itself in <strong>{duration(remaining)}</strong> if nobody confirms.
        </p>
      )}

      {loadError && <p className="error">{loadError}</p>}

      {plans.map((plan) => {
        const detail = actions?.find((a) => a.dest_target_id === plan.dest_target_id)
        return (
          <div key={plan.dest_target_id} className="plan-dest">
            <h3>{nameFor(plan.dest_target_id)}</h3>

            <ul className="plan-counts">
              <li>{plan.copies} to copy ({bytes(plan.copy_bytes)})</li>
              <li>{plan.mkdirs} directories to create</li>
              <li className={plan.deletes > 0 ? 'danger' : undefined}>{plan.deletes} to delete</li>
              <li className={plan.rmdirs > 0 ? 'danger' : undefined}>
                {plan.rmdirs} directories to remove
              </li>
            </ul>

            {plan.deletions_blocked && (
              <p className="warn">
                <strong>
                  Refusing to delete {plan.withheld_deletes ?? 0} file
                  {plan.withheld_deletes === 1 ? '' : 's'}
                  {plan.withheld_rmdirs ? ` and ${plan.withheld_rmdirs} director${plan.withheld_rmdirs === 1 ? 'y' : 'ies'}` : ''}
                  .
                </strong>{' '}
                {plan.blocked_reason} Those removals are not in the plan below and will not happen.
              </p>
            )}

            {detail && (
              <>
                <PathList
                  title="Will be deleted"
                  items={detail.deletes}
                  total={plan.deletes}
                  danger
                />
                <PathList
                  title="Directories will be removed"
                  items={detail.rmdirs.map((p) => ({ relpath: p }))}
                  total={plan.rmdirs}
                  danger
                />
                <PathList
                  title="Will be replaced (wrong type at the destination)"
                  items={detail.replaces}
                  total={plan.replaces}
                  danger
                />
              </>
            )}

            {plan.overwrites > 0 && (
              <p className="warn">
                {plan.overwrites} existing file{plan.overwrites === 1 ? '' : 's'} will be
                overwritten. The destination's version is replaced and cannot be recovered.
              </p>
            )}

            {/* Counts without paths is the dangerous state: it looks complete.
                Say so, and block the button, rather than letting someone
                approve destructive work they were never shown. */}
            {!detail && actions !== null && plan.deletes + plan.rmdirs + plan.replaces > 0 && (
              <p className="error">
                The path list for this destination could not be loaded, so the removals above cannot
                be shown individually. Not safe to confirm.
              </p>
            )}

            {plan.conflicts && plan.conflicts.length > 0 && (
              <details>
                <summary>{plan.conflicts.length} skipped for conflicts</summary>
                <ul className="paths">
                  {plan.conflicts.map((c) => (
                    <li key={c}>{c}</li>
                  ))}
                </ul>
              </details>
            )}
          </div>
        )
      })}

      {error && <p className="error">{error}</p>}

      <div className="actions">
        <button
          className="link"
          disabled={busy}
          onClick={() => void api.runs.cancel(runID).then(onConfirmed).catch(() => {})}
        >
          Cancel this run
        </button>
        <button disabled={busy || actions === null || !safeToConfirm} onClick={() => void confirm()}>
          {busy ? 'Starting…' : 'Confirm and run'}
        </button>
      </div>
    </section>
  )
}

/**
 * One list of paths, which reports its own truncation against the count the
 * server computed. A single aggregate "something was truncated" flag fires on
 * the copies and mkdirs lists this screen never renders, which trains people
 * to ignore the notice on the list that matters.
 */
function PathList({
  title,
  items,
  total,
  danger,
}: {
  title: string
  items: PlannedAction[]
  total: number
  danger?: boolean
}) {
  if (total === 0) return null
  const missing = total - items.length

  return (
    <details open={items.length <= 25}>
      <summary className={danger ? 'danger' : undefined}>
        {title} ({total})
      </summary>
      <ul className="paths">
        {items.map((a) => (
          <li key={a.relpath}>
            <code>{a.relpath}</code>
            {a.size ? <span className="muted"> {bytes(a.size)}</span> : null}
          </li>
        ))}
      </ul>
      {missing > 0 && (
        <p className="warn">
          {missing} more not listed — this list is capped. Confirming acts on all {total}.
        </p>
      )}
    </details>
  )
}

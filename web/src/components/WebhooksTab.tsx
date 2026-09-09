import { useCallback, useEffect, useState } from 'react'
import { ApiError, api } from '../api/client'
import type { Webhook, WebhookFormat, WebhookPayload } from '../api/types'
import { WEBHOOK_EVENTS } from '../api/types'

/** Friendlier names for the event identifiers the API uses. */
const EVENT_LABELS: Record<string, string> = {
  run_started: 'A run starts',
  progress: 'Progress (throttled)',
  target_unavailable_prompt: 'A destination is unreachable and waiting',
  run_completed: 'A run finishes',
  run_failed: 'A run fails',
}

/**
 * The Webhooks/API tab (SPEC.md §9): trigger this job from other software, and
 * have it report back.
 */
export function WebhooksTab({
  jobID,
  hasToken,
  onTokenChange,
}: {
  jobID: string
  hasToken: boolean
  onTokenChange: () => void
}) {
  const [token, setToken] = useState<string | null>(null)
  const [hooks, setHooks] = useState<Webhook[]>([])
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      setHooks(await api.webhooks.list())
    } catch {
      // The callback list is secondary to the token; a failure here should not
      // hide the rest of the tab.
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  async function issue() {
    // Regenerating invalidates the current token in the same act, and anything
    // already using it stops working immediately. That is the point of
    // regenerating, but it is not obvious from a button labelled "Regenerate".
    if (
      hasToken &&
      !window.confirm(
        'Regenerate this job’s trigger token?\n\n' +
          'The current token stops working immediately, and anything already using it — ' +
          'a NAS task, n8n, Home Assistant — will start failing until you paste in the new one.',
      )
    ) {
      return
    }

    setBusy(true)
    setError(null)
    try {
      const issued = await api.issueToken(jobID)
      setToken(issued.token)
      onTokenChange()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not issue a token.')
    } finally {
      setBusy(false)
    }
  }

  async function revoke() {
    if (
      !window.confirm(
        'Revoke this job’s trigger token?\n\nAnything using it stops being able to start this job.',
      )
    ) {
      return
    }
    setBusy(true)
    setError(null)
    try {
      await api.revokeToken(jobID)
      setToken(null)
      onTokenChange()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not revoke the token.')
    } finally {
      setBusy(false)
    }
  }

  const origin = window.location.origin

  return (
    <div className="tab-body">
      {error && <p className="error">{error}</p>}

      <div className="card">
        <h3>Trigger this job from elsewhere</h3>
        <p className="muted">
          A token lets other software start this job without signing in — a NAS task, n8n, Home
          Assistant. It works for this job only.
        </p>

        {token ? (
          <>
            <p className="warn">
              <strong>Copy this now.</strong> It is stored only as a hash, so this is the one and
              only time it can be shown. If you lose it, regenerate — which invalidates this one.
            </p>
            <pre className="token">{token}</pre>
            <p className="hint">Then trigger a run with:</p>
            <pre className="curl">
              {`curl -X POST \\\n  -H "Authorization: Bearer ${token}" \\\n  ${origin}/api/hooks/jobs/${jobID}/run`}
            </pre>
            <p className="hint">…and poll it with:</p>
            <pre className="curl">
              {`curl -H "Authorization: Bearer ${token}" \\\n  ${origin}/api/hooks/jobs/${jobID}/status`}
            </pre>
            <p className="hint">
              The header is the recommended form. <code>?token=…</code> also works for tools that
              can only set a URL, but a token in a URL ends up in proxy logs and browser history.
            </p>
          </>
        ) : (
          <p className="muted">
            {hasToken
              ? 'This job has a token. It cannot be shown again — regenerate if you need a new one.'
              : 'This job has no token, so its trigger endpoints refuse everything.'}
          </p>
        )}

        <div className="actions">
          <button disabled={busy} onClick={() => void issue()}>
            {hasToken ? 'Regenerate token' : 'Create token'}
          </button>
          {hasToken && (
            <button className="link danger" disabled={busy} onClick={() => void revoke()}>
              Revoke
            </button>
          )}
        </div>
      </div>

      <CallbackList hooks={hooks} jobID={jobID} onChanged={load} />
    </div>
  )
}

/** The outbound half: where this job reports to when things happen. */
function CallbackList({
  hooks,
  jobID,
  onChanged,
}: {
  hooks: Webhook[]
  jobID: string
  onChanged: () => void
}) {
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [draft, setDraft] = useState<WebhookPayload>({
    url: '',
    events: [],
    enabled: true,
    format: 'json',
    min_interval_sec: 30,
    secret: '',
  })

  // This job's own callbacks, plus the global ones, which apply here too and
  // are worth showing rather than leaving someone wondering where the extra
  // POSTs come from.
  const mine = hooks.filter((h) => h.job_id === jobID)
  const global = hooks.filter((h) => !h.job_id)

  async function add() {
    setBusy(true)
    setError(null)
    try {
      await api.webhooks.create({ ...draft, job_id: jobID })
      setDraft({ url: '', events: [], enabled: true, format: 'json', min_interval_sec: 30, secret: '' })
      onChanged()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not add the callback.')
    } finally {
      setBusy(false)
    }
  }

  async function remove(hook: Webhook) {
    if (!window.confirm(`Stop sending callbacks to ${hook.url}?`)) return
    try {
      await api.webhooks.remove(hook.id)
      onChanged()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not remove the callback.')
    }
  }

  const toggleEvent = (event: string) =>
    setDraft((d) => ({
      ...d,
      events: d.events.includes(event)
        ? d.events.filter((e) => e !== event)
        : [...d.events, event],
    }))

  return (
    <div className="card">
      <h3>Report back to</h3>
      <p className="muted">
        This job POSTs to these URLs as it runs. Delivery never delays or fails a sync: a URL that is
        down is retried a few times and then logged against the run.
      </p>

      {error && <p className="error">{error}</p>}

      {mine.length === 0 && global.length === 0 && (
        <p className="muted">No callbacks configured.</p>
      )}

      <ul className="target-list">
        {mine.map((h) => (
          <li key={h.id} className="card">
            <div className="target-head">
              <code>{h.url}</code>
              {!h.enabled && <span className="badge">paused</span>}
              {h.format === 'cn4m' && <span className="badge">cn4m</span>}
              {h.has_secret && <span className="badge">signed</span>}
            </div>
            <p className="muted">
              {h.events.length === 0 ? 'Every event' : h.events.map((e) => EVENT_LABELS[e] ?? e).join(', ')}
            </p>
            <div className="actions">
              <button className="link danger" onClick={() => void remove(h)}>
                Remove
              </button>
            </div>
          </li>
        ))}
        {global.map((h) => (
          <li key={h.id} className="card">
            <div className="target-head">
              <code>{h.url}</code>
              <span className="badge">every job</span>
              {h.format === 'cn4m' && <span className="badge">cn4m</span>}
            </div>
            <p className="hint">
              {h.format === 'cn4m'
                ? 'Reports this job to cn4m, the parent system. Configured for every job; if cn4m is not running, nothing happens and nothing is logged against your runs.'
                : 'Configured globally, so it applies to this job too.'}
            </p>
          </li>
        ))}
      </ul>

      <label>
        Add a callback URL
        <input
          value={draft.url}
          placeholder="https://n8n.example.lan/webhook/cn4m"
          onChange={(e) => setDraft({ ...draft, url: e.target.value })}
        />
      </label>

      <label>
        Send it as
        <select
          value={draft.format}
          onChange={(e) => setDraft({ ...draft, format: e.target.value as WebhookFormat })}
        >
          <option value="json">JSON, signed (n8n, Home Assistant, anything custom)</option>
          <option value="cn4m">cn4m status update</option>
        </select>
        <span className="hint">
          {draft.format === 'cn4m'
            ? 'Form-encoded app/message/level, which is what cn4m’s /suite/status accepts — not JSON. No signature: that endpoint does not check one. Failures are silent, because a cn4m that is not running is a normal state rather than a problem with your sync.'
            : 'A JSON body with the same fields the status endpoint returns, plus an event name.'}
        </span>
      </label>

      <fieldset className="events-pick">
        <legend>Send when</legend>
        {WEBHOOK_EVENTS.map((event) => (
          <label key={event} className="checkbox">
            <input
              type="checkbox"
              checked={draft.events.includes(event)}
              onChange={() => toggleEvent(event)}
            />
            {EVENT_LABELS[event]}
          </label>
        ))}
        <span className="hint">Choose nothing to receive every event.</span>
      </fieldset>

      <div className="row">
        {draft.format === 'json' && (
        <label>
          Signing secret <span className="hint">optional</span>
          <input
            type="password"
            value={draft.secret ?? ''}
            placeholder="leave blank for unsigned"
            onChange={(e) => setDraft({ ...draft, secret: e.target.value })}
          />
          <span className="hint">
            Sent as an <code>X-Signature</code> header — HMAC-SHA256 of the body — so the receiver
            can tell a real callback from anything else that can reach its URL.
          </span>
        </label>
        )}
        <label>
          Progress no more often than
          <input
            type="number"
            min={1}
            value={draft.min_interval_sec}
            onChange={(e) => setDraft({ ...draft, min_interval_sec: Number(e.target.value) })}
          />
          <span className="hint">seconds</span>
        </label>
      </div>

      <div className="actions">
        <button disabled={busy || !draft.url.trim()} onClick={() => void add()}>
          Add callback
        </button>
      </div>
    </div>
  )
}

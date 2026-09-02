import { useState } from 'react'
import type { FormEvent } from 'react'
import { ApiError } from '../api/client'
import { useAuth } from '../auth'

/** Serves both first-run setup and ordinary login — the forms differ only in
 *  wording and in the minimum-length rule the server enforces. */
export function SignIn({ mode }: { mode: 'setup' | 'login' }) {
  const { signIn, completeSetup } = useAuth()
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const isSetup = mode === 'setup'

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError(null)

    if (isSetup && password !== confirm) {
      setError('The two passwords do not match.')
      return
    }

    setBusy(true)
    try {
      if (isSetup) await completeSetup(password)
      else await signIn(password)
    } catch (err) {
      if (err instanceof ApiError) {
        // 429 is its own case: telling someone their password is wrong when
        // the server has stopped checking would send them in circles.
        setError(
          err.code === 'too_many_attempts'
            ? 'Too many failed attempts. Wait a few minutes and try again.'
            : err.message,
        )
      } else {
        setError('Could not reach the server.')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="centred">
      <form className="card auth-form" onSubmit={submit}>
        <h1>{isSetup ? 'Set an admin password' : 'Sign in'}</h1>
        {isSetup && (
          <p className="muted">
            This is a new instance. The password you choose here is the only one, and it cannot be
            recovered — there is no reset flow yet. There is no minimum length, and blank is
            allowed.
          </p>
        )}

        <label>
          Password
          <input
            type="password"
            value={password}
            autoFocus
            autoComplete={isSetup ? 'new-password' : 'current-password'}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>

        {isSetup && (
          <label>
            Confirm password
            <input
              type="password"
              value={confirm}
              autoComplete="new-password"
              onChange={(e) => setConfirm(e.target.value)}
            />
          </label>
        )}

        {isSetup && password === '' && (
          <p className="warn">
            Leaving this blank means <strong>anyone who can reach this server can sign in</strong>.
            That is a reasonable choice on a closed network and a bad one on an open one.
          </p>
        )}
        {error && <p className="error">{error}</p>}

        {/* Deliberately not disabled on an empty password: a blank one is a
            supported choice, not an incomplete form. */}
        <button type="submit" disabled={busy}>
          {busy ? 'Working…' : isSetup ? 'Set password' : 'Sign in'}
        </button>
      </form>
    </div>
  )
}

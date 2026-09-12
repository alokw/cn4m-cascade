import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { ApiError, api } from '../api/client'
import type { MountErrorKind, Target, TargetPayload, TargetType } from '../api/types'
import { bytes } from '../format'

/** §9 asks for "real mount errors". The server tags every mount failure with a
 *  kind; turning that into an actionable next step is the difference between a
 *  useful error and "connection failed". */
export const MOUNT_HINTS: Record<MountErrorKind, string> = {
  auth: 'Check the username, password and domain.',
  share_missing: 'The host answered but has no share by that name.',
  unreachable: 'No route to the host. Check the address and that the server is on.',
  timeout: 'The host accepted the connection but never finished. It may be overloaded.',
  dialect: 'No SMB dialect in common. The server may be too old, or SMB1 may be disabled.',
  permission: 'The credentials are valid but lack access to this share.',
  unknown: '',
}

interface Props {
  target: Target | null
  /** Prefill from `target` but create a new record. Many targets differ only
   *  by address, so copying one beats retyping it. */
  duplicate?: boolean
  onClose: () => void
  /** Saved and finished — close. */
  onSaved: () => void
  /** Saved but staying open (a test), so the list behind should refresh. */
  onRefresh?: () => void
}

export function TargetModal({ target, duplicate = false, onClose, onSaved, onRefresh }: Props) {
  const editing = target !== null && !duplicate

  // The id to PATCH, which is not the same question as `editing`.
  //
  // "Save and test" saves first, so a brand-new target that fails its test is
  // already *saved* — and the next attempt has to update it, not create it
  // again. Without this, correcting a typo'd IP and saving again is refused
  // for a name that is already in use: your own, from thirty seconds ago.
  //
  // Kept separate from `editing` because `editing` also drives the title and
  // the password placeholder, and those read `target`, which is still null
  // here.
  const [createdID, setCreatedID] = useState<string | null>(null)
  const persistID = createdID ?? (editing ? target.id : null)

  const [type, setType] = useState<TargetType>(target?.type ?? 'smb')
  const [form, setForm] = useState({
    name: duplicate && target ? `${target.name} (copy)` : (target?.name ?? ''),
    host: target?.host ?? '',
    share: target?.share ?? '',
    subpath: target?.subpath ?? '',
    local_path: target?.local_path ?? '',
    username: target?.username ?? '',
    password: '',
    domain: target?.domain ?? '',
    mount_opts_override: target?.mount_opts_override ?? '',
    multichannel: target?.multichannel ?? false,
  })
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testOK, setTestOK] = useState<string | null>(null)
  const [testError, setTestError] = useState<string | null>(null)

  const set = <K extends keyof typeof form>(key: K, value: (typeof form)[K]) =>
    setForm((f) => ({ ...f, [key]: value }))

  function buildPayload(): TargetPayload {
    // The server validates the two types as mutually exclusive: a local target
    // must have no host, share or username, and an SMB target no local_path.
    // So the fields belonging to the *other* type are sent as "" rather than
    // omitted — omitting leaves whatever a previous configuration stored, and
    // switching an existing target between types would then be rejected.
    const payload: TargetPayload = { name: form.name, type, subpath: form.subpath }

    if (type === 'smb') {
      payload.host = form.host
      payload.share = form.share
      payload.username = form.username
      payload.domain = form.domain
      payload.mount_opts_override = form.mount_opts_override
      payload.multichannel = form.multichannel
      payload.local_path = ''
      // An omitted password keeps the stored one; an empty string would be
      // sent as a real (blank) password and erase it. A duplicate has no
      // stored password to keep, so it must be typed again.
      if (form.password !== '') payload.password = form.password
    } else {
      payload.local_path = form.local_path
      payload.host = ''
      payload.share = ''
      payload.username = ''
      payload.domain = ''
      payload.mount_opts_override = ''
      payload.multichannel = false
      // A local folder has no credentials. Clearing it also avoids stranding a
      // stored password with no username, which the server rejects if the
      // target is ever switched back to SMB.
      payload.password = ''
    }
    return payload
  }

  async function persist(): Promise<Target> {
    const payload = buildPayload()
    if (persistID) {
      return api.targets.update(persistID, payload)
    }
    const created = await api.targets.create(payload)
    // From here on this modal is editing that record, whatever it says at the
    // top. A duplicate becomes a real target the moment it is first saved too.
    setCreatedID(created.id)
    return created
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await persist()
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save the target.')
    } finally {
      setBusy(false)
    }
  }

  // The API only tests a *saved* target, so this saves first and stays open
  // with the result. Saving to test is safe here in a way it would not be for
  // a job: a target is one flat record, whereas a job's PATCH deletes and
  // reinserts its destinations and filter rules.
  async function saveAndTest() {
    setTesting(true)
    setTestOK(null)
    setTestError(null)
    setError(null)
    try {
      const saved = await persist()
      onRefresh?.()
      const r = await api.targets.test(saved.id)
      setTestOK(
        `Connected in ${r.elapsed_ms} ms` +
          (r.negotiated_vers ? ` over SMB ${r.negotiated_vers}` : '') +
          (r.free_bytes !== undefined
            ? ` — ${bytes(r.free_bytes)} free of ${bytes(r.capacity_bytes)}`
            : '') +
          '.',
      )
    } catch (err) {
      if (err instanceof ApiError) {
        setTestError(err.message + (err.kind && MOUNT_HINTS[err.kind] ? ` ${MOUNT_HINTS[err.kind]}` : ''))
      } else {
        setTestError('Could not test the target.')
      }
    } finally {
      setTesting(false)
    }
  }

  // The path this target actually points at, so a subpath typed on top of a
  // root that already contains it — the doubled-path trap — is visible while
  // typing rather than at run time.
  const base =
    type === 'smb'
      ? `//${form.host || '<host>'}/${form.share || '<share>'}`
      : form.local_path || '<path>'
  const resolved = form.subpath === '' ? base : `${base}/${form.subpath.replace(/^\/+/, '')}`

  const incomplete =
    form.name === '' || (type === 'smb' ? form.host === '' || form.share === '' : form.local_path === '')

  // Escape still closes, because a dialog with no keyboard way out is its own
  // kind of trap. It is a deliberate keystroke, unlike a stray click.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const title = duplicate ? 'Duplicate target' : editing ? 'Edit target' : 'Add target'

  return (
    // No click-to-dismiss on the backdrop. A half-filled target with a typed
    // password is expensive to lose, and the click that dismissed it was
    // usually the click that refocused the browser window — so the form
    // vanished for doing nothing at all. Closing is deliberate: the Cancel
    // button or Escape.
    <div className="backdrop">
      <form className="card modal" onSubmit={submit}>
        <h2>{title}</h2>
        {duplicate && (
          <p className="hint">
            Copied from <strong>{target?.name}</strong>. Credentials are not copied — retype the
            password if this target needs one.
          </p>
        )}

        <fieldset className="types">
          <legend>Type</legend>
          <label className="checkbox">
            <input type="radio" name="type" checked={type === 'smb'} onChange={() => setType('smb')} />
            SMB share
          </label>
          <label className="checkbox">
            <input
              type="radio"
              name="type"
              checked={type === 'local'}
              onChange={() => setType('local')}
            />
            Local folder
          </label>
        </fieldset>

        <label>
          Name
          <input value={form.name} autoFocus onChange={(e) => set('name', e.target.value)} />
        </label>

        {type === 'smb' ? (
          <>
            <div className="row">
              <label>
                Host or IP
                <input
                  value={form.host}
                  placeholder="192.168.1.50"
                  onChange={(e) => set('host', e.target.value)}
                />
              </label>
              <label>
                Share
                <input value={form.share} placeholder="media" onChange={(e) => set('share', e.target.value)} />
              </label>
            </div>

            <div className="row">
              <label>
                Username
                <input value={form.username} onChange={(e) => set('username', e.target.value)} />
              </label>
              <label>
                Password
                <input
                  type="password"
                  value={form.password}
                  placeholder={editing && target.has_password ? 'unchanged' : 'blank for guest'}
                  onChange={(e) => set('password', e.target.value)}
                />
              </label>
            </div>
          </>
        ) : (
          <label>
            Folder path
            <input
              value={form.local_path}
              placeholder="an absolute path the server can read"
              onChange={(e) => set('local_path', e.target.value)}
            />
            <span className="hint">
              An absolute path <strong>as the server sees it</strong>, which is not always the path
              you see.
              <br />
              Running <strong>natively</strong>, it is an ordinary path on that machine &mdash;{' '}
              <code>M:\projects\repo</code> or <code>/srv/media</code>.
              <br />
              Running in <strong>Docker</strong>, it is a path inside the container:{' '}
              <code>/mnt/local</code> is the folder bind-mounted there (your <code>~/cn4m</code>, or{' '}
              <code>%USERPROFILE%\cn4m</code> on Windows), and subfolders such as{' '}
              <code>/mnt/local/photos</code> work too. Set <code>CN4M_CASCADE_LOCAL_DIR</code> in{' '}
              <code>.env</code> to share a different folder.
            </span>
          </label>
        )}

        <label>
          Subpath <span className="hint">optional, relative to the root above</span>
          <input value={form.subpath} onChange={(e) => set('subpath', e.target.value)} />
          <span className="hint">
            Resolves to <code>{resolved}</code>
          </span>
        </label>

        {type === 'smb' && (
          <details>
            <summary>Advanced</summary>
            <label>
              Domain
              <input value={form.domain} onChange={(e) => set('domain', e.target.value)} />
            </label>
            <label>
              Mount options override
              <input
                value={form.mount_opts_override}
                placeholder="vers=3.0,echo_interval=1"
                onChange={(e) => set('mount_opts_override', e.target.value)}
              />
            </label>
            <label className="checkbox">
              <input
                type="checkbox"
                checked={form.multichannel}
                onChange={(e) => set('multichannel', e.target.checked)}
              />
              Enable SMB multichannel
            </label>
          </details>
        )}

        {error && <p className="error">{error}</p>}
        {testOK && <p className="ok">{testOK}</p>}
        {testError && <p className="error">{testError}</p>}

        <div className="actions">
          <button type="button" className="link" onClick={onClose}>
            Close
          </button>
          <button
            type="button"
            className="link"
            disabled={busy || testing || incomplete}
            onClick={() => void saveAndTest()}
            title="Saves first — the server can only test a target it knows about"
          >
            {testing ? 'Testing…' : 'Save and test'}
          </button>
          <button type="submit" disabled={busy || testing || incomplete}>
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </div>
  )
}

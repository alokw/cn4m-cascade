import { useState } from 'react'
import type { FormEvent } from 'react'
import { ApiError, api } from '../api/client'
import type { Target, TargetPayload, TargetType } from '../api/types'

interface Props {
  target: Target | null
  onClose: () => void
  onSaved: () => void
}

export function TargetModal({ target, onClose, onSaved }: Props) {
  const editing = target !== null

  const [type, setType] = useState<TargetType>(target?.type ?? 'smb')
  const [form, setForm] = useState({
    name: target?.name ?? '',
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

  const set = <K extends keyof typeof form>(key: K, value: (typeof form)[K]) =>
    setForm((f) => ({ ...f, [key]: value }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setBusy(true)

    // The server validates the two types as mutually exclusive: a local target
    // must have no host, share or username, and an SMB target no local_path.
    // So the fields belonging to the *other* type are sent as "" rather than
    // omitted — omitting leaves whatever a previous configuration stored, and
    // switching an existing target between types would then be rejected.
    const payload: TargetPayload = {
      name: form.name,
      type,
      subpath: form.subpath,
    }

    if (type === 'smb') {
      payload.host = form.host
      payload.share = form.share
      payload.username = form.username
      payload.domain = form.domain
      payload.mount_opts_override = form.mount_opts_override
      payload.multichannel = form.multichannel
      payload.local_path = ''
      // An omitted password keeps the stored one; an empty string would be
      // sent as a real (blank) password and erase it.
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

    try {
      if (editing) await api.targets.update(target.id, payload)
      else await api.targets.create(payload)
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save the target.')
    } finally {
      setBusy(false)
    }
  }

  const incomplete = form.name === '' || (type === 'smb' ? form.host === '' || form.share === '' : form.local_path === '')

  return (
    <div className="backdrop" onClick={onClose}>
      <form className="card modal" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        <h2>{editing ? 'Edit target' : 'Add target'}</h2>

        <fieldset className="types">
          <legend>Type</legend>
          <label className="checkbox">
            <input
              type="radio"
              name="type"
              checked={type === 'smb'}
              onChange={() => setType('smb')}
            />
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
              placeholder="/mnt/local/stuff"
              onChange={(e) => set('local_path', e.target.value)}
            />
            <span className="hint">
              An absolute path <strong>inside the container</strong>, not on your host — mount the
              host directory into the container first (SPEC.md §10 shows the volume). No credentials
              and no mounting: the engine reads it directly.
            </span>
          </label>
        )}

        <label>
          Subpath <span className="hint">optional, relative to the root above</span>
          <input value={form.subpath} onChange={(e) => set('subpath', e.target.value)} />
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

        <div className="actions">
          <button type="button" className="link" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" disabled={busy || incomplete}>
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </div>
  )
}

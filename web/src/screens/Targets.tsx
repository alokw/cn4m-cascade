import { useCallback, useEffect, useState } from 'react'
import { ApiError, api } from '../api/client'
import type { MountErrorKind, Target, TestResult } from '../api/types'
import { targetRoot } from '../api/types'
import { MOUNT_HINTS, TargetModal } from '../components/TargetModal'
import { bytes } from '../format'

type TestState = { status: 'idle' } | { status: 'testing' } | { status: 'done'; result: TestResult } | { status: 'failed'; message: string; kind?: MountErrorKind }

export function Targets() {
  const [targets, setTargets] = useState<Target[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [editing, setEditing] = useState<Target | null>(null)
  const [duplicating, setDuplicating] = useState<Target | null>(null)
  const [adding, setAdding] = useState(false)
  const [tests, setTests] = useState<Record<string, TestState>>({})

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setTargets(await api.targets.list())
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not load targets.')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  async function test(id: string) {
    setTests((t) => ({ ...t, [id]: { status: 'testing' } }))
    try {
      const result = await api.targets.test(id)
      setTests((t) => ({ ...t, [id]: { status: 'done', result } }))
    } catch (err) {
      const e = err instanceof ApiError ? err : null
      setTests((t) => ({
        ...t,
        [id]: {
          status: 'failed',
          message: e?.message ?? 'The test failed.',
          kind: e?.kind,
        },
      }))
    }
    // A test mounts the share, so the cached health may have changed.
    void load()
  }

  async function remove(target: Target) {
    if (!window.confirm(`Delete target "${target.name}"? Jobs using it will need editing.`)) return
    try {
      await api.targets.remove(target.id)
      void load()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not delete the target.')
    }
  }

  return (
    <section>
      <div className="page-head">
        <h1>Targets</h1>
        <button onClick={() => setAdding(true)}>Add target</button>
      </div>

      {error && <p className="error">{error}</p>}
      {loading && <p className="muted">Loading…</p>}
      {!loading && targets.length === 0 && <p className="muted">No targets yet.</p>}

      <ul className="target-list">
        {targets.map((t) => {
          const state = tests[t.id] ?? { status: 'idle' as const }
          return (
            <li key={t.id} className="card">
              <div className="target-head">
                <span className={`dot ${t.health.state}`} title={t.health.message ?? t.health.state} />
                <strong>{t.name}</strong>
                <span className="muted">{targetRoot(t)}</span>
                {t.type === 'local' && <span className="badge">local</span>}
                {t.negotiated_vers && <span className="badge">SMB {t.negotiated_vers}</span>}
              </div>

              <div className="actions">
                <button className="link" onClick={() => void test(t.id)} disabled={state.status === 'testing'}>
                  {state.status === 'testing' ? 'Testing…' : 'Test connection'}
                </button>
                <button className="link" onClick={() => setEditing(t)}>
                  Edit
                </button>
                <button className="link" onClick={() => setDuplicating(t)}>
                  Duplicate
                </button>
                <button className="link danger" onClick={() => void remove(t)}>
                  Delete
                </button>
              </div>

              {state.status === 'done' && (
                <p className="ok">
                  Connected in {state.result.elapsed_ms} ms
                  {state.result.negotiated_vers ? ` over SMB ${state.result.negotiated_vers}` : ''}
                  {state.result.free_bytes !== undefined
                    ? ` — ${bytes(state.result.free_bytes)} free of ${bytes(state.result.capacity_bytes)}`
                    : ''}
                  .
                </p>
              )}
              {state.status === 'failed' && (
                <p className="error">
                  {state.message}
                  {state.kind && MOUNT_HINTS[state.kind] ? ` ${MOUNT_HINTS[state.kind]}` : ''}
                </p>
              )}
            </li>
          )
        })}
      </ul>

      {(adding || editing || duplicating) && (
        <TargetModal
          target={editing ?? duplicating}
          duplicate={duplicating !== null}
          onClose={() => {
            setAdding(false)
            setEditing(null)
            setDuplicating(null)
          }}
          onSaved={() => {
            setAdding(false)
            setEditing(null)
            setDuplicating(null)
            void load()
          }}
          onRefresh={() => void load()}
        />
      )}
    </section>
  )
}

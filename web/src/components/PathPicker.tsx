import { useCallback, useEffect, useState } from 'react'
import { ApiError, api } from '../api/client'
import type { BrowseEntry } from '../api/types'

interface Props {
  targetID: string
  /** The subpath being edited, relative to the target root. */
  value: string
  onPick: (path: string) => void
  onClose: () => void
}

/**
 * Browses a target so a subpath can be chosen rather than typed. Subpaths are
 * always relative to the target root — the server rejects absolute paths and
 * `..` segments — so this only ever moves within that root.
 */
export function PathPicker({ targetID, value, onPick, onClose }: Props) {
  const [path, setPath] = useState(value)
  const [entries, setEntries] = useState<BrowseEntry[]>([])
  const [parent, setParent] = useState<string | undefined>()
  const [truncated, setTruncated] = useState(false)
  const [total, setTotal] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  const load = useCallback(
    async (at: string) => {
      setLoading(true)
      setError(null)
      try {
        const res = await api.browse(targetID, at)
        setEntries(res.entries ?? [])
        setParent(res.parent)
        setTruncated(res.truncated)
        setTotal(res.total)
        setPath(res.path)
      } catch (err) {
        // A target that will not mount is the common case here, and its
        // message already says why (wrong password, host down, no such share).
        setError(err instanceof ApiError ? err.message : 'Could not list that folder.')
        setEntries([])
      } finally {
        setLoading(false)
      }
    },
    [targetID],
  )

  useEffect(() => {
    void load(value)
  }, [load, value])

  const dirs = entries.filter((e) => e.is_dir)

  return (
    <div className="backdrop" onClick={onClose}>
      <div className="card modal" onClick={(e) => e.stopPropagation()}>
        <h2>Choose a folder</h2>
        <p className="muted">
          <code>{path === '' ? '(target root)' : path}</code>
        </p>

        {error && <p className="error">{error}</p>}
        {loading && <p className="muted">Loading…</p>}

        {!loading && !error && (
          <>
            <ul className="paths picker">
              {path !== '' && (
                <li>
                  <button className="link" onClick={() => void load(parent ?? '')}>
                    ../ (up one level)
                  </button>
                </li>
              )}
              {dirs.map((e) => (
                <li key={e.path}>
                  <button className="link" onClick={() => void load(e.path)}>
                    {e.name}/
                  </button>
                </li>
              ))}
              {dirs.length === 0 && <li className="muted">No subfolders here.</li>}
            </ul>

            {truncated && (
              <p className="warn">
                Showing the first {entries.length} of {total} entries. If the folder you want is not
                listed, type its path instead — a long listing is cut off, not sorted for you.
              </p>
            )}
          </>
        )}

        <div className="actions">
          <button className="link" onClick={onClose}>
            Cancel
          </button>
          <button onClick={() => onPick(path)}>Use this folder</button>
        </div>
      </div>
    </div>
  )
}

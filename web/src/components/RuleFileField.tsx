import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { FilterFileCheck } from '../api/types'

/**
 * The file-path input for a listfile/jsonfile rule, with an advisory existence
 * check.
 *
 * Advisory on purpose: rule files are read at the start of every run, not
 * snapshotted at save (SPEC.md §6.5), so configuring a job before its rule file
 * exists is a legitimate workflow and must not be blocked. What this catches is
 * the silent case — a typo that sits unnoticed until a run fails.
 */
export function RuleFileField({
  value,
  onChange,
  placeholder = 'a path the server can read, or target://<target-id>/excludes.txt',
}: {
  value: string
  onChange: (v: string) => void
  /** Overridden per source, so a JSON rule can show a catalogue path rather
   *  than an excludes list that would be the wrong shape for it. */
  placeholder?: string
}) {
  const [check, setCheck] = useState<FilterFileCheck | null>(null)

  // Debounced, and only after the field looks like a path worth asking about.
  useEffect(() => {
    const path = value.trim()
    if (path === '') {
      setCheck(null)
      return
    }
    let cancelled = false
    const timer = window.setTimeout(() => {
      api
        .checkFilterFile(path)
        .then((r) => {
          if (!cancelled) setCheck(r)
        })
        .catch(() => {
          // The check is a nicety; a failure here must not derail the form.
          if (!cancelled) setCheck(null)
        })
    }, 500)
    return () => {
      cancelled = true
      window.clearTimeout(timer)
    }
  }, [value])

  return (
    <label>
      File path
      <input value={value} placeholder={placeholder} onChange={(e) => onChange(e.target.value)} />
      <span className="hint">
        A path <strong>as the server sees it</strong> &mdash; an ordinary path when running
        natively, a path inside the container under Docker &mdash; or{' '}
        <code>target://&lt;target-id&gt;/path</code> to read it off a share. Re-read at the start of
        every run, not saved with the job.
      </span>
      {check && !check.checked && check.message && <span className="hint">{check.message}</span>}
      {check?.checked && !check.exists && (
        <span className="warn">
          {check.message ?? 'No file at that path yet.'} You can still save — it is read when the job
          runs.
        </span>
      )}
      {check?.checked && check.exists && <span className="ok">File found.</span>}
    </label>
  )
}

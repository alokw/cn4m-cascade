import { useCallback, useEffect, useMemo, useState } from 'react'
import type { FormEvent } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ApiError, api } from '../api/client'
import type {
  FilterRulePayload,
  FilterTestResult,
  JobPayload,
  SchedulePreview,
  Target,
  UnavailablePolicy,
} from '../api/types'
import { targetRoot, toJobPayload } from '../api/types'
import { FilterRuleRow } from '../components/FilterRuleRow'
import { PathPicker } from '../components/PathPicker'
import { WebhooksTab } from '../components/WebhooksTab'
import { timestamp } from '../format'

const BLANK: JobPayload = {
  name: '',
  source_target_id: '',
  source_subpath: '',
  mode: 'update',
  compare: 'fast',
  compare_tolerance_sec: 2,
  ignore_dst_hour: false,
  workers: 1,
  log_every_file: false,
  on_error: 'skip',
  delete_policy: 'skip_deletes',
  unavailable_policy: 'skip',
  prompt_timeout_sec: 600,
  prompt_fallback: 'skip',
  create_dest_dirs: 'ask',
  parallel_destinations: false,
  schedule_cron: '',
  enabled: true,
  destinations: [{ dest_target_id: '', dest_subpath: '' }],
  filters: [],
}

const NEW_RULE: FilterRulePayload = {
  scope: 'job',
  direction: 'exclude',
  source: 'inline',
  patterns: [],
  case_sensitive: false,
  on_error: 'fail_run',
}

type Tab = 'settings' | 'filters' | 'webhooks'

/** The source subpath's existence, as far as the last check could tell.
 *  'unknown' covers a target that is unreachable or erroring for some other
 *  reason: that is the targets page's problem to report, and offering to
 *  create a folder on a NAS that is down would be noise. */
type SourceCheck =
  | { state: 'idle' }
  | { state: 'checking' }
  | { state: 'ok' }
  | { state: 'unknown' }
  | { state: 'missing'; message: string }
  | { state: 'creating' }
  | { state: 'created' }
  | { state: 'failed'; message: string }

export function JobEditor() {
  const { id } = useParams()
  const navigate = useNavigate()
  const editing = id !== undefined

  const [job, setJob] = useState<JobPayload>(BLANK)
  const [targets, setTargets] = useState<Target[]>([])
  const [tab, setTab] = useState<Tab>('settings')
  // Whether this job currently has a trigger token. Tracked here rather than
  // inside the tab so the tab can be unmounted and remounted without losing
  // it, and refreshed after issuing or revoking.
  const [hasToken, setHasToken] = useState(false)

  // Re-read after issuing or revoking, so the tab's buttons and the "this job
  // has a token" line reflect what the server actually holds rather than what
  // the last click intended.
  const refreshToken = useCallback(async () => {
    if (!id) return
    try {
      setHasToken((await api.jobs.get(id)).has_trigger_token)
    } catch {
      // Advisory: the tab has already shown the outcome of the action itself.
    }
  }, [id])
  const [error, setError] = useState<string | null>(null)
  const [ruleErrors, setRuleErrors] = useState<Record<number, string>>({})
  const [busy, setBusy] = useState(false)
  const [loading, setLoading] = useState(editing)
  /** True when an edit's load failed. The form must not render: it would show
   *  BLANK — one empty destination, no filters — under a real job id, and
   *  saving that PATCHes every destination and filter rule out of existence. */
  const [loadFailed, setLoadFailed] = useState(false)
  const [picking, setPicking] = useState<{ target: string; value: string; apply: (p: string) => void } | null>(null)

  // Filter tests run against the SAVED job, so the button is disabled until
  // the editor and the database agree.
  const [savedSnapshot, setSavedSnapshot] = useState<string>('')
  const [savedJob, setSavedJob] = useState<string>('')
  const [testResult, setTestResult] = useState<FilterTestResult | null>(null)
  const [testError, setTestError] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)

  useEffect(() => {
    void api.targets
      .list()
      .then(setTargets)
      .catch(() =>
        setError('Could not load targets, so the target menus are empty. Reload to try again.'),
      )
  }, [])

  useEffect(() => {
    // Reset per id: navigating /jobs/A → /jobs/B, or → /jobs/new, otherwise
    // leaves A's populated form on screen under B's id, and a save in that
    // window writes A's settings onto B.
    if (!editing) {
      setJob(BLANK)
      setSavedSnapshot('')
      setLoadFailed(false)
      setLoading(false)
      return
    }

    let cancelled = false
    setLoading(true)
    setLoadFailed(false)
    void (async () => {
      try {
        // toJobPayload is required, not tidiness: the server sets
        // DisallowUnknownFields, so a fetched Job handed back verbatim is a 400.
        const fetched = await api.jobs.get(id)
        const payload = toJobPayload(fetched)
        if (cancelled) return
        setJob(payload)
        setHasToken(fetched.has_trigger_token)
        setSavedSnapshot(snapshotOf(payload))
        setSavedJob(JSON.stringify(payload))
      } catch (err) {
        if (cancelled) return
        setLoadFailed(true)
        setError(err instanceof ApiError ? err.message : 'Could not load the job.')
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [editing, id])

  const set = useCallback(<K extends keyof JobPayload>(key: K, value: JobPayload[K]) => {
    setJob((j) => ({ ...j, [key]: value }))
  }, [])


  const destTargetIDs = useMemo(
    () => job.destinations.map((d) => d.dest_target_id).filter((t) => t !== ''),
    [job.destinations],
  )

  // Anything filter-test actually reads must be saved, not just the rules:
  // runner.FilterTest scans the saved source subpath and iterates the saved
  // destinations, so a changed source turns the report into a sample of a
  // different tree presented as validation of these rules.
  const testInputsDirty = snapshotOf(job) !== savedSnapshot
  // Everything, for the Run buttons: running a job that differs from what is
  // on screen is the same trap as testing filters that are not saved.
  const dirty = editing && JSON.stringify(job) !== savedJob

  /** Guards for the rules whose server-side messages are hard to act on once
   *  the form has been submitted and re-rendered. */
  function localProblem(): string | null {
    if (job.name.trim() === '') return 'The job needs a name.'
    if (job.source_target_id === '') return 'Choose a source target.'
    if (job.destinations.some((d) => d.dest_target_id === '')) return 'Every destination needs a target.'

    const seen = new Set<string>()
    for (const d of job.destinations) {
      if (seen.has(d.dest_target_id)) {
        return 'Two destinations point at the same target. Use one destination per target.'
      }
      seen.add(d.dest_target_id)
    }

    // A destination that is the source, or sits inside it, would have the job
    // copying into its own source tree.
    for (const d of job.destinations) {
      if (d.dest_target_id !== job.source_target_id) continue
      const src = job.source_subpath ?? ''
      const dst = d.dest_subpath ?? ''
      if (src === dst || src === '' || dst === '' || dst.startsWith(src + '/') || src.startsWith(dst + '/')) {
        return 'A destination must not be the same location as the source, or nested inside it.'
      }
    }
    return null
  }

  async function save(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setRuleErrors({})

    const problem = localProblem()
    if (problem) {
      setError(problem)
      return
    }

    setBusy(true)
    try {
      const saved = editing ? await api.jobs.update(id, job) : await api.jobs.create(job)
      const payload = toJobPayload(saved)
      setJob(payload)
      setSavedSnapshot(snapshotOf(payload))
      setSavedJob(JSON.stringify(payload))
      // Creating a job ends at the jobs list, not in the editor for the thing
      // just created: "Save" on a new job reads as "I am done here", and
      // landing back on the same form makes it look as though nothing
      // happened. Editing stays put, because saving mid-edit is a checkpoint
      // rather than a finish.
      if (!editing) navigate('/jobs')
    } catch (err) {
      if (err instanceof ApiError) {
        // "filter rule N: ..." — blame the row rather than printing the whole
        // string above a form with ten fields in it. N is 1-based.
        // Two shapes exist server-side: "filter rule N: reason" from the rule's
        // own validation, and "filter rule N is scoped to ..." from the job's
        // cross-check. Matching only the first left the second unattributed.
        const match = /^filter rule (\d+)(?::\s*|\s+)(.*)$/s.exec(err.message)
        const row = match?.[1]
        const reason = match?.[2]
        if (row !== undefined && reason !== undefined) {
          setRuleErrors({ [Number(row) - 1]: reason })
          setTab('filters')
          setError('One of the filter rules is not valid.')
        } else if (err.code === 'job_running') {
          setError('This job is running, so it cannot be edited yet. Wait for it to finish, or cancel it.')
        } else {
          setError(err.message)
        }
      } else {
        setError('Could not save the job.')
      }
    } finally {
      setBusy(false)
    }
  }

  // Running from the editor: the whole point is not having to go back to the
  // jobs list to start what you just configured.
  async function saveAndRun(preview: boolean) {
    if (!editing) return
    setBusy(true)
    setError(null)
    try {
      const run = await api.jobs.run(id, preview)
      navigate(`/runs/${run.id}`)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not start the run.')
      setBusy(false)
    }
  }

  async function runFilterTest() {
    if (!editing) return
    setTesting(true)
    setTestError(null)
    try {
      setTestResult(await api.jobs.filterTest(id))
    } catch (err) {
      // The split is deliberate: 502 means the share is unreachable, 400 means
      // a rule is broken. Telling someone their rules are wrong when the NAS
      // is unplugged sends them to the wrong place.
      const e = err instanceof ApiError ? err : null
      setTestError(
        e?.code === 'source_unavailable'
          ? `The source could not be reached, so the rules were not tested. ${e.message}`
          : (e?.message ?? 'The filter test failed.'),
      )
      setTestResult(null)
    } finally {
      setTesting(false)
    }
  }

  if (loading) return <p className="muted">Loading…</p>
  if (loadFailed) {
    return (
      <section>
        <div className="page-head">
          <h1>Edit job</h1>
        </div>
        <p className="error">{error ?? 'Could not load the job.'}</p>
        <p className="muted">
          The form is not shown because saving a partially-loaded job would replace its
          destinations and filter rules with what little loaded.
        </p>
        <div className="actions">
          <button onClick={() => navigate(0)}>Try again</button>
          <button className="link" onClick={() => navigate('/jobs')}>
            Back to jobs
          </button>
        </div>
      </section>
    )
  }

  return (
    <section>
      <div className="page-head">
        <h1>{editing ? 'Edit job' : 'New job'}</h1>
        <div className="tabs">
          <button className={tab === 'settings' ? 'link active' : 'link'} onClick={() => setTab('settings')}>
            Settings
          </button>
          <button className={tab === 'filters' ? 'link active' : 'link'} onClick={() => setTab('filters')}>
            Filters {job.filters.length > 0 ? `(${job.filters.length})` : ''}
          </button>
          {/* Only for a saved job: a trigger token needs a job id to belong
              to, and issuing one against a job that may never be created
              would leave a live credential for nothing. */}
          {editing && (
            <button
              className={tab === 'webhooks' ? 'link active' : 'link'}
              onClick={() => setTab('webhooks')}
            >
              Webhooks &amp; API
            </button>
          )}
        </div>
      </div>

      {error && <p className="error">{error}</p>}

      <form onSubmit={save}>
        {/* Disabled while saving: the response replaces the whole form, so
            anything typed mid-flight would be silently discarded. */}
        <fieldset className="bare" disabled={busy}>
        {tab === 'webhooks' ? (
          // `id ?? ''` only to satisfy the type: this branch is unreachable
          // unless `editing`, which is exactly `id !== undefined`.
          <WebhooksTab jobID={id ?? ''} hasToken={hasToken} onTokenChange={() => void refreshToken()} />
        ) : tab === 'settings' ? (
          <Settings
            job={job}
            set={set}
            targets={targets}
            onPick={(target, value, apply) => setPicking({ target, value, apply })}
          />
        ) : (
          <Filters
            job={job}
            set={set}
            targets={targets}
            destTargetIDs={destTargetIDs}
            ruleErrors={ruleErrors}
            editing={editing}
            dirty={testInputsDirty}
            testing={testing}
            testResult={testResult}
            testError={testError}
            onTest={runFilterTest}
          />
        )}

        </fieldset>

        <div className="actions sticky">
          {/* "Back" rather than "Cancel": after a save there is nothing to
              cancel, and the only way out read as discarding the work. */}
          <button type="button" className="link" onClick={() => navigate('/jobs')}>
            Back to jobs
          </button>
          {editing && (
            <>
              <button
                type="button"
                className="link"
                disabled={busy || dirty}
                title={dirty ? 'Save your changes first' : undefined}
                onClick={() => void saveAndRun(true)}
              >
                Preview
              </button>
              <button
                type="button"
                disabled={busy || dirty}
                title={dirty ? 'Save your changes first' : undefined}
                onClick={() => void saveAndRun(false)}
              >
                Run
              </button>
            </>
          )}
          <button type="submit" disabled={busy}>
            {busy ? 'Saving…' : editing ? 'Save changes' : 'Create job'}
          </button>
        </div>
        {editing && dirty && (
          <p className="hint">Save your changes before running, so the run uses what you see.</p>
        )}
      </form>

      {picking && (
        <PathPicker
          targetID={picking.target}
          value={picking.value}
          onClose={() => setPicking(null)}
          onPick={(p) => {
            picking.apply(p)
            setPicking(null)
          }}
        />
      )}
    </section>
  )
}

/** The path a target + subpath actually resolves to, so a doubled segment is
 *  visible in the form rather than at run time. */
function ResolvedPath({ targets, targetID, subpath }: { targets: Target[]; targetID: string; subpath: string }) {
  const target = targets.find((t) => t.id === targetID)
  if (!target) return null

  const base = targetRoot(target)
  const full = subpath === '' ? base : `${base}/${subpath.replace(/^\/+/, '')}`
  return (
    <span className="hint">
      Resolves to <code>{full}</code>
    </span>
  )
}

function Settings({
  job,
  set,
  targets,
  onPick,
}: {
  job: JobPayload
  set: <K extends keyof JobPayload>(k: K, v: JobPayload[K]) => void
  targets: Target[]
  onPick: (target: string, value: string, apply: (p: string) => void) => void
}) {
  const options = targets.map((t) => (
    <option key={t.id} value={t.id}>
      {t.name}
    </option>
  ))

  // Whether the source subpath actually exists. A missing *source* is a hard
  // error at run time by design (internal/storage/storage.go, CreateSubpath):
  // inventing one would turn a typo into a run that copies nothing and reports
  // success. Offering to create it here, before the job is saved, is the
  // version of that which cannot be mistaken for a successful sync.
  const [sourceCheck, setSourceCheck] = useState<SourceCheck>({ state: 'idle' })

  const sourceTargetID = job.source_target_id
  const sourceSubpath = job.source_subpath ?? ''

  // Check the source folder as the user types, debounced. A listing can mount
  // a share, so this must not fire on every keystroke.
  useEffect(() => {
    if (sourceTargetID === '' || sourceSubpath.trim() === '') {
      // No subpath means the target root, which Resolve already proves exists.
      setSourceCheck({ state: 'idle' })
      return
    }
    let cancelled = false
    setSourceCheck({ state: 'checking' })
    const timer = setTimeout(() => {
      void (async () => {
        try {
          await api.browse(sourceTargetID, sourceSubpath)
          if (!cancelled) setSourceCheck({ state: 'ok' })
        } catch (err) {
          if (cancelled) return
          if (err instanceof ApiError && err.status === 404 && err.code === 'path_not_found') {
            setSourceCheck({ state: 'missing', message: err.message })
          } else {
            setSourceCheck({ state: 'unknown' })
          }
        }
      })()
    }, 500)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [sourceTargetID, sourceSubpath])

  const createSourceFolder = useCallback(async () => {
    setSourceCheck({ state: 'creating' })
    try {
      await api.makeDir(sourceTargetID, sourceSubpath)
      setSourceCheck({ state: 'created' })
    } catch (err) {
      setSourceCheck({
        state: 'failed',
        message: err instanceof ApiError ? err.message : 'Could not create that folder.',
      })
    }
  }, [sourceTargetID, sourceSubpath])

  return (
    <>
      <div className="card">
        <label>
          Name
          <input value={job.name} autoFocus onChange={(e) => set('name', e.target.value)} />
        </label>

        <h3>Source</h3>
        <div className="row">
          <label>
            Target
            <select value={job.source_target_id} onChange={(e) => set('source_target_id', e.target.value)}>
              <option value="">Choose…</option>
              {options}
            </select>
          </label>
          <label>
            Subpath <span className="hint">inside the target, not a full path</span>
            <div className="with-button">
              <input
                value={job.source_subpath ?? ''}
                placeholder="(target root)"
                onChange={(e) => set('source_subpath', e.target.value)}
              />
              <button
                type="button"
                className="link"
                disabled={job.source_target_id === ''}
                onClick={() =>
                  onPick(job.source_target_id, job.source_subpath ?? '', (p) => set('source_subpath', p))
                }
              >
                Browse
              </button>
            </div>
            <ResolvedPath
              targets={targets}
              targetID={job.source_target_id}
              subpath={job.source_subpath ?? ''}
            />
            {sourceCheck.state === 'missing' && (
              <p className="warn">
                <strong>That folder does not exist yet.</strong> A run will stop rather than create
                it: an empty source that was never there copies nothing and would report success.
                {' '}
                <button type="button" className="link" onClick={() => void createSourceFolder()}>
                  Create it now
                </button>
              </p>
            )}
            {sourceCheck.state === 'creating' && <p className="hint">Creating the folder…</p>}
            {sourceCheck.state === 'created' && <p className="hint">Folder created.</p>}
            {sourceCheck.state === 'failed' && <p className="error">{sourceCheck.message}</p>}
          </label>
        </div>

        <h3>Destinations</h3>
        {job.destinations.map((d, i) => (
          <div className="row" key={i}>
            <label>
              Target
              <select
                value={d.dest_target_id}
                onChange={(e) => {
                  const next = [...job.destinations]
                  next[i] = { ...d, dest_target_id: e.target.value }
                  set('destinations', next)
                }}
              >
                <option value="">Choose…</option>
                {options}
              </select>
            </label>
            <label>
              Subpath
              <div className="with-button">
                <input
                  value={d.dest_subpath ?? ''}
                  placeholder="(target root)"
                  onChange={(e) => {
                    const next = [...job.destinations]
                    next[i] = { ...d, dest_subpath: e.target.value }
                    set('destinations', next)
                  }}
                />
                <button
                  type="button"
                  className="link"
                  disabled={d.dest_target_id === ''}
                  onClick={() =>
                    onPick(d.dest_target_id, d.dest_subpath ?? '', (p) => {
                      const next = [...job.destinations]
                      next[i] = { ...next[i]!, dest_subpath: p }
                      set('destinations', next)
                    })
                  }
                >
                  Browse
                </button>
                {job.destinations.length > 1 && (
                  <button
                    type="button"
                    className="link danger"
                    onClick={() => set('destinations', job.destinations.filter((_, j) => j !== i))}
                  >
                    Remove
                  </button>
                )}
              </div>
              <ResolvedPath targets={targets} targetID={d.dest_target_id} subpath={d.dest_subpath ?? ''} />
            </label>
          </div>
        ))}
        <button
          type="button"
          className="link"
          onClick={() => set('destinations', [...job.destinations, { dest_target_id: '', dest_subpath: '' }])}
        >
          Add destination
        </button>
      </div>

      <div className="card">
        <h3>How to sync</h3>
        <div className="row">
          <label>
            Mode
            <select value={job.mode} onChange={(e) => set('mode', e.target.value as JobPayload['mode'])}>
              <option value="update">Update — copy new and changed files, never delete</option>
              <option value="mirror">Mirror — make the destination match, deleting extras</option>
            </select>
          </label>
          <label>
            Compare by
            <select value={job.compare} disabled>
              <option value="fast">Size and modified time</option>
              <option value="content" disabled>
                Contents — arrives in Phase 6
              </option>
            </select>
          </label>
        </div>

        <div className="row">
          <label>
            Files at a time <span className="hint">1–16</span>
            <input
              type="number"
              min={1}
              max={16}
              value={job.workers}
              onChange={(e) => set('workers', Number(e.target.value))}
            />
            <span className="hint">
              How many files are copied at once <em>within one destination</em>. This does not
              affect the order of destinations — that is the setting below, and it is off, so
              destinations are already done one after another in the order listed. Raising this
              makes the <em>first</em> destination finish sooner, at the cost of more load on the
              source.
            </span>
          </label>
          <label>
            Time tolerance <span className="hint">seconds; 0 is treated as 2</span>
            <input
              type="number"
              min={0}
              value={job.compare_tolerance_sec ?? 2}
              onChange={(e) => set('compare_tolerance_sec', Number(e.target.value))}
            />
          </label>
        </div>

        <label className="checkbox">
          <input
            type="checkbox"
            checked={job.ignore_dst_hour}
            onChange={(e) => set('ignore_dst_hour', e.target.checked)}
          />
          Treat a whole-hour difference as equal (daylight-saving artefact)
        </label>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={job.parallel_destinations}
            onChange={(e) => set('parallel_destinations', e.target.checked)}
          />
          Sync destinations in parallel <span className="hint">multiplies read load on the source</span>
        </label>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={job.log_every_file}
            onChange={(e) => set('log_every_file', e.target.checked)}
          />
          Log every file copied <span className="hint">overwrites and deletions are always logged</span>
        </label>
      </div>

      <div className="card">
        <h3>When something goes wrong</h3>
        <div className="row">
          <label>
            On a file error
            <select value={job.on_error} onChange={(e) => set('on_error', e.target.value as JobPayload['on_error'])}>
              <option value="skip">Skip it and carry on</option>
              <option value="abort">Abort the run</option>
            </select>
          </label>
          <label>
            If some files could not be copied
            <select
              value={job.delete_policy}
              onChange={(e) => set('delete_policy', e.target.value as JobPayload['delete_policy'])}
            >
              <option value="skip_deletes">Do not delete anything this run</option>
              <option value="proceed">Delete extras anyway</option>
            </select>
            <span className="hint">
              A failed copy can mean the source was only partly readable. Deleting on that basis is
              how a mirror removes files it should have kept.
            </span>
          </label>
        </div>

        <div className="card">
          <h3>Schedule</h3>
          <label>
            Run automatically
            <input
              value={job.schedule_cron ?? ''}
              placeholder="0 2 * * *"
              onChange={(e) => set('schedule_cron', e.target.value)}
            />
            <span className="hint">
              Five fields — minute, hour, day of month, month, day of week. <code>0 2 * * *</code> is
              2am every day; <code>30 6 * * 1-5</code> is 6:30am on weekdays. <code>@daily</code> and{' '}
              <code>@every 6h</code> also work. Leave it empty to run this job only when you ask.
            </span>
          </label>

          <NextRunPreview expression={job.schedule_cron ?? ''} enabled={job.enabled} />

          <label className="checkbox">
            <input
              type="checkbox"
              checked={job.enabled}
              onChange={(e) => set('enabled', e.target.checked)}
            />
            Scheduling is on
          </label>
          <span className="hint">
            Turning this off pauses the schedule without discarding it, and does not stop you running
            the job by hand. A paused job shows no next run at all rather than one it will not honour.
          </span>
        </div>

        <label>
          If a destination folder does not exist
          <select
            value={job.create_dest_dirs}
            onChange={(e) => set('create_dest_dirs', e.target.value as JobPayload['create_dest_dirs'])}
          >
            <option value="ask">Ask me before creating it</option>
            <option value="always">Create it automatically</option>
            <option value="never">Treat it as unavailable</option>
          </select>
          <span className="hint">
            A mistyped folder gets created and synced into, which looks exactly like success — so
            this asks by default. With no answer the destination is skipped, and nothing is created.
          </span>
        </label>

        <div className="row">
          <label>
            If a destination is unreachable
            <select
              value={job.unavailable_policy}
              onChange={(e) => set('unavailable_policy', e.target.value as UnavailablePolicy)}
            >
              <option value="skip">Skip it and carry on</option>
              <option value="abort">Fail the run</option>
              <option value="prompt">Ask me</option>
            </select>
          </label>
          {job.unavailable_policy === 'prompt' && (
            <label>
              Wait for an answer <span className="hint">5–86400 seconds</span>
              <input
                type="number"
                min={5}
                max={86400}
                value={job.prompt_timeout_sec}
                onChange={(e) => set('prompt_timeout_sec', Number(e.target.value))}
              />
            </label>
          )}
        </div>

        {job.unavailable_policy === 'prompt' && (
          <>
            <label>
              If nobody answers
              <select
                value={job.prompt_fallback}
                onChange={(e) => set('prompt_fallback', e.target.value as JobPayload['prompt_fallback'])}
              >
                <option value="skip">Skip that destination</option>
                <option value="abort">Fail the run</option>
              </select>
            </label>
            <p className="hint">
              This timeout also bounds how long a previewed run holds its plan before cancelling
              itself.
            </p>
          </>
        )}
      </div>
    </>
  )
}

function Filters({
  job,
  set,
  targets,
  destTargetIDs,
  ruleErrors,
  editing,
  dirty,
  testing,
  testResult,
  testError,
  onTest,
}: {
  job: JobPayload
  set: <K extends keyof JobPayload>(k: K, v: JobPayload[K]) => void
  targets: Target[]
  destTargetIDs: string[]
  ruleErrors: Record<number, string>
  editing: boolean
  dirty: boolean
  testing: boolean
  testResult: FilterTestResult | null
  testError: string | null
  onTest: () => void
}) {
  const hasInclude = job.filters.some((r) => r.direction === 'include')

  return (
    <>
      <div className="card">
        <p className="muted">
          Rules apply to both sides. A path excluded here is never copied <em>and</em> never deleted,
          so adding an exclude cannot cause a mirror to remove the files it now skips.
        </p>
        {hasInclude && (
          <p className="warn">
            This job has an include rule, so <strong>only</strong> paths matching an include are
            synced; excludes then apply on top. An include rule that ends up with no patterns admits
            nothing.
          </p>
        )}
      </div>

      {job.filters.map((rule, i) => (
        <FilterRuleRow
          key={i}
          rule={rule}
          index={i}
          destinationTargetIDs={destTargetIDs}
          targets={targets}
          error={ruleErrors[i]}
          onChange={(next) => set('filters', job.filters.map((r, j) => (j === i ? next : r)))}
          onRemove={() => set('filters', job.filters.filter((_, j) => j !== i))}
        />
      ))}

      <div className="card">
        <div className="actions">
          <button type="button" className="link" onClick={() => set('filters', [...job.filters, { ...NEW_RULE }])}>
            Add rule
          </button>
          <button type="button" disabled={!editing || dirty || testing} onClick={onTest}>
            {testing ? 'Testing…' : 'Test filters'}
          </button>
        </div>
        {!editing && <p className="hint">Save the job before testing its filters.</p>}
        {editing && dirty && (
          <p className="hint">
            Filter changes are unsaved. The test runs against the saved job, so save first to test
            what you see here.
          </p>
        )}
        {testError && <p className="error">{testError}</p>}
      </div>

      {testResult && <FilterTestReport result={testResult} targets={targets} />}
    </>
  )
}

function FilterTestReport({ result, targets }: { result: FilterTestResult; targets: Target[] }) {
  const nameFor = (id: string) => targets.find((t) => t.id === id)?.name ?? id

  return (
    <div className="card">
      <h3>Filter test</h3>
      <p className="muted">
        Scanned {result.scanned_paths} paths
        {result.truncated ? ' (stopped at the scan cap, so this is part of the tree)' : ''}.
      </p>

      {result.destinations.map((d) => (
        <div key={d.dest_target_id} className="plan-dest">
          <h4>{nameFor(d.dest_target_id)}</h4>
          <p className="muted">
            {d.included_total} included · {d.excluded_total} excluded
          </p>

          <table className="counts">
            <thead>
              <tr>
                <th>Rule</th>
                <th>Direction</th>
                <th>Admitted</th>
                <th>Excluded</th>
              </tr>
            </thead>
            <tbody>
              {/* Keyed by rule_id: this list is ordered includes-then-excludes,
                  not by the row order in the editor above. */}
              {d.rules.map((r) => (
                <tr key={r.rule_id}>
                  <td>{r.description}</td>
                  <td>{r.direction}</td>
                  <td>{r.admitted}</td>
                  <td>{r.excluded}</td>
                </tr>
              ))}
            </tbody>
          </table>

          {d.rules.length < 1 && <p className="muted">No rules were applied.</p>}

          <details>
            <summary>Excluded sample ({d.excluded.length} of {d.excluded_total})</summary>
            <ul className="paths">
              {d.excluded.map((s) => (
                <li key={s.relpath}>
                  <code>{s.relpath}</code>{' '}
                  <span className="muted">
                    {/* An empty `rule` means no include matched, which is not
                        the same as "no rule was responsible". */}
                    {s.rule ? s.rule : 'outside the include set'}
                  </span>
                </li>
              ))}
            </ul>
          </details>

          <details>
            <summary>Included sample ({d.included.length} of {d.included_total})</summary>
            <ul className="paths">
              {d.included.map((s) => (
                <li key={s.relpath}>
                  <code>{s.relpath}</code>
                </li>
              ))}
            </ul>
          </details>
        </div>
      ))}
    </div>
  )
}

/** What "unsaved" means for the filter test: everything the test actually
 *  reads — the rules, and the source and destinations it scans. */
function snapshotOf(job: JobPayload): string {
  return JSON.stringify({
    filters: job.filters,
    source_target_id: job.source_target_id,
    source_subpath: job.source_subpath ?? '',
    destinations: job.destinations,
  })
}

/**
 * What a cron expression actually resolves to, asked of the server.
 *
 * A cron string cannot be checked by reading it. Whether "0 2 * * *" fires at
 * 2am where you live depends on the server's timezone, which the expression
 * does not mention; and "0 0 30 2 *" — the 30th of February — parses perfectly
 * and never happens. Showing the next three firings makes both answerable at a
 * glance, and makes a weekly schedule distinguishable from a daily one, which
 * a single date cannot.
 */
function NextRunPreview({ expression, enabled }: { expression: string; enabled: boolean }) {
  const [preview, setPreview] = useState<SchedulePreview | null>(null)

  useEffect(() => {
    const trimmed = expression.trim()
    if (!trimmed) {
      setPreview(null)
      return
    }
    // Debounced: this fires on every keystroke, and most keystrokes are
    // half-written expressions nobody needs an answer about.
    let cancelled = false
    const timer = window.setTimeout(() => {
      void api
        .schedulePreview(trimmed)
        .then((p) => {
          if (!cancelled) setPreview(p)
        })
        // Advisory only. A failed lookup must not make the field look invalid,
        // because the save will still succeed.
        .catch(() => {})
    }, 300)

    return () => {
      cancelled = true
      window.clearTimeout(timer)
    }
  }, [expression])

  if (!expression.trim() || !preview) return null

  if (!preview.valid) {
    return <p className="error">{preview.error}</p>
  }
  if (preview.next.length === 0) {
    return (
      <p className="warn">
        That is a valid expression for a date that never occurs, so this job would never run.
      </p>
    )
  }

  // The server's zone is what the expression is read in; the browser's is
  // what the reader recognises. Showing only the second is what makes
  // "16 2 * * *" appear as 7:16pm and read as a bug.
  const here = preview.next_here.join(', ')
  const yours = preview.next.map((t) => timestamp(t)).join(', ')

  return (
    <p className={enabled ? 'hint' : 'warn'}>
      {enabled ? 'Next runs' : 'Would run'} <strong>{here}</strong>{' '}
      <code>{preview.timezone}</code> — the server's clock, which is what the expression means.
      {!enabled && ' Scheduling is off, so it will not.'}
      <br />
      In your timezone that is <strong>{yours}</strong>. If the server's clock is not the one you
      meant, set <code>TZ</code> on the container and these will agree.
    </p>
  )
}

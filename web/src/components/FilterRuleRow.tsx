import { useEffect, useState } from 'react'
import type {
  FilterDirection,
  FilterOnError,
  FilterRulePayload,
  FilterScope,
  FilterSource,
  Target,
} from '../api/types'
import { RuleFileField } from './RuleFileField'

interface Props {
  rule: FilterRulePayload
  index: number
  /** The job's current, unsaved destinations — the server validates a
   *  target-scoped rule against these, so the dropdown must match. */
  destinationTargetIDs: string[]
  targets: Target[]
  /** Set when the server blamed this specific rule ("filter rule N: ..."). */
  error?: string
  onChange: (rule: FilterRulePayload) => void
  onRemove: () => void
}

const SOURCE_LABEL: Record<FilterSource, string> = {
  inline: 'Patterns typed here',
  listfile: 'A list file (one pattern per line)',
  jsonfile: 'A JSON file (patterns under a key)',
}

export function FilterRuleRow({
  rule,
  index,
  destinationTargetIDs,
  targets,
  error,
  onChange,
  onRemove,
}: Props) {
  const nameFor = (id: string) => targets.find((t) => t.id === id)?.name ?? id

  // Switching source or scope must clear the fields the new combination
  // forbids: the server rejects a listfile rule carrying patterns, a jsonfile
  // key on a listfile rule, and a scope_target_id on a job-scoped rule. Left
  // in place they become validation errors the user cannot see the cause of.
  const setSource = (source: FilterSource) =>
    onChange({
      ...rule,
      source,
      patterns: source === 'inline' ? (rule.patterns ?? []) : undefined,
      file_path: source === 'inline' ? undefined : (rule.file_path ?? ''),
      json_key: source === 'jsonfile' ? (rule.json_key ?? '') : undefined,
    })

  const setScope = (scope: FilterScope) =>
    onChange({
      ...rule,
      scope,
      scope_target_id: scope === 'target' ? (rule.scope_target_id ?? destinationTargetIDs[0]) : undefined,
    })

  return (
    <div className={`card filter-rule${error ? ' invalid' : ''}`}>
      <div className="rule-head">
        <strong>Rule {index + 1}</strong>
        <button type="button" className="link danger" onClick={onRemove}>
          Remove
        </button>
      </div>

      <div className="row">
        <label>
          Direction
          <select
            value={rule.direction}
            onChange={(e) => onChange({ ...rule, direction: e.target.value as FilterDirection })}
          >
            <option value="exclude">Exclude — skip what matches</option>
            <option value="include">Include — only what matches</option>
          </select>
        </label>

        <label>
          Applies to
          <select value={rule.scope} onChange={(e) => setScope(e.target.value as FilterScope)}>
            <option value="job">The whole job</option>
            <option value="target" disabled={destinationTargetIDs.length === 0}>
              One destination
            </option>
          </select>
        </label>
      </div>

      {rule.scope === 'target' && (
        <label>
          Destination
          <select
            value={rule.scope_target_id ?? ''}
            onChange={(e) => onChange({ ...rule, scope_target_id: e.target.value })}
          >
            {/* Without this, a scope_target_id that is no longer one of the
                job's destinations makes the browser select the first option —
                the row would then display one destination while the rule still
                carries another, and no change event fires to correct it. */}
            <option value="">Choose…</option>
            {destinationTargetIDs.map((id) => (
              <option key={id} value={id}>
                {nameFor(id)}
              </option>
            ))}
          </select>
          {rule.scope_target_id !== undefined &&
            rule.scope_target_id !== '' &&
            !destinationTargetIDs.includes(rule.scope_target_id) && (
              <span className="error">
                This rule is scoped to a destination the job no longer has. Pick another.
              </span>
            )}
        </label>
      )}

      <label>
        Patterns come from
        <select value={rule.source} onChange={(e) => setSource(e.target.value as FilterSource)}>
          {(Object.keys(SOURCE_LABEL) as FilterSource[]).map((s) => (
            <option key={s} value={s}>
              {SOURCE_LABEL[s]}
            </option>
          ))}
        </select>
      </label>

      {rule.source === 'inline' ? (
        <PatternsField rule={rule} onChange={onChange} />
      ) : (
        <>
          <RuleFileField
            value={rule.file_path ?? ''}
            onChange={(v) => onChange({ ...rule, file_path: v })}
          />
          {rule.source === 'jsonfile' && (
            <label>
              JSON key
              <input
                value={rule.json_key ?? ''}
                placeholder="backup.exclude"
                onChange={(e) => onChange({ ...rule, json_key: e.target.value })}
              />
              <span className="hint">
                Dot-path to the list inside the file. Numeric segments index arrays.
              </span>
            </label>
          )}
        </>
      )}

      <div className="row">
        <label className="checkbox">
          <input
            type="checkbox"
            checked={rule.case_sensitive}
            onChange={(e) => onChange({ ...rule, case_sensitive: e.target.checked })}
          />
          Case sensitive
        </label>

        <label>
          If the rule cannot be loaded
          <select
            value={rule.on_error}
            onChange={(e) => onChange({ ...rule, on_error: e.target.value as FilterOnError })}
          >
            <option value="fail_run">Fail the run</option>
            <option value="ignore_rule">Ignore the rule</option>
          </select>
        </label>
      </div>

      {rule.on_error === 'ignore_rule' && (
        <p className="warn">
          Ignoring a rule widens what the job sees. To stay safe the run then performs{' '}
          <strong>no deletions at all</strong> and finishes <code>partial</code>.
        </p>
      )}

      {error && <p className="error">{error}</p>}
    </div>
  )
}


/**
 * The patterns textarea keeps its own raw text.
 *
 * Deriving `value` from the parsed pattern array does not work: parsing strips
 * blank lines, so pressing Enter at the end of a line produces text that parses
 * back to the same array, React re-asserts the old DOM value, and the newline
 * is erased as it is typed. Two patterns then run together into one that
 * matches nothing — and a filter rule that matches nothing is a rule that
 * silently stops protecting whatever it named.
 */
function PatternsField({
  rule,
  onChange,
}: {
  rule: FilterRulePayload
  onChange: (rule: FilterRulePayload) => void
}) {
  const [text, setText] = useState(() => (rule.patterns ?? []).join('\n'))

  // Resync only when the rule's patterns diverge from what this text parses
  // to — which happens on load or a source switch, not while typing.
  useEffect(() => {
    const external = (rule.patterns ?? []).join('\u0000')
    if (external !== parsePatterns(text).join('\u0000')) {
      setText((rule.patterns ?? []).join('\n'))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rule.patterns])

  return (
    <label>
      Patterns <span className="hint">one per line</span>
      <textarea
        rows={4}
        value={text}
        placeholder={'*.tmp\ncache/\n/build'}
        onChange={(e) => {
          setText(e.target.value)
          onChange({ ...rule, patterns: parsePatterns(e.target.value) })
        }}
      />
      <span className="hint">
        gitignore style: a leading <code>/</code> anchors to the sync root, a trailing{' '}
        <code>/</code> means directories only, and a bare name matches at any depth. Unlike a list
        file, <strong>a <code>#</code> line here is a pattern, not a comment</strong>.
      </span>
    </label>
  )
}

/** Blank lines are dropped; everything else is kept verbatim apart from
 *  surrounding whitespace. */
function parsePatterns(text: string): string[] {
  return text
    .split('\n')
    .map((p) => p.trim())
    .filter((p) => p !== '')
}

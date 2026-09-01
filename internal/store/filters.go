package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FilterScope says which destinations a rule applies to (SPEC.md §6.5).
type FilterScope string

const (
	// ScopeJob applies to every destination of the job.
	ScopeJob FilterScope = "job"
	// ScopeTarget applies only to the destination it names, so two
	// destinations of one job can receive different subsets.
	ScopeTarget FilterScope = "target"
)

// FilterDirection is whether a rule admits or removes paths.
type FilterDirection string

const (
	FilterInclude FilterDirection = "include"
	FilterExclude FilterDirection = "exclude"
)

// FilterSource is where a rule's patterns come from.
type FilterSource string

const (
	// SourceInline holds patterns directly in the rule.
	SourceInline FilterSource = "inline"
	// SourceListFile reads one pattern per line from a text file.
	SourceListFile FilterSource = "listfile"
	// SourceJSONFile reads a list from a JSON file at a dot-path key.
	SourceJSONFile FilterSource = "jsonfile"
)

// FilterErrorPolicy is what a broken rule does at run time.
type FilterErrorPolicy string

const (
	// FilterFailRun is the default for excludes: silently syncing files the
	// user meant to exclude is the dangerous direction (SPEC.md §6.5).
	FilterFailRun FilterErrorPolicy = "fail_run"
	// FilterIgnoreRule drops the rule and carries on.
	FilterIgnoreRule FilterErrorPolicy = "ignore_rule"
)

// FilterRule is one rule in a job's filter chain.
type FilterRule struct {
	ID    string `json:"id"`
	JobID string `json:"job_id"`

	Scope         FilterScope `json:"scope"`
	ScopeTargetID string      `json:"scope_target_id,omitempty"`

	Direction FilterDirection `json:"direction"`
	Source    FilterSource    `json:"source"`

	// Patterns is used when Source is inline.
	Patterns []string `json:"patterns,omitempty"`
	// FilePath is used when Source is listfile or jsonfile. It may be a
	// container path, or "target://<target-id>/path" to read from a share.
	FilePath string `json:"file_path,omitempty"`
	// JSONKey is the dot-path into a JSON file, e.g. "backup.exclude" or
	// "0.skip".
	JSONKey string `json:"json_key,omitempty"`

	CaseSensitive bool              `json:"case_sensitive"`
	OnError       FilterErrorPolicy `json:"on_error"`
	Position      int               `json:"position"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TargetRef is the "target://<id>/<path>" form a rule file may use.
const TargetRefPrefix = "target://"

// ParseTargetRef splits a target:// reference into its target and path. It
// reports ok=false for an ordinary container path.
func ParseTargetRef(filePath string) (targetID, path string, ok bool) {
	if !strings.HasPrefix(filePath, TargetRefPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(filePath, TargetRefPrefix)
	targetID, path, found := strings.Cut(rest, "/")
	if !found || targetID == "" || path == "" {
		return "", "", false
	}
	return targetID, path, true
}

// ApplyDefaults fills in what the API lets a caller omit.
func (f *FilterRule) ApplyDefaults() {
	if f.Scope == "" {
		f.Scope = ScopeJob
	}
	if f.OnError == "" {
		// fail_run for both directions. SPEC.md §6.5 asks for it on
		// excludes; includes need it at least as much, because dropping the
		// only include rule leaves an empty include set — and an empty
		// include set admits *everything*, which in mirror mode makes every
		// out-of-scope destination file extraneous.
		f.OnError = FilterFailRun
	}
}

// Validate checks one rule. Messages are user-facing.
func (f *FilterRule) Validate() error {
	switch f.Scope {
	case ScopeJob:
		if f.ScopeTargetID != "" {
			return errors.New("scope_target_id is only valid when scope is \"target\"")
		}
	case ScopeTarget:
		if f.ScopeTargetID == "" {
			return errors.New("scope_target_id is required when scope is \"target\"")
		}
	default:
		return fmt.Errorf("scope must be %q or %q, got %q", ScopeJob, ScopeTarget, f.Scope)
	}

	switch f.Direction {
	case FilterInclude, FilterExclude:
	default:
		return fmt.Errorf("direction must be %q or %q, got %q", FilterInclude, FilterExclude, f.Direction)
	}

	switch f.OnError {
	case FilterFailRun, FilterIgnoreRule:
	default:
		return fmt.Errorf("on_error must be %q or %q, got %q", FilterFailRun, FilterIgnoreRule, f.OnError)
	}

	switch f.Source {
	case SourceInline:
		if len(f.Patterns) == 0 {
			return errors.New("an inline rule needs at least one pattern")
		}
		if f.FilePath != "" || f.JSONKey != "" {
			return errors.New("file_path and json_key are only valid for a listfile or jsonfile rule")
		}

	case SourceListFile:
		if strings.TrimSpace(f.FilePath) == "" {
			return errors.New("a listfile rule needs file_path")
		}
		if f.JSONKey != "" {
			return errors.New("json_key is only valid for a jsonfile rule")
		}

	case SourceJSONFile:
		if strings.TrimSpace(f.FilePath) == "" {
			return errors.New("a jsonfile rule needs file_path")
		}
		if strings.TrimSpace(f.JSONKey) == "" {
			return errors.New("a jsonfile rule needs json_key, the dot-path to the pattern list inside the file")
		}

	default:
		return fmt.Errorf("source must be %q, %q or %q, got %q",
			SourceInline, SourceListFile, SourceJSONFile, f.Source)
	}

	if f.Source != SourceInline {
		if ref := strings.TrimSpace(f.FilePath); strings.HasPrefix(ref, TargetRefPrefix) {
			if _, _, ok := ParseTargetRef(ref); !ok {
				return fmt.Errorf("file_path %q is not a valid target reference; expected target://<target-id>/path/to/file", f.FilePath)
			}
		}
	}
	return nil
}

// Describe names a rule in the run log.
func (f *FilterRule) Describe() string {
	switch f.Source {
	case SourceInline:
		return fmt.Sprintf("%s (inline, %d pattern(s))", f.Direction, len(f.Patterns))
	case SourceListFile:
		return fmt.Sprintf("%s (list file %s)", f.Direction, f.FilePath)
	default:
		return fmt.Sprintf("%s (JSON file %s key %s)", f.Direction, f.FilePath, f.JSONKey)
	}
}

const filterColumns = `id, job_id, scope, scope_target_id, direction, source,
	patterns_json, file_path, json_key, case_sensitive, on_error, position,
	created_at, updated_at`

// insertFilterRule adds one rule inside an existing transaction.
func insertFilterRule(ctx context.Context, tx execer, jobID string, f *FilterRule, position int) error {
	f.ApplyDefaults()
	if err := f.Validate(); err != nil {
		return err
	}

	id, err := newID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	f.ID, f.JobID, f.Position = id, jobID, position
	f.CreatedAt, f.UpdatedAt = now, now

	encoded, err := json.Marshal(f.Patterns)
	if err != nil {
		return fmt.Errorf("encoding the patterns of a filter rule: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO filter_rules (`+filterColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.ID, jobID, string(f.Scope), f.ScopeTargetID, string(f.Direction), string(f.Source),
		string(encoded), f.FilePath, f.JSONKey, boolToInt(f.CaseSensitive), string(f.OnError),
		position, formatTime(f.CreatedAt), formatTime(f.UpdatedAt))
	if err != nil {
		return fmt.Errorf("adding a filter rule to job %s: %w", jobID, err)
	}
	return nil
}

// ListFilterRules returns a job's rules in evaluation order.
func (d *DB) ListFilterRules(ctx context.Context, jobID string) ([]FilterRule, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+filterColumns+` FROM filter_rules WHERE job_id = ? ORDER BY position, id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("loading the filter rules of job %s: %w", jobID, err)
	}
	defer rows.Close()

	rules := []FilterRule{}
	for rows.Next() {
		var (
			f                                 FilterRule
			scope, direction, source, onError string
			patternsJSON, created, updated    string
			caseSensitive                     int
		)
		if err := rows.Scan(&f.ID, &f.JobID, &scope, &f.ScopeTargetID, &direction, &source,
			&patternsJSON, &f.FilePath, &f.JSONKey, &caseSensitive, &onError, &f.Position,
			&created, &updated); err != nil {
			return nil, fmt.Errorf("loading the filter rules of job %s: %w", jobID, err)
		}

		f.Scope = FilterScope(scope)
		f.Direction = FilterDirection(direction)
		f.Source = FilterSource(source)
		f.OnError = FilterErrorPolicy(onError)
		f.CaseSensitive = caseSensitive != 0
		f.CreatedAt = parseTime(created)
		f.UpdatedAt = parseTime(updated)

		if err := json.Unmarshal([]byte(patternsJSON), &f.Patterns); err != nil {
			return nil, fmt.Errorf("decoding the patterns of filter rule %s: %w", f.ID, err)
		}
		rules = append(rules, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading the filter rules of job %s: %w", jobID, err)
	}
	return rules, nil
}

// execer is satisfied by both *sql.DB and *sql.Tx, so a rule can be inserted
// inside the same transaction that creates its job.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

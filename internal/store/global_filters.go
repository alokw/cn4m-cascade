package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// GlobalFilterRule is an exclusion applied to every job (SPEC.md §6.5).
//
// It is deliberately a narrower thing than FilterRule: no scope, because a
// global rule is job-wide by definition, and no direction, because these are
// exclusions only. A global *include* would widen every job rather than narrow
// it — includes are OR'd, so a global "*.txt" plus a job's "*.jpg" admits both,
// which is the opposite of what a global filter means to anyone.
//
// Global excludes are absolute: a job's own include rule cannot re-admit a path
// a global rule removed. That falls out of how filter.Chain evaluates — excludes
// are applied unconditionally after includes — and making it otherwise would
// mean rewriting the code that guarantees PrunesDir and Admits agree, which is
// what stops a mirror deleting a subtree one side cannot see.
type GlobalFilterRule struct {
	ID string `json:"id"`

	Source        FilterSource      `json:"source"`
	Patterns      []string          `json:"patterns,omitempty"`
	FilePath      string            `json:"file_path,omitempty"`
	JSONKey       string            `json:"json_key,omitempty"`
	CaseSensitive bool              `json:"case_sensitive"`
	OnError       FilterErrorPolicy `json:"on_error"`

	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AsFilterRule projects a global rule onto the per-job rule type, so the
// compiler, the chain and the run log treat it like any other exclusion. The
// job-only fields are left zero: Scope is job-wide, which is what allows a
// global rule to prune the shared source walk.
func (g *GlobalFilterRule) AsFilterRule() FilterRule {
	return FilterRule{
		ID:            g.ID,
		Scope:         ScopeJob,
		Direction:     FilterExclude,
		Source:        g.Source,
		Patterns:      g.Patterns,
		FilePath:      g.FilePath,
		JSONKey:       g.JSONKey,
		CaseSensitive: g.CaseSensitive,
		OnError:       g.OnError,
		Position:      g.Position,
	}
}

// ApplyDefaults fills in what the API lets a caller omit.
func (g *GlobalFilterRule) ApplyDefaults() {
	if g.OnError == "" {
		g.OnError = FilterFailRun
	}
}

// Validate reuses FilterRule's rules for the fields the two share, so the two
// paths cannot drift on what a listfile or jsonfile rule requires.
func (g *GlobalFilterRule) Validate() error {
	r := g.AsFilterRule()
	r.ApplyDefaults()
	return r.Validate()
}

// Describe names the rule in a run log.
func (g *GlobalFilterRule) Describe() string {
	r := g.AsFilterRule()
	return "global " + r.Describe()
}

const globalFilterColumns = `id, source, patterns_json, file_path, json_key,
	case_sensitive, on_error, position, created_at, updated_at`

// ListGlobalFilterRules returns every global rule in evaluation order.
func (d *DB) ListGlobalFilterRules(ctx context.Context) ([]GlobalFilterRule, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+globalFilterColumns+` FROM global_filter_rules ORDER BY position, id`)
	if err != nil {
		return nil, fmt.Errorf("loading the global filter rules: %w", err)
	}
	defer rows.Close()

	out := []GlobalFilterRule{}
	for rows.Next() {
		var (
			g            GlobalFilterRule
			source, oerr string
			patterns     string
			caseSens     int
			created, upd string
		)
		if err := rows.Scan(&g.ID, &source, &patterns, &g.FilePath, &g.JSONKey,
			&caseSens, &oerr, &g.Position, &created, &upd); err != nil {
			return nil, fmt.Errorf("reading a global filter rule: %w", err)
		}
		if err := json.Unmarshal([]byte(patterns), &g.Patterns); err != nil {
			return nil, fmt.Errorf("decoding the patterns of global filter rule %s: %w", g.ID, err)
		}
		g.Source = FilterSource(source)
		g.OnError = FilterErrorPolicy(oerr)
		g.CaseSensitive = caseSens != 0
		g.CreatedAt, g.UpdatedAt = parseTime(created), parseTime(upd)
		out = append(out, g)
	}
	return out, rows.Err()
}

// ReplaceGlobalFilterRules swaps the whole set in one transaction.
//
// Wholesale replacement, like a job's rules: they are positional and there is
// no stable identity to diff against — reordering the list *is* how you reorder
// evaluation. Doing it in a transaction matters more here than for a job,
// because a half-applied global list applies to every job at once.
func (d *DB) ReplaceGlobalFilterRules(ctx context.Context, rules []GlobalFilterRule) error {
	for i := range rules {
		rules[i].ApplyDefaults()
		if err := rules[i].Validate(); err != nil {
			return fmt.Errorf("global filter rule %d: %w", i+1, err)
		}
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replacing the global filter rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM global_filter_rules`); err != nil {
		return fmt.Errorf("clearing the global filter rules: %w", err)
	}

	now := time.Now().UTC()
	for i := range rules {
		id, err := newID()
		if err != nil {
			return err
		}
		g := &rules[i]
		g.ID, g.Position = id, i
		g.CreatedAt, g.UpdatedAt = now, now

		encoded, err := json.Marshal(g.Patterns)
		if err != nil {
			return fmt.Errorf("encoding the patterns of a global filter rule: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO global_filter_rules (`+globalFilterColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			g.ID, string(g.Source), string(encoded), g.FilePath, g.JSONKey,
			boolToInt(g.CaseSensitive), string(g.OnError), g.Position,
			formatTime(g.CreatedAt), formatTime(g.UpdatedAt)); err != nil {
			return fmt.Errorf("adding a global filter rule: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replacing the global filter rules: %w", err)
	}
	return nil
}

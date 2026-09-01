package filter

import (
	"fmt"
	"strings"
	"sync"
)

// Direction says whether a rule admits or removes paths.
type Direction string

const (
	Include Direction = "include"
	Exclude Direction = "exclude"
)

// Rule is one compiled filter rule with its patterns.
//
// Patterns are immutable once compiled and are shared between chains, but the
// counters are not: each chain gets its own Rule via clone, so a job-scoped
// rule tallies separately per destination and two destinations running in
// parallel never touch the same counter.
type Rule struct {
	// ID identifies the rule for per-rule counters in the run log.
	ID string
	// Description is what the run log calls this rule.
	Description string

	Direction Direction
	Patterns  []*Pattern

	mu       sync.Mutex
	admitted uint64
	excluded uint64
}

// clone returns a rule sharing the compiled patterns but with fresh counters.
func (r *Rule) clone() *Rule {
	return &Rule{
		ID:          r.ID,
		Description: r.Description,
		Direction:   r.Direction,
		Patterns:    r.Patterns,
	}
}

// Counts reports how many paths a rule admitted or excluded. SPEC.md §6.5
// asks for counts rather than full path dumps.
type Counts struct {
	RuleID      string `json:"rule_id"`
	Description string `json:"description"`
	Direction   string `json:"direction"`
	Admitted    uint64 `json:"admitted"`
	Excluded    uint64 `json:"excluded"`
}

// Chain is the resolved filter for one destination: the job-level rules with
// that destination's own rules layered on top.
//
// The zero value admits everything, which is what a job with no rules means.
type Chain struct {
	includes []*Rule
	excludes []*Rule

	// degraded records that a rule could not be loaded and was dropped
	// under its ignore_rule policy. A dropped rule *widens* scope, which in
	// mirror mode turns previously-excluded destination files into
	// extraneous ones — so a degraded chain must never be allowed to
	// delete. See Degraded.
	degraded       bool
	degradedReason string
}

// NewChain builds a chain from rules already in evaluation order. Each rule
// is cloned so the chain owns its own counters.
func NewChain(rules []*Rule) *Chain {
	c := &Chain{}
	for _, r := range rules {
		clone := r.clone()
		if clone.Direction == Include {
			c.includes = append(c.includes, clone)
		} else {
			c.excludes = append(c.excludes, clone)
		}
	}
	return c
}

// MarkDegraded records that a rule was dropped rather than applied.
func (c *Chain) MarkDegraded(reason string) {
	c.degraded = true
	if c.degradedReason == "" {
		c.degradedReason = reason
	}
}

// Degraded reports whether any rule was dropped, and why.
//
// The differ refuses to delete anything from a degraded chain. Dropping an
// exclude puts previously-protected destination files back in scope, where
// mirror sees them missing from the source and removes them; dropping the
// only include rule is worse still, because an empty include set admits
// everything.
func (c *Chain) Degraded() (bool, string) {
	if c == nil {
		return false, ""
	}
	return c.degraded, c.degradedReason
}

// Empty reports whether the chain would admit everything.
func (c *Chain) Empty() bool { return len(c.includes) == 0 && len(c.excludes) == 0 }

// Admits reports whether a path is in scope, tallying the rule that decided.
//
// SPEC.md §6.5's evaluation order: if any include rules exist a path must
// match at least one of them, and excludes then win over includes.
func (c *Chain) Admits(relPath string, isDir bool) bool {
	admitted, rule := c.decide(relPath, isDir)
	if rule != nil {
		rule.count(admitted)
	}
	return admitted
}

// Explain is Admits with the reason: it names the rule that decided, so a
// filter preview can show why a path is in or out. It tallies too, so a
// preview built on its own chain reports real counts.
func (c *Chain) Explain(relPath string, isDir bool) (bool, *Rule) {
	admitted, rule := c.decide(relPath, isDir)
	if rule != nil {
		rule.count(admitted)
	}
	return admitted, rule
}

// decide is the evaluation itself, without side effects.
func (c *Chain) decide(relPath string, isDir bool) (bool, *Rule) {
	if c == nil || c.Empty() {
		return true, nil
	}

	var matchedInclude *Rule
	if len(c.includes) > 0 {
		for _, r := range c.includes {
			if r.matches(relPath, isDir) {
				matchedInclude = r
				break
			}
		}
		if matchedInclude == nil {
			// Includes define the universe; nothing matched, so this path
			// is outside it. No single rule is to blame.
			return false, nil
		}
	}

	for _, r := range c.excludes {
		if r.matches(relPath, isDir) {
			return false, r
		}
	}
	return true, matchedInclude
}

// PrunesDir reports whether a directory is excluded outright, so the walk can
// skip descending into it.
//
// It must agree with Admits about everything beneath that directory —
// otherwise one side of the diff sees a subtree the other does not, and in
// mirror mode that difference deletes. Rule.matches checks ancestors for
// every pattern, which is what keeps the two consistent.
func (c *Chain) PrunesDir(relPath string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.excludes {
		if r.matches(relPath, true) {
			// Pruning skips an entire subtree, so it is counted once here;
			// the paths beneath are never visited to be counted.
			r.count(false)
			return true
		}
	}
	return false
}

// Counts returns the per-rule tallies for the run log.
func (c *Chain) Counts() []Counts {
	if c == nil {
		return nil
	}

	rules := append(append([]*Rule{}, c.includes...), c.excludes...)
	out := make([]Counts, 0, len(rules))
	for _, r := range rules {
		r.mu.Lock()
		out = append(out, Counts{
			RuleID:      r.ID,
			Description: r.Description,
			Direction:   string(r.Direction),
			Admitted:    r.admitted,
			Excluded:    r.excluded,
		})
		r.mu.Unlock()
	}
	return out
}

// matches reports whether any of a rule's patterns select the path.
//
// A rule with no patterns matches nothing. For an include rule that means an
// empty universe — nothing is in scope — which is the safe reading of an
// empty pattern list: it copies nothing rather than admitting everything.
func (r *Rule) matches(relPath string, isDir bool) bool {
	for _, p := range r.Patterns {
		if p.Match(relPath, isDir) {
			return true
		}
		// A pattern that matches a directory selects everything beneath it,
		// whether or not it was written with a trailing slash. Restricting
		// this to trailing-slash patterns would make "cache" prune the walk
		// but still admit "cache/x.bin".
		if p.MatchesAncestor(relPath) {
			return true
		}
	}
	return false
}

func (r *Rule) count(admitted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if admitted {
		r.admitted++
		return
	}
	r.excluded++
}

// PatternCount reports how many patterns a rule compiled to.
func (r *Rule) PatternCount() int { return len(r.Patterns) }

// CompileRule turns raw patterns into a Rule.
//
// An empty result is not an error: an empty JSON array, or a list file of
// nothing but comments, is ordinary live configuration. The rule simply
// matches nothing, which for an exclude means it removes nothing and for an
// include means the universe is empty.
func CompileRule(id, description string, direction Direction, patterns []string, caseSensitive bool) (*Rule, error) {
	rule := &Rule{ID: id, Description: description, Direction: direction}

	for _, raw := range patterns {
		line := strings.TrimSpace(raw)
		// List files allow comments and blank lines (SPEC.md §6.5).
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := Compile(line, caseSensitive)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", description, err)
		}
		rule.Patterns = append(rule.Patterns, p)
	}
	return rule, nil
}

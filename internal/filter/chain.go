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
type Rule struct {
	// ID identifies the rule for per-rule counters in the run log.
	ID string
	// Description is what the run log calls this rule.
	Description string

	Direction Direction
	Patterns  []*Pattern

	admitted uint64
	excluded uint64
}

// Counts reports how many paths this rule admitted or excluded. SPEC.md §6.5
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
	mu sync.Mutex

	includes []*Rule
	excludes []*Rule
}

// NewChain builds a chain from rules already in evaluation order.
func NewChain(rules []*Rule) *Chain {
	c := &Chain{}
	for _, r := range rules {
		if r.Direction == Include {
			c.includes = append(c.includes, r)
		} else {
			c.excludes = append(c.excludes, r)
		}
	}
	return c
}

// Empty reports whether the chain would admit everything.
func (c *Chain) Empty() bool { return len(c.includes) == 0 && len(c.excludes) == 0 }

// Admits reports whether a path is in scope.
//
// SPEC.md §6.5's evaluation order: if any include rules exist a path must
// match at least one of them, and excludes then win over includes.
func (c *Chain) Admits(relPath string, isDir bool) bool {
	if c == nil || c.Empty() {
		return true
	}

	if len(c.includes) > 0 {
		var included bool
		for _, r := range c.includes {
			if r.matches(relPath, isDir) {
				r.count(true)
				included = true
			}
		}
		if !included {
			return false
		}
	}

	for _, r := range c.excludes {
		if r.matches(relPath, isDir) {
			r.count(false)
			return false
		}
	}
	return true
}

// PrunesDir reports whether a directory is excluded outright, so the walk can
// skip descending into it. Only exclude rules can prune: an include rule says
// nothing about what lies beneath a directory that does not itself match.
func (c *Chain) PrunesDir(relPath string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.excludes {
		for _, p := range r.Patterns {
			if p.Match(relPath, true) {
				return true
			}
		}
	}
	return false
}

// Counts returns the per-rule tallies for the run log.
func (c *Chain) Counts() []Counts {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]Counts, 0, len(c.includes)+len(c.excludes))
	for _, r := range append(append([]*Rule{}, c.includes...), c.excludes...) {
		out = append(out, Counts{
			RuleID:      r.ID,
			Description: r.Description,
			Direction:   string(r.Direction),
			Admitted:    r.admitted,
			Excluded:    r.excluded,
		})
	}
	return out
}

func (r *Rule) matches(relPath string, isDir bool) bool {
	for _, p := range r.Patterns {
		if p.Match(relPath, isDir) {
			return true
		}
		// A directory rule removes everything beneath it, so a file deep
		// inside an excluded directory is excluded too.
		if p.DirOnly() && p.MatchesAncestor(relPath) {
			return true
		}
	}
	return false
}

func (r *Rule) count(admitted bool) {
	if admitted {
		r.admitted++
		return
	}
	r.excluded++
}

// CompileRule turns raw patterns into a Rule.
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

	if len(rule.Patterns) == 0 {
		return nil, fmt.Errorf("rule %s has no usable patterns", description)
	}
	return rule, nil
}

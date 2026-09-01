// Package filter decides which paths are in scope for a sync (SPEC.md §6.5).
//
// A path that a filter excludes is out of scope on *both* sides: it is never
// copied, never deleted, and never compared. Treating an excluded path as
// "missing from the source" would make mirror delete it from the
// destination, so adding a rule to skip copying a directory would silently
// destroy the backup of that directory.
package filter

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Pattern is one compiled gitignore-style rule.
//
// The glob mechanics come from doublestar, which is well tested; what is
// implemented here is the surrounding gitignore *semantics* — anchoring, the
// trailing-slash directory rule, and matching at any depth — which no glob
// library provides on its own.
type Pattern struct {
	// raw is the pattern as the user wrote it, for error messages.
	raw string
	// glob is the normalised pattern handed to the matcher.
	glob string
	// anchored patterns start with "/" and match only from the sync root.
	anchored bool
	// dirOnly patterns end with "/" and match directories, pruning the
	// whole subtree beneath them.
	dirOnly bool
	// caseSensitive selects whether the comparison is folded.
	caseSensitive bool
}

// Compile prepares one pattern for matching.
func Compile(raw string, caseSensitive bool) (*Pattern, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	p := &Pattern{raw: raw, caseSensitive: caseSensitive}

	if strings.HasSuffix(trimmed, "/") {
		p.dirOnly = true
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	explicitlyAnchored := strings.HasPrefix(trimmed, "/")
	if explicitlyAnchored {
		p.anchored = true
		trimmed = strings.TrimPrefix(trimmed, "/")
	}
	if trimmed == "" {
		return nil, fmt.Errorf("pattern %q selects nothing", raw)
	}

	// A pattern with no separator matches a basename at any depth, which is
	// what users rely on for "*.tmp" or "Thumbs.db". An explicit leading
	// slash overrides that: SPEC.md §6.5 says a leading "/" anchors to the
	// sync root, and it must still anchor once the slash has been stripped.
	if !explicitlyAnchored && !strings.Contains(trimmed, "/") {
		p.anchored = false
	}

	p.glob = trimmed
	if !caseSensitive {
		p.glob = strings.ToLower(p.glob)
	}

	if !doublestar.ValidatePattern(p.glob) {
		return nil, fmt.Errorf("pattern %q is not valid", raw)
	}
	return p, nil
}

// DirOnly reports whether this pattern only ever matches directories.
func (p *Pattern) DirOnly() bool { return p.dirOnly }

// String returns the pattern as written.
func (p *Pattern) String() string { return p.raw }

// Match reports whether relPath is selected by this pattern.
//
// relPath is slash-separated and relative to the sync root. isDir matters
// because a trailing-slash rule matches only directories.
func (p *Pattern) Match(relPath string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}

	candidate := relPath
	if !p.caseSensitive {
		candidate = strings.ToLower(candidate)
	}

	if p.anchored {
		ok, err := doublestar.Match(p.glob, candidate)
		return err == nil && ok
	}

	// Unanchored patterns match at any depth: against the whole path, and
	// against every suffix of it, so "cache/" matches "a/b/cache".
	if ok, err := doublestar.Match(p.glob, candidate); err == nil && ok {
		return true
	}
	for i := 0; i < len(candidate); i++ {
		if candidate[i] != '/' {
			continue
		}
		if ok, err := doublestar.Match(p.glob, candidate[i+1:]); err == nil && ok {
			return true
		}
	}
	return false
}

// MatchesAncestor reports whether any ancestor directory of relPath is
// matched by this pattern, which is how a directory rule prunes everything
// beneath it.
func (p *Pattern) MatchesAncestor(relPath string) bool {
	for i := 0; i < len(relPath); i++ {
		if relPath[i] != '/' {
			continue
		}
		if p.Match(relPath[:i], true) {
			return true
		}
	}
	return false
}

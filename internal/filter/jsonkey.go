package filter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// wildcardSegment fans a path out across every value of an object or every
// element of an array.
const wildcardSegment = "*"

// PatternsAtKey extracts a list of patterns from JSON at one or more dot-path
// keys (SPEC.md §6.5).
//
// The key field may hold several keys, one per line. Blank lines are ignored,
// the results are unioned in the order the keys are written, and duplicates
// are dropped — which is what lets one rule draw on several sections of the
// same catalogue without configuring the file twice.
//
// A "*" segment matches every value of an object or every element of an array,
// so "assets.*.name" collects the name of each asset in a catalogue keyed by
// something the user cannot predict, such as a hash.
//
// **Strictness differs between the two forms, deliberately.** A key with no
// "*" is an assertion that a path exists: a missing key, a non-numeric array
// index, or a scalar where a container was expected is an error, because the
// alternative is a typo that silently selects nothing. A key containing "*" is
// a query: children that lack the rest of the path are skipped, values that
// are not strings are skipped, and matching nothing at all is allowed — a
// catalogue whose sections vary, or whose section is empty, is a normal input
// rather than a broken one. A rule that ends up with no patterns is reported
// by the runner as a run event, so "nothing matched" is visible without being
// fatal.
func PatternsAtKey(document []byte, key string) ([]string, error) {
	var root any
	if err := json.Unmarshal(document, &root); err != nil {
		return nil, fmt.Errorf("the file is not valid JSON: %w", err)
	}

	keys := splitKeys(key)
	if len(keys) == 0 {
		return nil, fmt.Errorf("no key given: a jsonfile rule needs at least one dot-path key")
	}

	patterns := make([]string, 0, len(keys))
	seen := make(map[string]struct{})
	for _, k := range keys {
		found, err := patternsAtOneKey(root, k)
		if err != nil {
			return nil, err
		}
		for _, p := range found {
			if _, duplicate := seen[p]; duplicate {
				continue
			}
			seen[p] = struct{}{}
			patterns = append(patterns, p)
		}
	}
	return patterns, nil
}

// splitKeys separates a multi-line key field into individual dot-paths.
func splitKeys(key string) []string {
	var keys []string
	for _, line := range strings.Split(key, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			keys = append(keys, line)
		}
	}
	return keys
}

// patternsAtOneKey walks a single dot-path. It carries a *set* of nodes rather
// than one, because a "*" segment turns one node into many.
func patternsAtOneKey(root any, key string) ([]string, error) {
	segments := strings.Split(key, ".")

	// A wildcard anywhere in the path makes the whole path lenient. The two
	// readings cannot be mixed within one key: having "assets.*.name" error
	// because one asset lacks a name, while tolerating a missing "assets",
	// would be a rule nobody could hold in their head.
	lenient := false
	for _, segment := range segments {
		if segment == wildcardSegment {
			lenient = true
			break
		}
	}

	current := []any{root}
	walked := make([]string, 0, 4)

	for _, segment := range segments {
		if segment == "" {
			return nil, fmt.Errorf("key %q has an empty path segment", key)
		}
		walked = append(walked, segment)
		where := strings.Join(walked, ".")
		parent := strings.Join(walked[:len(walked)-1], ".")

		next := make([]any, 0, len(current))
		for _, node := range current {
			switch container := node.(type) {
			case map[string]any:
				if segment == wildcardSegment {
					// Sorted, because Go randomises map iteration and an
					// unstable pattern order would make the run log and the
					// rule's pattern count flap between identical runs.
					for _, name := range sortedKeys(container) {
						next = append(next, container[name])
					}
					continue
				}
				value, ok := container[segment]
				if !ok {
					if lenient {
						continue
					}
					return nil, fmt.Errorf("key %q does not exist in the file (no %q)", key, where)
				}
				next = append(next, value)

			case []any:
				if segment == wildcardSegment {
					next = append(next, container...)
					continue
				}
				index, err := strconv.Atoi(segment)
				if err != nil {
					if lenient {
						continue
					}
					return nil, fmt.Errorf("key %q: %q is an array, so %q must be a number", key, parent, segment)
				}
				if index < 0 || index >= len(container) {
					if lenient {
						continue
					}
					return nil, fmt.Errorf("key %q: index %d is out of range, the array has %d entries", key, index, len(container))
				}
				next = append(next, container[index])

			default:
				if lenient {
					continue
				}
				return nil, fmt.Errorf("key %q: %q is not an object or an array", key, parent)
			}
		}
		current = next
	}

	return collectPatterns(key, current, lenient)
}

// collectPatterns turns the nodes a key resolved to into pattern strings. A
// node may be a single string or an array of them; without a wildcard there is
// exactly one node, which is what keeps the non-wildcard errors unchanged.
func collectPatterns(key string, nodes []any, lenient bool) ([]string, error) {
	patterns := make([]string, 0, len(nodes))
	for _, node := range nodes {
		switch value := node.(type) {
		case string:
			patterns = append(patterns, value)

		case []any:
			for i, item := range value {
				text, ok := item.(string)
				if !ok {
					if lenient {
						continue
					}
					return nil, fmt.Errorf("key %q: entry %d is %s, but every entry must be a string",
						key, i, jsonTypeName(item))
				}
				patterns = append(patterns, text)
			}

		default:
			if lenient {
				continue
			}
			return nil, fmt.Errorf("key %q holds %s, but it must be a list of patterns or a single pattern",
				key, jsonTypeName(node))
		}
	}
	return patterns, nil
}

func sortedKeys(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ParseList reads a plain-text pattern file: one per line, "#" comments and
// blank lines allowed (SPEC.md §6.5).
func ParseList(document []byte) []string {
	var patterns []string
	for _, line := range strings.Split(string(document), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	default:
		return "an unexpected value"
	}
}

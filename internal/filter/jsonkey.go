package filter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PatternsAtKey extracts a list of patterns from JSON at a dot-path key
// (SPEC.md §6.5).
//
// The path traverses objects by name and arrays by index, so both
// "backup.exclude" and "0.skip" work. The value must be an array of strings,
// or a single string; anything else is a validation error, reported at save
// time and again at run time because the file may have changed.
func PatternsAtKey(document []byte, key string) ([]string, error) {
	var root any
	if err := json.Unmarshal(document, &root); err != nil {
		return nil, fmt.Errorf("the file is not valid JSON: %w", err)
	}

	current := root
	walked := make([]string, 0, 4)

	for _, segment := range strings.Split(key, ".") {
		if segment == "" {
			return nil, fmt.Errorf("key %q has an empty path segment", key)
		}
		walked = append(walked, segment)
		where := strings.Join(walked, ".")

		switch container := current.(type) {
		case map[string]any:
			value, ok := container[segment]
			if !ok {
				return nil, fmt.Errorf("key %q does not exist in the file (no %q)", key, where)
			}
			current = value

		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil {
				return nil, fmt.Errorf("key %q: %q is an array, so %q must be a number", key, strings.Join(walked[:len(walked)-1], "."), segment)
			}
			if index < 0 || index >= len(container) {
				return nil, fmt.Errorf("key %q: index %d is out of range, the array has %d entries", key, index, len(container))
			}
			current = container[index]

		default:
			return nil, fmt.Errorf("key %q: %q is not an object or an array", key, strings.Join(walked[:len(walked)-1], "."))
		}
	}

	switch value := current.(type) {
	case string:
		return []string{value}, nil

	case []any:
		patterns := make([]string, 0, len(value))
		for i, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("key %q: entry %d is %s, but every entry must be a string",
					key, i, jsonTypeName(item))
			}
			patterns = append(patterns, text)
		}
		return patterns, nil

	default:
		return nil, fmt.Errorf("key %q holds %s, but it must be a list of patterns or a single pattern",
			key, jsonTypeName(current))
	}
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

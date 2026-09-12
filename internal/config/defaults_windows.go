//go:build windows

package config

import (
	"os"
	"path/filepath"
)

// defaultDataDir is %ProgramData%\cn4m-cascade.
//
// ProgramData rather than the user profile because a native install is meant to
// run as a service under an account nobody logs into (SPEC.md §11, Phase 6b);
// a database under one operator's profile would vanish from the service's view
// the moment it ran as anyone else.
//
// The fallback is a relative path rather than a guessed absolute one: if
// ProgramData is somehow unset, failing next to the executable is easier to
// diagnose than silently writing to a directory the operator never chose.
func defaultDataDir() string {
	if dir := os.Getenv("ProgramData"); dir != "" {
		return filepath.Join(dir, "cn4m-cascade")
	}
	return "data"
}

// defaultMountRoot is empty on Windows, and that is not an oversight.
//
// Nothing is mounted: the connector authenticates a UNC path and hands that
// path straight to the engine (SPEC.md §3). There is no directory to create, no
// mountpoint to clean up at startup, and no stale mount to reap.
func defaultMountRoot() string { return "" }

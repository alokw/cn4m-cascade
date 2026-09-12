//go:build !windows

package engine

// lockedByAnotherProcess is always false off Windows.
//
// Linux and macOS replace a file another process has open without complaint:
// the old inode survives until the last handle closes, so a rename over it
// succeeds and nobody is disturbed. There is no error to recognise, and
// pretending otherwise would make the two platforms behave differently for no
// reason.
func lockedByAnotherProcess(error) bool { return false }

// renameBlockedByLock is always false off Windows, for the same reason.
func renameBlockedByLock(error, string) bool { return false }

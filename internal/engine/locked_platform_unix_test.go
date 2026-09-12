//go:build !windows

package engine

// Off Windows, no ordinary error is a lock: renames over open files succeed.
const wantLockedForPlatform = false

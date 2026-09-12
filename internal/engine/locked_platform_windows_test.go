//go:build windows

package engine

// On Windows a lock is a specific set of Win32 codes, so a generic error whose
// text merely mentions sharing is still not one of them.
const wantLockedForPlatform = false

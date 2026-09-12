//go:build windows

package mountmgr

import "github.com/alokw/cn4m-cascade/internal/store"

// mountpointFor is the UNC root itself, because Windows mounts nothing.
//
// This is the load-bearing difference between the two platforms. On Linux the
// engine is handed `/mnt/smb/<target-id>`, a local path that a CIFS mount has
// been laid over. On Windows it is handed `\\host\share` directly, and the OS
// resolves it through the network redirector on every call. Either way the
// engine receives an ordinary filesystem path and learns nothing about SMB,
// which is the boundary SPEC.md §4 exists to protect.
//
// mountRoot is ignored, and unused on Windows for exactly this reason.
//
// A malformed target cannot produce a traversal here: uncRoot keeps only the
// host and share segments and discards anything below them. If it cannot parse
// the target at all the raw path is returned, which then fails to open with a
// legible error rather than resolving somewhere unintended.
func mountpointFor(_ string, t *store.Target) string {
	root, err := uncRoot(t.UNCPath())
	if err != nil {
		return t.UNCPath()
	}
	return root
}

// prepareMountpoint does nothing: there is no directory to create.
//
// Creating one would be actively wrong — the path is a share on another
// machine, not a local directory this process owns.
func prepareMountpoint(_ string) error { return nil }

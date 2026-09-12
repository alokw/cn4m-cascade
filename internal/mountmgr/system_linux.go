//go:build linux

package mountmgr

// NewSystemMounter returns the Mounter for this platform.
//
// Linux mounts CIFS in the kernel with mount.cifs (SPEC.md §3), which is what
// the container deployment uses.
func NewSystemMounter() Mounter { return NewExecMounter() }

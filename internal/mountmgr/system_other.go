//go:build !linux && !windows

package mountmgr

// NewSystemMounter returns the Mounter for this platform.
//
// There is no CIFS support off Linux other than Windows, so this is the Linux
// mounter and it will fail at the first mount. It exists so the package builds
// on a developer's macOS machine, which is the same reason statfs_other.go
// does — every build and test there runs in the Linux dev container anyway.
func NewSystemMounter() Mounter { return NewExecMounter() }

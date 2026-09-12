//go:build windows

package mountmgr

// NewSystemMounter returns the Mounter for this platform.
//
// Windows has no mount.cifs and needs none: the OS opens UNC paths directly,
// so the connector authenticates the session and hands back the UNC root
// (SPEC.md §3).
func NewSystemMounter() Mounter { return NewWindowsConnector() }

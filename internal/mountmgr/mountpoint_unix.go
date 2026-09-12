//go:build !windows

package mountmgr

import (
	"os"
	"path/filepath"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// mountpointFor is the path a target is mounted at. Target IDs are hex, so
// this cannot escape the mount root.
func mountpointFor(mountRoot string, t *store.Target) string {
	return filepath.Join(mountRoot, t.ID)
}

// prepareMountpoint creates the empty directory mount.cifs will mount over.
func prepareMountpoint(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

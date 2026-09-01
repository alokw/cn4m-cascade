//go:build !linux

package mountmgr

import (
	"context"
	"errors"
)

// StatFS is unavailable off Linux. CIFS mounting is Linux-only; this stub
// exists so the package still builds on a developer's macOS machine.
func (m *ExecMounter) StatFS(_ context.Context, _ string) (FSStat, error) {
	return FSStat{}, errors.New("CIFS mounts are only supported on Linux")
}

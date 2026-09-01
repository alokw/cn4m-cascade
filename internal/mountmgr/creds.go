package mountmgr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// writeCredentialsFile writes a mount.cifs credentials file with 0600
// permissions and returns its path plus a cleanup function.
//
// SPEC.md §5 and CLAUDE.md both forbid credentials on the command line,
// where `ps` would expose them. The caller must always defer the returned
// cleanup, including on the error paths of the mount itself.
func writeCredentialsFile(dir, username, password, domain string) (path string, cleanup func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("preparing the credentials directory: %w", err)
	}

	f, err := os.CreateTemp(dir, "creds-*.tmp")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating the credentials file: %w", err)
	}
	name := f.Name()
	cleanup = func() {
		_ = os.Remove(name)
	}

	// CreateTemp already makes the file 0600, but be explicit: this is the
	// one file in the system that must never be group- or world-readable.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("securing the credentials file: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "username=%s\n", username)
	fmt.Fprintf(&b, "password=%s\n", password)
	if domain != "" {
		fmt.Fprintf(&b, "domain=%s\n", domain)
	}

	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("writing the credentials file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("closing the credentials file: %w", err)
	}
	return name, cleanup, nil
}

// mountpointFor is the path a target is mounted at. Target IDs are hex, so
// this cannot escape the mount root.
func mountpointFor(mountRoot string, t *store.Target) string {
	return filepath.Join(mountRoot, t.ID)
}

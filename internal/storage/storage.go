// Package storage is the protocol-agnostic view of a target (SPEC.md §4).
//
// The sync engine only ever sees a local root path; it never learns whether
// that path is a CIFS mount or a bind mount.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/mountmgr"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// Storage is SPEC.md §4's interface, verbatim.
type Storage interface {
	// Resolve returns a local filesystem root path for this target,
	// ensuring it is ready (mounted, reachable). May block briefly.
	Resolve(ctx context.Context) (rootPath string, err error)
	// Health checks reachability cheaply (e.g., statfs on mountpoint).
	Health(ctx context.Context) error
	// Release is called when no jobs reference the target (unmount).
	Release(ctx context.Context) error
}

// Provider builds a Storage for a target.
type Provider struct {
	mounts  *mountmgr.Manager
	healthc *health.Cache
}

// NewProvider wires the provider to the mount manager and health cache.
func NewProvider(mounts *mountmgr.Manager, healthc *health.Cache) *Provider {
	return &Provider{mounts: mounts, healthc: healthc}
}

// For returns the Storage implementation for a target.
func (p *Provider) For(t *store.Target) (Storage, error) {
	switch t.Type {
	case store.TargetSMB:
		return &SMBStorage{target: t, mounts: p.mounts, healthc: p.healthc}, nil
	case store.TargetLocal:
		return &LocalStorage{target: t, healthc: p.healthc}, nil
	default:
		return nil, fmt.Errorf("target %s has unsupported type %q", t.Describe(), t.Type)
	}
}

// SMBStorage resolves a target by mounting it through the mount manager.
type SMBStorage struct {
	target  *store.Target
	mounts  *mountmgr.Manager
	healthc *health.Cache
	root    string
	release func()
}

// Resolve mounts the share (or joins an existing mount) and returns the root
// path, including the target's subpath.
func (s *SMBStorage) Resolve(ctx context.Context) (string, error) {
	if s.release != nil {
		return s.root, nil
	}
	mountRoot, release, err := s.mounts.Acquire(ctx, s.target)
	if err != nil {
		return "", err
	}
	s.release = release
	s.root = withSubpath(mountRoot, s.target.Subpath)

	if s.target.Subpath != "" {
		// A stat on a CIFS path can block for as long as the kernel lets it,
		// so it gets the same treatment as every other SMB metadata call.
		if err := statBounded(ctx, s.root); err != nil {
			release()
			s.release, s.root = nil, ""
			return "", fmt.Errorf("subpath %q on %s: %w", s.target.Subpath, s.target.Describe(), err)
		}
	}
	return s.root, nil
}

// Health statfs's the mountpoint, bounded by the manager's timeout. An
// unresolved target is reported as unhealthy rather than being mounted:
// health checks must stay cheap (PROGRESS.md D-6).
func (s *SMBStorage) Health(ctx context.Context) error {
	if s.release == nil {
		return fmt.Errorf("%s is not mounted", s.target.Describe())
	}
	if _, err := s.mounts.Probe(ctx, s.root); err != nil {
		// SPEC.md §5: a share that stops responding marks the target
		// unhealthy. Without this the cached status behind GET /api/targets
		// would stay green until something tried to mount it again.
		err = fmt.Errorf("%s is not responding: %w", s.target.Describe(), err)
		s.healthc.SetUnhealthy(s.target.ID, err.Error())
		return err
	}
	s.healthc.SetHealthy(s.target.ID)
	return nil
}

// Release drops this reference. The mount itself lingers for the manager's
// idle grace period.
func (s *SMBStorage) Release(_ context.Context) error {
	if s.release != nil {
		s.release()
		s.release, s.root = nil, ""
	}
	return nil
}

// LocalStorage is a bind-mounted path inside the container.
type LocalStorage struct {
	target  *store.Target
	healthc *health.Cache
}

// Resolve verifies the path exists and is a directory.
func (l *LocalStorage) Resolve(ctx context.Context) (string, error) {
	root := withSubpath(l.target.LocalPath, l.target.Subpath)
	if err := l.check(root); err != nil {
		l.healthc.SetUnhealthy(l.target.ID, err.Error())
		return "", err
	}
	l.healthc.SetHealthy(l.target.ID)
	return root, nil
}

// Health re-checks the path.
func (l *LocalStorage) Health(_ context.Context) error {
	if err := l.check(withSubpath(l.target.LocalPath, l.target.Subpath)); err != nil {
		l.healthc.SetUnhealthy(l.target.ID, err.Error())
		return err
	}
	l.healthc.SetHealthy(l.target.ID)
	return nil
}

// Release is a no-op: nothing was mounted.
func (l *LocalStorage) Release(_ context.Context) error { return nil }

func (l *LocalStorage) check(root string) error {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s does not exist — check that the host directory is bind-mounted into the container", l.target.Describe())
	}
	if err != nil {
		return fmt.Errorf("%s is not readable: %w", l.target.Describe(), err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a file, not a directory", l.target.Describe())
	}
	return nil
}

func withSubpath(root, subpath string) string {
	if subpath == "" {
		return root
	}
	return filepath.Join(root, filepath.Clean("/"+subpath))
}

// DirEntry is one item in a target's root listing.
type DirEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// TestResult is what POST /api/targets/{id}/test returns.
type TestResult struct {
	OK             bool       `json:"ok"`
	Root           string     `json:"root"`
	NegotiatedVers string     `json:"negotiated_vers,omitempty"`
	CapacityBytes  uint64     `json:"capacity_bytes,omitempty"`
	FreeBytes      uint64     `json:"free_bytes,omitempty"`
	Entries        []DirEntry `json:"entries"`
	Truncated      bool       `json:"truncated"`
	ElapsedMS      int64      `json:"elapsed_ms"`
}

// listLimit caps the root listing so that testing a share with a million
// files in its root does not build a million-element response.
const listLimit = 200

// Test resolves a target, proves it responds, and lists its root
// (SPEC.md §8: "mount + statfs + list root"). It always releases the
// reference it took, so a failed test cannot leak a mount.
func (p *Provider) Test(ctx context.Context, t *store.Target) (*TestResult, error) {
	started := time.Now()

	st, err := p.For(t)
	if err != nil {
		return nil, err
	}

	root, err := st.Resolve(ctx)
	if err != nil {
		p.healthc.SetUnhealthy(t.ID, err.Error())
		return nil, err
	}
	// The reference is dropped either way; the mount stays up for the idle
	// grace period so a run started right after a test reuses it.
	defer func() { _ = st.Release(ctx) }()

	res := &TestResult{OK: true, Root: root, NegotiatedVers: t.NegotiatedVers}

	if smb, ok := st.(*SMBStorage); ok {
		stat, err := smb.mounts.Probe(ctx, root)
		if err != nil {
			p.healthc.SetUnhealthy(t.ID, err.Error())
			return nil, err
		}
		res.CapacityBytes = stat.Blocks * uint64(stat.BlockSize)
		res.FreeBytes = stat.BlocksAvail * uint64(stat.BlockSize)
	}

	entries, truncated, err := listDir(ctx, root, listLimit)
	if err != nil {
		p.healthc.SetUnhealthy(t.ID, err.Error())
		return nil, err
	}
	res.Entries, res.Truncated = entries, truncated
	res.ElapsedMS = time.Since(started).Milliseconds()

	p.healthc.SetHealthy(t.ID)
	return res, nil
}

// statBounded runs os.Stat in a goroutine and abandons it when ctx expires,
// so a dead SMB mount cannot block the caller indefinitely (SPEC.md §5).
func statBounded(ctx context.Context, path string) error {
	ch := make(chan error, 1)
	go func() {
		_, err := os.Stat(path)
		ch <- err
	}()

	select {
	case err := <-ch:
		if os.IsNotExist(err) {
			return errors.New("does not exist")
		}
		return err
	case <-ctx.Done():
		return fmt.Errorf("did not respond in time: %w", ctx.Err())
	}
}

// listDir reads at most limit entries. The read happens in a goroutine and
// the caller selects on ctx: readdir on a dead SMB mount can block for a
// long time, and nothing here is allowed to hang (SPEC.md §5).
func listDir(ctx context.Context, root string, limit int) ([]DirEntry, bool, error) {
	type result struct {
		entries   []DirEntry
		truncated bool
		err       error
	}
	ch := make(chan result, 1)

	go func() {
		f, err := os.Open(root)
		if err != nil {
			ch <- result{err: fmt.Errorf("listing %s: %w", root, err)}
			return
		}
		defer f.Close()

		// Read one extra to detect truncation.
		names, err := f.ReadDir(limit + 1)
		if err != nil && len(names) == 0 && !errors.Is(err, io.EOF) {
			ch <- result{err: fmt.Errorf("listing %s: %w", root, err)}
			return
		}

		truncated := len(names) > limit
		if truncated {
			names = names[:limit]
		}

		out := make([]DirEntry, 0, len(names))
		for _, n := range names {
			e := DirEntry{Name: n.Name(), IsDir: n.IsDir()}
			if info, err := n.Info(); err == nil {
				e.Size, e.ModTime = info.Size(), info.ModTime()
			}
			out = append(out, e)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		ch <- result{entries: out, truncated: truncated}
	}()

	select {
	case r := <-ch:
		return r.entries, r.truncated, r.err
	case <-ctx.Done():
		return nil, false, fmt.Errorf("listing %s did not respond in time: %w", root, ctx.Err())
	}
}

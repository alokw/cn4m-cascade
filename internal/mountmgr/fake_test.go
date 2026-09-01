package mountmgr

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// fakeMounter records what the manager asked the kernel to do, and lets a
// test script the outcome. It is the whole reason the manager's policy is
// testable without root, CIFS, or a Samba server.
type fakeMounter struct {
	mu sync.Mutex

	// mountFunc decides the outcome of each Mount call. nil means success.
	mountFunc func(spec MountSpec) error
	// statFunc decides the outcome of each StatFS call. nil means success.
	statFunc func(dir string) error
	// unmountFunc decides the outcome of each Unmount call. nil means success.
	unmountFunc func(ctx context.Context, dir string, lazy bool) error

	mounted      map[string]MountSpec
	extra        []MountInfo
	mountCalls   []MountSpec
	unmountCalls []unmountCall
	statCalls    int
	// credsSeen captures the contents of each credentials file at mount
	// time, so a test can prove the password never reached the arguments.
	credsSeen []string
}

type unmountCall struct {
	dir  string
	lazy bool
	// ctxExpired records whether the context was already done on arrival,
	// which is how the lazy-fallback regression test detects a caller
	// reusing a context it had already spent.
	ctxExpired bool
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{mounted: map[string]MountSpec{}}
}

func (f *fakeMounter) Mount(ctx context.Context, spec MountSpec) error {
	// The lock is not held across mountFunc: the real ExecMounter has no
	// global lock, so serialising a slow mount against Mounts() here would
	// invent contention that production does not have.
	f.mu.Lock()

	f.mountCalls = append(f.mountCalls, spec)
	if spec.CredsFile != "" {
		body, err := os.ReadFile(spec.CredsFile)
		if err != nil {
			f.mu.Unlock()
			return fmt.Errorf("test: reading the credentials file: %w", err)
		}
		f.credsSeen = append(f.credsSeen, string(body))

		info, err := os.Stat(spec.CredsFile)
		if err != nil {
			f.mu.Unlock()
			return fmt.Errorf("test: stat credentials file: %w", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			f.mu.Unlock()
			return fmt.Errorf("test: credentials file has mode %o, want 600", perm)
		}
	}

	mountFunc := f.mountFunc
	f.mu.Unlock()

	if mountFunc != nil {
		if err := mountFunc(spec); err != nil {
			return err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted[spec.Dir] = spec
	return nil
}

func (f *fakeMounter) Unmount(ctx context.Context, dir string, lazy bool) error {
	f.mu.Lock()
	f.unmountCalls = append(f.unmountCalls, unmountCall{dir: dir, lazy: lazy, ctxExpired: ctx.Err() != nil})
	unmountFunc := f.unmountFunc
	f.mu.Unlock()

	if unmountFunc != nil {
		if err := unmountFunc(ctx, dir, lazy); err != nil {
			return err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mounted, dir)
	return nil
}

func (f *fakeMounter) Mounts() ([]MountInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := append([]MountInfo{}, f.extra...)
	for dir, spec := range f.mounted {
		out = append(out, MountInfo{Dir: dir, FSType: "cifs", Source: spec.Source})
	}
	return out, nil
}

// StatFS deliberately ignores ctx. A Mounter is contractually required to
// honour it, and this fake breaking that contract is what proves the manager
// enforces its own deadline rather than trusting the layer below it.
func (f *fakeMounter) StatFS(_ context.Context, dir string) (FSStat, error) {
	f.mu.Lock()
	statFunc := f.statFunc
	f.statCalls++
	f.mu.Unlock()

	if statFunc != nil {
		if err := statFunc(dir); err != nil {
			return FSStat{}, err
		}
	}
	return FSStat{BlockSize: 4096, Blocks: 1000, BlocksFree: 500, BlocksAvail: 500}, nil
}

func (f *fakeMounter) snapshot() ([]MountSpec, []unmountCall, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MountSpec{}, f.mountCalls...),
		append([]unmountCall{}, f.unmountCalls...),
		append([]string{}, f.credsSeen...)
}

// newTestManager builds a Manager over a fake mounter and a real (temporary)
// database, since the manager persists the negotiated SMB version.
func newTestManager(t *testing.T, fake *fakeMounter, tune func(*Config)) (*Manager, *store.DB, *health.Cache) {
	t.Helper()

	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir+"/test.db")
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := Config{
		MountRoot:      dir + "/mnt",
		CredsDir:       dir + "/creds",
		MountTimeout:   2 * time.Second,
		StatFSTimeout:  time.Second,
		UnmountTimeout: time.Second,
		IdleGrace:      time.Hour, // reaping is driven explicitly in tests
		Params:         Params{UID: 1000, GID: 1000},
	}
	if tune != nil {
		tune(&cfg)
	}

	healthc := health.NewCache()
	creds := func(_ context.Context, tgt *store.Target) (string, error) {
		return "s3cret-for-" + tgt.Username, nil
	}
	return New(cfg, fake, db, creds, healthc, testLogger()), db, healthc
}

// testTarget inserts a target so it has a real ID and can be updated.
func testTarget(t *testing.T, db *store.DB, mutate func(*store.Target)) *store.Target {
	t.Helper()

	tgt := &store.Target{
		Name:     fmt.Sprintf("target-%d", time.Now().UnixNano()),
		Type:     store.TargetSMB,
		Host:     "192.168.1.50",
		Share:    "media",
		Username: "syncuser",
	}
	if mutate != nil {
		mutate(tgt)
	}
	if err := db.CreateTarget(context.Background(), tgt); err != nil {
		t.Fatalf("creating the test target: %v", err)
	}
	return tgt
}

// mountErrno builds the output mount.cifs produces for a given errno.
func mountErrnoOutput(errno int, text string) string {
	return fmt.Sprintf("mount error(%d): %s\nRefer to the mount.cifs(8) manual page", errno, text)
}

func dialectOf(spec MountSpec) string {
	for _, part := range strings.Split(spec.Options, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == "vers" {
			return v
		}
	}
	return ""
}

// hasOptionIn reports whether a resolved option string contains a bare flag.
func hasOptionIn(spec MountSpec, flag string) bool {
	for _, part := range strings.Split(spec.Options, ",") {
		if part == flag {
			return true
		}
	}
	return false
}

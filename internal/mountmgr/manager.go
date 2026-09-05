package mountmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// ErrInUse is returned when a target cannot be released because a job still
// holds a reference to it.
var ErrInUse = errors.New("target is in use")

// minReapInterval floors the idle-reaper tick. In production the interval is
// IdleGrace/4 (15s by default); the floor only matters for the very short
// grace periods used in tests.
const minReapInterval = 10 * time.Millisecond

// CredentialLookup resolves a target's plaintext password at mount time.
// Passing it as a function keeps decryption — and therefore the decryption
// key — out of this package entirely.
type CredentialLookup func(ctx context.Context, t *store.Target) (password string, err error)

// Config are the manager's tunables, sourced from process configuration.
type Config struct {
	MountRoot      string
	CredsDir       string
	MountTimeout   time.Duration
	StatFSTimeout  time.Duration
	UnmountTimeout time.Duration
	IdleGrace      time.Duration
	Params         Params
}

// Manager implements SPEC.md §5. It refcounts mounts, keeps them alive for a
// grace period after the last user lets go, notices mounts whose server has
// died, and translates failures into legible errors.
type Manager struct {
	cfg     Config
	mounter Mounter
	creds   CredentialLookup
	db      *store.DB
	healthc *health.Cache
	log     *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry

	reaperStop chan struct{}
	reaperDone chan struct{}
	started    atomic.Bool
	stopOnce   sync.Once
}

// entry is the per-target mount state. Its own mutex serialises mount and
// unmount for that target, so two goroutines racing to use the same share
// mount it exactly once while different targets proceed in parallel.
type entry struct {
	mu        sync.Mutex
	refs      int
	mounted   bool
	idleSince time.Time
}

// New constructs a Manager. The caller owns starting and stopping it.
func New(cfg Config, mounter Mounter, db *store.DB, creds CredentialLookup, healthc *health.Cache, log *slog.Logger) *Manager {
	if cfg.CredsDir == "" {
		cfg.CredsDir = filepath.Join(cfg.MountRoot, ".creds")
	}
	return &Manager{
		cfg:        cfg,
		mounter:    mounter,
		creds:      creds,
		db:         db,
		healthc:    healthc,
		log:        log,
		entries:    map[string]*entry{},
		reaperStop: make(chan struct{}),
		reaperDone: make(chan struct{}),
	}
}

// Start launches the idle reaper. Calling it more than once is a no-op.
func (m *Manager) Start() {
	if m.started.Swap(true) {
		return
	}
	go m.reap()
}

// Acquire mounts the target if it is not already mounted, takes a reference,
// and returns the local root path. The returned release function must be
// called exactly once; the mount is torn down IdleGrace after the last
// reference goes away.
func (m *Manager) Acquire(ctx context.Context, t *store.Target) (string, func(), error) {
	if t.Type != store.TargetSMB {
		return "", nil, fmt.Errorf("target %s is not an SMB target", t.Describe())
	}

	e := m.entryFor(t.ID)
	dir := mountpointFor(m.cfg.MountRoot, t)

	e.mu.Lock()
	defer e.mu.Unlock()

	if err := m.ensureMounted(ctx, t, e, dir); err != nil {
		m.healthc.SetUnhealthy(t.ID, err.Error())
		return "", nil, err
	}

	e.refs++
	m.healthc.SetHealthy(t.ID)

	var once sync.Once
	release := func() {
		once.Do(func() { m.release(t.ID) })
	}
	return dir, release, nil
}

// release drops one reference and starts the idle clock at zero.
func (m *Manager) release(id string) {
	e := m.entryFor(id)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.refs > 0 {
		e.refs--
	}
	if e.refs == 0 {
		e.idleSince = time.Now()
	}
}

// ensureMounted brings the mount up, or verifies that an existing one is
// still alive. Caller holds e.mu.
func (m *Manager) ensureMounted(ctx context.Context, t *store.Target, e *entry, dir string) error {
	existing, mounted, err := m.mountAt(dir)
	if err != nil {
		return err
	}

	if mounted {
		switch {
		case existing.Source != t.UNCPath():
			// The target was edited to point elsewhere while this mount was
			// alive. Handing it back would resolve the target to a different
			// server than the one it now names — in Phase 2 that means
			// copying files to the wrong NAS.
			m.log.Warn("mountpoint holds a different share than the target now names, remounting",
				"target", t.Name, "target_id", t.ID, "mountpoint", dir,
				"mounted_source", existing.Source, "target_source", t.UNCPath())
			m.forceUnmount(dir)
			e.mounted = false

		case m.probe(ctx, dir) == nil:
			// SPEC.md §5: before reporting a mount ready, prove it responds.
			e.mounted = true
			return nil

		default:
			// Stale: the server went away underneath us. Detach it lazily —
			// a normal umount would block on the dead server — and remount.
			m.log.Warn("stale mount detected, remounting",
				"target", t.Name, "target_id", t.ID, "mountpoint", dir)
			m.forceUnmount(dir)
			e.mounted = false
		}
	}

	if err := m.mount(ctx, t, dir); err != nil {
		return err
	}
	e.mounted = true
	return nil
}

// mount walks the SMB dialect ladder, stopping at the first success and
// recording which rung worked (SPEC.md §5).
func (m *Manager) mount(ctx context.Context, t *store.Target, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the mountpoint for %s: %w", t.Describe(), err)
	}

	password := ""
	if !t.IsGuest() {
		var err error
		if password, err = m.creds(ctx, t); err != nil {
			return fmt.Errorf("reading the credentials for %s: %w", t.Describe(), err)
		}
	}

	err := m.tryDialects(ctx, t, dir, password, t.Multichannel)

	// SPEC.md §5: "Try multichannel when the user enables it per-target;
	// fall back gracefully if the server rejects it." It cannot just be
	// another rung of the ladder: multichannel needs SMB 3.x, so leaving it
	// on also poisons the 2.1 rung. Falling back means dropping it and
	// walking the whole ladder again.
	if err != nil && t.Multichannel && dialectRetryable(err) {
		m.log.Warn("the server rejected multichannel, retrying without it",
			"target", t.Name, "target_id", t.ID)
		err = m.tryDialects(ctx, t, dir, password, false)
	}

	if err != nil {
		// MountError already names the share, so this only adds which target
		// it was — wrapping with Describe() would print the path twice.
		return fmt.Errorf("target %q: %w", t.Name, err)
	}
	return nil
}

// tryDialects walks the SMB version ladder once at a fixed multichannel
// setting, stopping at the first rung that mounts.
func (m *Manager) tryDialects(ctx context.Context, t *store.Target, dir, password string, multichannel bool) error {
	var lastErr error

	for _, dialect := range dialectsFor(t) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("mounting %s was cancelled: %w", t.Describe(), ctx.Err())
		default:
		}

		err := m.mountOnce(ctx, t, dir, dialect, password, multichannel)
		if err == nil {
			m.log.Info("mounted share",
				"target", t.Name, "target_id", t.ID, "source", t.UNCPath(),
				"mountpoint", dir, "vers", dialect, "multichannel", multichannel)
			m.recordDialect(ctx, t, dialect)
			return nil
		}

		lastErr = err
		if !dialectRetryable(err) {
			// Bad credentials or a missing share will fail identically at
			// every dialect; failing fast keeps the common typo legible.
			break
		}
		m.log.Debug("mount attempt failed, trying an older SMB version",
			"target_id", t.ID, "vers", dialect, "error", err)
	}

	return lastErr
}

// dialectRetryable reports whether an older dialect could plausibly help.
// Anything that is not a classified mount failure — one of our own
// credentials-file errors, say — is not worth retrying at all.
func dialectRetryable(err error) bool {
	var me *MountError
	if errors.As(err, &me) {
		return me.DialectRetryable()
	}
	return false
}

// recordDialect persists which rung of the ladder worked (SPEC.md §5).
func (m *Manager) recordDialect(ctx context.Context, t *store.Target, dialect string) {
	if t.NegotiatedVers == dialect {
		return
	}
	if err := m.db.SetNegotiatedVers(ctx, t.ID, dialect); err != nil {
		// Not fatal: the mount is up, we just failed to remember why.
		m.log.Warn("could not record the negotiated SMB version",
			"target_id", t.ID, "error", err)
	}
	t.NegotiatedVers = dialect
}

// mountOnce is a single mount.cifs invocation at one dialect.
func (m *Manager) mountOnce(ctx context.Context, t *store.Target, dir, dialect, password string, multichannel bool) error {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.MountTimeout)
	defer cancel()

	spec := MountSpec{
		Source:  t.UNCPath(),
		Dir:     dir,
		Options: buildOptions(t, m.cfg.Params, dialect, multichannel),
	}

	if !t.IsGuest() {
		credsFile, cleanup, err := writeCredentialsFile(m.cfg.CredsDir, t.Username, password, t.Domain)
		if err != nil {
			return err
		}
		// SPEC.md §5: the file is deleted immediately after the mount,
		// whether it succeeded or not.
		defer cleanup()
		spec.CredsFile = credsFile
	}

	return m.mounter.Mount(ctx, spec)
}

// probe is the stale-mount watchdog: a statfs that is not allowed to take
// longer than StatFSTimeout.
func (m *Manager) probe(ctx context.Context, dir string) error {
	_, err := m.Probe(ctx, dir)
	return err
}

// Probe reports whether a mounted path still responds, within the
// configured timeout.
func (m *Manager) Probe(ctx context.Context, dir string) (FSStat, error) {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StatFSTimeout)
	defer cancel()

	// The Mounter contract already requires StatFS to honour ctx, but the
	// deadline is enforced again here because this is the layer that must
	// never wedge: probe() runs while holding a target's lock, so a mounter
	// that ignored ctx would block every later use of that target forever,
	// not just this call. The abandoned goroutine writes to a buffered
	// channel and ends when the underlying call finally returns.
	type result struct {
		stat FSStat
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		stat, err := m.mounter.StatFS(ctx, dir)
		ch <- result{stat: stat, err: err}
	}()

	select {
	case r := <-ch:
		return r.stat, r.err
	case <-ctx.Done():
		return FSStat{}, fmt.Errorf("checking %s did not respond in time: %w", dir, ctx.Err())
	}
}

// forceUnmount detaches a mountpoint lazily. Used when the server is gone
// and a normal unmount would block; SPEC.md §13 notes these can linger, so
// each one is logged.
func (m *Manager) forceUnmount(dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.UnmountTimeout)
	defer cancel()
	if err := m.mounter.Unmount(ctx, dir, true); err != nil {
		m.log.Error("lazy unmount failed", "mountpoint", dir, "error", err)
		return
	}
	m.log.Warn("lazily unmounted a stale mount; it may linger until its last reference is dropped",
		"mountpoint", dir)
}

// mountAt consults the kernel mount table rather than trusting our own
// bookkeeping, which can be wrong after an unclean restart. It returns what
// is mounted at dir so callers can check it is still the share they expect.
func (m *Manager) mountAt(dir string) (MountInfo, bool, error) {
	mounts, err := m.mounter.Mounts()
	if err != nil {
		return MountInfo{}, false, err
	}
	for _, mi := range mounts {
		if mi.Dir == dir {
			return mi, true, nil
		}
	}
	return MountInfo{}, false, nil
}

// isMounted reports presence only, for callers tearing a mountpoint down.
func (m *Manager) isMounted(dir string) (bool, error) {
	_, mounted, err := m.mountAt(dir)
	return mounted, err
}

// Unmount tears down a target's mount now. It refuses while references are
// outstanding, which is what makes DELETE /api/targets/{id} safe.
func (m *Manager) Unmount(ctx context.Context, id string) error {
	e := m.entryFor(id)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.refs > 0 {
		return fmt.Errorf("%w: %d job(s) still using it", ErrInUse, e.refs)
	}
	return m.unmountLocked(ctx, id, e)
}

// unmountLocked unmounts if mounted. Caller holds e.mu.
func (m *Manager) unmountLocked(ctx context.Context, id string, e *entry) error {
	dir := filepath.Join(m.cfg.MountRoot, id)

	mounted, err := m.isMounted(dir)
	if err != nil {
		return err
	}
	if !mounted {
		e.mounted = false
		return nil
	}

	normalCtx, cancel := context.WithTimeout(ctx, m.cfg.UnmountTimeout)
	defer cancel()

	if err := m.mounter.Unmount(normalCtx, dir, false); err != nil {
		// A normal unmount fails when the server is unreachable, and that
		// failure is usually the deadline above expiring. The lazy retry
		// therefore needs a budget of its own: reusing the spent context
		// would kill it instantly and leave the mount wedged forever, which
		// is the one case the lazy detach exists for.
		m.log.Warn("unmount failed, detaching lazily", "mountpoint", dir, "error", err)

		lazyCtx, lazyCancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.UnmountTimeout)
		defer lazyCancel()

		if lerr := m.mounter.Unmount(lazyCtx, dir, true); lerr != nil {
			return fmt.Errorf("unmounting %s: %w", dir, lerr)
		}
	}

	e.mounted = false
	m.log.Info("unmounted share", "target_id", id, "mountpoint", dir)
	return nil
}

// reap unmounts targets that have been unreferenced for longer than the idle
// grace period, so back-to-back jobs do not churn mounts (SPEC.md §5).
func (m *Manager) reap() {
	defer close(m.reaperDone)

	interval := m.cfg.IdleGrace / 4
	if interval < minReapInterval {
		interval = minReapInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.reaperStop:
			return
		case <-ticker.C:
			m.reapOnce()
		}
	}
}

func (m *Manager) reapOnce() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		e := m.entryFor(id)

		// Never wait on the entry lock. A target being mounted right now
		// holds it for as long as its own timeout, and blocking here would
		// stall reaping for every other target — and, because Shutdown
		// waits for this loop to finish, would push SIGTERM handling past
		// its deadline too. A busy target is not idle by definition, so
		// skipping it costs nothing but one tick.
		if !e.mu.TryLock() {
			continue
		}

		idle := e.refs == 0 && e.mounted && !e.idleSince.IsZero() &&
			time.Since(e.idleSince) >= m.cfg.IdleGrace
		if idle {
			ctx, cancel := context.WithTimeout(context.Background(), m.cfg.UnmountTimeout)
			if err := m.unmountLocked(ctx, id, e); err != nil {
				m.log.Error("idle unmount failed", "target_id", id, "error", err)
			}
			cancel()
		}
		e.mu.Unlock()
	}
}

// ReconcileStale lazily unmounts anything left under the mount root by an
// unclean shutdown (SPEC.md §5, "startup hygiene"). It runs before the API
// starts serving.
func (m *Manager) ReconcileStale(ctx context.Context) error {
	mounts, err := m.mounter.Mounts()
	if err != nil {
		return err
	}

	root := strings.TrimSuffix(m.cfg.MountRoot, "/") + "/"
	found := 0
	for _, mi := range mounts {
		if !strings.HasPrefix(mi.Dir, root) || mi.FSType != "cifs" {
			continue
		}
		found++
		m.log.Warn("found a leftover mount from a previous run, detaching",
			"mountpoint", mi.Dir, "source", mi.Source)

		uctx, cancel := context.WithTimeout(ctx, m.cfg.UnmountTimeout)
		err := m.mounter.Unmount(uctx, mi.Dir, true)
		cancel()
		if err != nil {
			m.log.Error("could not detach leftover mount", "mountpoint", mi.Dir, "error", err)
		}
	}

	// Credentials files should never outlive a mount, but a hard kill
	// between writing one and deleting it would leave one behind.
	m.removeStaleCredentials()

	if found > 0 {
		m.log.Info("startup mount cleanup complete", "detached", found)
	}
	return nil
}

func (m *Manager) removeStaleCredentials() {
	entries, err := os.ReadDir(m.cfg.CredsDir)
	if err != nil {
		return // nothing to clean
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "creds-") {
			path := filepath.Join(m.cfg.CredsDir, e.Name())
			if err := os.Remove(path); err != nil {
				m.log.Error("could not remove a leftover credentials file", "path", path, "error", err)
			} else {
				m.log.Warn("removed a leftover credentials file", "path", path)
			}
		}
	}
}

// Shutdown stops the reaper and detaches every mount we own. Called on
// SIGTERM (SPEC.md §10).
func (m *Manager) Shutdown(ctx context.Context) error {
	// Idempotent: closing a closed channel panics, and library code must not
	// panic (CLAUDE.md). Only signal the reaper if it was ever started.
	m.stopOnce.Do(func() {
		close(m.reaperStop)
		if m.started.Load() {
			select {
			case <-m.reaperDone:
			case <-ctx.Done():
			}
		}
	})

	m.mu.Lock()
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for i, id := range ids {
		// Per-command bounds are not enough on their own. Each unmount is
		// capped at UnmountTimeout, but N targets in series can still exceed
		// the whole shutdown budget and get the process SIGKILLed mid-cleanup
		// — the same SPEC.md §10 promise the per-command bound exists to keep,
		// just one level up. Whatever is left is the kernel's problem, which
		// is what a lazy detach hands it anyway.
		if ctx.Err() != nil {
			m.log.Warn("shutdown deadline reached; leaving the rest to the kernel",
				"detached", i, "remaining", len(ids)-i)
			break
		}

		dir := filepath.Join(m.cfg.MountRoot, id)

		// Take the entry lock if it is free, but never wait on it: a mount
		// in progress holds it for as long as its own timeout, which would
		// blow straight through the shutdown deadline. A lazy detach is
		// safe to issue either way — it is just umount -l — so an unlocked
		// teardown is correct, only unrecorded.
		e := m.entryFor(id)
		locked := e.mu.TryLock()

		if mounted, err := m.isMounted(dir); err == nil && mounted {
			// Shutdown never blocks on a dead server: detach lazily.
			m.forceUnmount(dir)
			if locked {
				e.mounted = false
			}
		}
		if locked {
			e.mu.Unlock()
		}
	}
	m.removeStaleCredentials()
	return nil
}

// Forget drops all state for a target after it has been deleted.
//
// It refuses to drop an entry that still has references. Dropping one would
// orphan the mount: the holder's release() would look the entry up again,
// find nothing, and create a fresh one with mounted=false — which the reaper
// skips, so nothing would ever unmount the share again.
func (m *Manager) Forget(id string) {
	m.mu.Lock()
	e, ok := m.entries[id]
	m.mu.Unlock()

	if ok {
		e.mu.Lock()
		refs := e.refs
		e.mu.Unlock()

		if refs > 0 {
			m.log.Warn("not forgetting a target that is still in use; its mount will be reaped when released",
				"target_id", id, "refs", refs)
			return
		}
	}

	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
	m.healthc.Delete(id)
}

// Refs reports the current reference count, for tests and for the delete
// path's in-use check.
func (m *Manager) Refs(id string) int {
	e := m.entryFor(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refs
}

func (m *Manager) entryFor(id string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[id]; ok {
		return e
	}
	e := &entry{}
	m.entries[id] = e
	return e
}

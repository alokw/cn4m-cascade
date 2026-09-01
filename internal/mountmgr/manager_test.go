package mountmgr

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/store"
)

func TestAcquireMountsOnceAndRefcounts(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)
	ctx := context.Background()

	rootA, releaseA, err := mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	rootB, releaseB, err := mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}

	if rootA != rootB {
		t.Fatalf("two acquisitions returned different roots: %q and %q", rootA, rootB)
	}
	if !strings.HasSuffix(rootA, tgt.ID) {
		t.Errorf("root %q is not the target's mountpoint", rootA)
	}
	if mounts, _, _ := fake.snapshot(); len(mounts) != 1 {
		t.Fatalf("mounted %d times, want 1: concurrent users must share one mount", len(mounts))
	}
	if got := mgr.Refs(tgt.ID); got != 2 {
		t.Fatalf("refs = %d, want 2", got)
	}

	releaseA()
	if got := mgr.Refs(tgt.ID); got != 1 {
		t.Fatalf("refs after one release = %d, want 1", got)
	}

	// A still-referenced target is never unmounted.
	mgr.reapOnce()
	if _, unmounts, _ := fake.snapshot(); len(unmounts) != 0 {
		t.Fatalf("unmounted %d times while still referenced", len(unmounts))
	}

	releaseB()
	if got := mgr.Refs(tgt.ID); got != 0 {
		t.Fatalf("refs after both releases = %d, want 0", got)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)

	_, release, err := mgr.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	release()
	release()

	if got := mgr.Refs(tgt.ID); got != 0 {
		t.Fatalf("refs = %d, want 0: a double release must not underflow into another job's reference", got)
	}
}

func TestIdleGraceDelaysUnmount(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, func(c *Config) { c.IdleGrace = 50 * time.Millisecond })
	tgt := testTarget(t, db, nil)

	_, release, err := mgr.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	// Inside the grace period the mount stays, so back-to-back jobs reuse it.
	mgr.reapOnce()
	if _, unmounts, _ := fake.snapshot(); len(unmounts) != 0 {
		t.Fatalf("unmounted during the idle grace period")
	}

	time.Sleep(60 * time.Millisecond)
	mgr.reapOnce()

	_, unmounts, _ := fake.snapshot()
	if len(unmounts) != 1 {
		t.Fatalf("unmount calls = %d, want 1 after the grace period", len(unmounts))
	}
	if unmounts[0].lazy {
		t.Errorf("idle unmount was lazy; a healthy mount should unmount normally")
	}
}

func TestReaperGoroutineUnmountsIdleTargets(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, func(c *Config) { c.IdleGrace = 30 * time.Millisecond })
	tgt := testTarget(t, db, nil)

	mgr.Start()
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })

	_, release, err := mgr.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, unmounts, _ := fake.snapshot(); len(unmounts) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the reaper never unmounted an idle target")
}

func TestStaleMountIsDetachedAndRemounted(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, healthc := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)
	ctx := context.Background()

	_, release, err := mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	release()

	// The server dies: the mount is still in the table, but statfs hangs.
	// SPEC.md §5 requires a lazy detach and a remount, not a hang.
	var statfsFailures int
	fake.mu.Lock()
	fake.statFunc = func(string) error {
		statfsFailures++
		if statfsFailures == 1 {
			return errors.New("host is down")
		}
		return nil
	}
	fake.mu.Unlock()

	if _, release, err = mgr.Acquire(ctx, tgt); err != nil {
		t.Fatalf("Acquire after the server died: %v", err)
	}
	release()

	mounts, unmounts, _ := fake.snapshot()
	if len(mounts) != 2 {
		t.Fatalf("mount calls = %d, want 2 (the stale mount must be replaced)", len(mounts))
	}
	if len(unmounts) != 1 || !unmounts[0].lazy {
		t.Fatalf("unmount calls = %+v, want exactly one lazy detach", unmounts)
	}
	if got := healthc.Get(tgt.ID); got.State != health.StateHealthy {
		t.Errorf("health = %q, want healthy after a successful remount", got.State)
	}
}

func TestStatFSTimeoutDoesNotHang(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, func(c *Config) { c.StatFSTimeout = 50 * time.Millisecond })
	tgt := testTarget(t, db, nil)
	ctx := context.Background()

	_, release, err := mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	// A statfs that never returns: the manager must give up on its own.
	fake.mu.Lock()
	fake.statFunc = func(string) error {
		<-time.After(10 * time.Second)
		return nil
	}
	fake.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Probe(ctx, "/mnt/smb/"+tgt.ID)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Probe succeeded despite a statfs that never returns")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Probe hung: nothing may block forever on a dead share (SPEC.md §5)")
	}
}

func TestMountWalksTheDialectLadder(t *testing.T) {
	fake := newFakeMounter()
	fake.mountFunc = func(spec MountSpec) error {
		if d := dialectOf(spec); d == "3.1.1" || d == "3.0" {
			return classifyMountFailure(spec.Source, 32, mountErrnoOutput(95, "Operation not supported"), nil)
		}
		return nil
	}

	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)

	if _, release, err := mgr.Acquire(context.Background(), tgt); err != nil {
		t.Fatalf("Acquire: %v", err)
	} else {
		release()
	}

	mounts, _, _ := fake.snapshot()
	if len(mounts) != 3 {
		t.Fatalf("mount attempts = %d, want 3 (3.1.1 → 3.0 → 2.1)", len(mounts))
	}
	for i, want := range []string{"3.1.1", "3.0", "2.1"} {
		if got := dialectOf(mounts[i]); got != want {
			t.Errorf("attempt %d used vers=%s, want %s", i+1, got, want)
		}
	}

	// SPEC.md §5: record which version worked.
	reloaded, err := db.GetTarget(context.Background(), tgt.ID)
	if err != nil {
		t.Fatalf("reloading the target: %v", err)
	}
	if reloaded.NegotiatedVers != "2.1" {
		t.Errorf("negotiated_vers = %q, want %q", reloaded.NegotiatedVers, "2.1")
	}
}

func TestBadCredentialsFailFastWithoutWalkingTheLadder(t *testing.T) {
	fake := newFakeMounter()
	fake.mountFunc = func(spec MountSpec) error {
		return classifyMountFailure(spec.Source, 32, mountErrnoOutput(13, "Permission denied"), nil)
	}

	mgr, db, healthc := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)

	_, _, err := mgr.Acquire(context.Background(), tgt)
	if err == nil {
		t.Fatal("Acquire succeeded with bad credentials")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("error %q is not the legible authentication message", err)
	}

	// Retrying older dialects cannot fix a password, and would make the
	// commonest mistake three times slower to report.
	if mounts, _, _ := fake.snapshot(); len(mounts) != 1 {
		t.Fatalf("mount attempts = %d, want 1: an auth failure must not walk the ladder", len(mounts))
	}
	if got := healthc.Get(tgt.ID); got.State != health.StateUnhealthy {
		t.Errorf("health = %q, want unhealthy", got.State)
	}
}

func TestCredentialsGoToA0600FileAndAreDeleted(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, func(tg *store.Target) { tg.Username = "syncuser"; tg.Domain = "WORKGROUP" })

	_, release, err := mgr.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	mounts, _, creds := fake.snapshot()
	if len(creds) != 1 {
		t.Fatalf("credentials files written = %d, want 1", len(creds))
	}
	for _, want := range []string{"username=syncuser", "password=s3cret-for-syncuser", "domain=WORKGROUP"} {
		if !strings.Contains(creds[0], want) {
			t.Errorf("credentials file %q missing %q", creds[0], want)
		}
	}

	// CLAUDE.md: never put credentials on a command line.
	if strings.Contains(mounts[0].Options, "s3cret") {
		t.Fatalf("the password leaked into the mount options: %q", mounts[0].Options)
	}

	// SPEC.md §5: deleted immediately after the mount.
	if _, err := os.Stat(mounts[0].CredsFile); !os.IsNotExist(err) {
		t.Fatalf("credentials file %s still exists after the mount", mounts[0].CredsFile)
	}
}

func TestGuestTargetWritesNoCredentialsFile(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, func(tg *store.Target) { tg.Username = "" })

	_, release, err := mgr.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	mounts, _, creds := fake.snapshot()
	if len(creds) != 0 {
		t.Fatalf("wrote a credentials file for a guest target")
	}
	if mounts[0].CredsFile != "" {
		t.Errorf("guest mount referenced a credentials file: %q", mounts[0].CredsFile)
	}
	if !hasOption(mounts[0].Options, "guest") {
		t.Errorf("guest mount options %q lack the guest flag", mounts[0].Options)
	}
}

func TestUnmountRefusedWhileInUse(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)
	ctx := context.Background()

	_, release, err := mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	err = mgr.Unmount(ctx, tgt.ID)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("Unmount while referenced = %v, want ErrInUse", err)
	}

	release()
	if err := mgr.Unmount(ctx, tgt.ID); err != nil {
		t.Fatalf("Unmount after release: %v", err)
	}
	if _, unmounts, _ := fake.snapshot(); len(unmounts) != 1 {
		t.Fatalf("unmount calls = %d, want 1", len(unmounts))
	}
}

func TestReconcileStaleDetachesLeftoverMounts(t *testing.T) {
	fake := newFakeMounter()
	mgr, _, _ := newTestManager(t, fake, func(c *Config) { c.MountRoot = "/mnt/smb" })

	fake.extra = []MountInfo{
		{Dir: "/mnt/smb/abc123", FSType: "cifs", Source: "//10.0.0.1/media"},
		{Dir: "/mnt/smb/def456", FSType: "cifs", Source: "//10.0.0.2/backup"},
		{Dir: "/mnt/local/stuff", FSType: "ext4", Source: "/dev/sda1"}, // not ours
		{Dir: "/", FSType: "overlay", Source: "overlay"},               // not ours
	}

	if err := mgr.ReconcileStale(context.Background()); err != nil {
		t.Fatalf("ReconcileStale: %v", err)
	}

	_, unmounts, _ := fake.snapshot()
	if len(unmounts) != 2 {
		t.Fatalf("detached %d mounts, want 2 (only the CIFS mounts under the mount root)", len(unmounts))
	}
	for _, u := range unmounts {
		if !u.lazy {
			t.Errorf("%s was detached non-lazily; a leftover mount's server may be gone", u.dir)
		}
		if !strings.HasPrefix(u.dir, "/mnt/smb/") {
			t.Errorf("detached %s, which is not under the mount root", u.dir)
		}
	}
}

func TestReconcileStaleRemovesLeftoverCredentialsFiles(t *testing.T) {
	fake := newFakeMounter()
	dir := t.TempDir()
	mgr, _, _ := newTestManager(t, fake, func(c *Config) { c.CredsDir = dir })

	leftover := dir + "/creds-orphan.tmp"
	if err := os.WriteFile(leftover, []byte("username=x\npassword=y\n"), 0o600); err != nil {
		t.Fatalf("seeding a leftover credentials file: %v", err)
	}

	if err := mgr.ReconcileStale(context.Background()); err != nil {
		t.Fatalf("ReconcileStale: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("a leftover credentials file survived startup cleanup")
	}
}

func TestAcquireRejectsLocalTargets(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)

	tgt := &store.Target{Name: "local-one", Type: store.TargetLocal, LocalPath: "/mnt/local/stuff"}
	if err := db.CreateTarget(context.Background(), tgt); err != nil {
		t.Fatalf("creating the local target: %v", err)
	}

	if _, _, err := mgr.Acquire(context.Background(), tgt); err == nil {
		t.Fatal("the mount manager accepted a local target")
	}
}

func TestShutdownDetachesEverything(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	first := testTarget(t, db, nil)
	second := testTarget(t, db, nil)

	mgr.Start()
	for _, tgt := range []*store.Target{first, second} {
		if _, release, err := mgr.Acquire(context.Background(), tgt); err != nil {
			t.Fatalf("Acquire: %v", err)
		} else {
			release()
		}
	}

	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	_, unmounts, _ := fake.snapshot()
	if len(unmounts) != 2 {
		t.Fatalf("detached %d mounts on shutdown, want 2", len(unmounts))
	}
	for _, u := range unmounts {
		if !u.lazy {
			t.Errorf("%s was not detached lazily; shutdown must never block on a dead server", u.dir)
		}
	}
}

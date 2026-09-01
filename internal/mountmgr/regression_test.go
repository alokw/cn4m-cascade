package mountmgr

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// When the server is gone, the ordinary unmount fails by burning its whole
// deadline. The lazy fallback must still get a usable context — reusing the
// spent one would kill it instantly and leave the mount wedged forever.
func TestLazyUnmountFallbackGetsAFreshDeadline(t *testing.T) {
	fake := newFakeMounter()
	fake.unmountFunc = func(ctx context.Context, _ string, lazy bool) error {
		if !lazy {
			// A normal umount against a dead server: blocks until the
			// caller's deadline expires.
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}

	mgr, db, _ := newTestManager(t, fake, func(c *Config) { c.UnmountTimeout = 50 * time.Millisecond })
	tgt := testTarget(t, db, nil)
	ctx := context.Background()

	_, release, err := mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	if err := mgr.Unmount(ctx, tgt.ID); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	_, unmounts, _ := fake.snapshot()
	if len(unmounts) != 2 {
		t.Fatalf("unmount calls = %+v, want a normal attempt followed by a lazy one", unmounts)
	}
	if !unmounts[1].lazy {
		t.Fatalf("the second attempt was not lazy: %+v", unmounts)
	}
	if unmounts[1].ctxExpired {
		t.Fatal("the lazy fallback was handed an already-expired context, so it would be killed before it could run")
	}
}

// A mountpoint holding a different share than the target now names must
// never be handed back: the target was edited underneath a live mount.
func TestMountpointHoldingADifferentShareIsReplaced(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)
	ctx := context.Background()

	_, release, err := mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	// The user repoints the target at another server.
	tgt.Host = "192.168.1.99"
	if err := db.UpdateTarget(ctx, tgt); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}

	_, release, err = mgr.Acquire(ctx, tgt)
	if err != nil {
		t.Fatalf("Acquire after repointing: %v", err)
	}
	release()

	mounts, unmounts, _ := fake.snapshot()
	if len(mounts) != 2 {
		t.Fatalf("mount calls = %d, want 2: the old share must not be reused", len(mounts))
	}
	if mounts[1].Source != "//192.168.1.99/media" {
		t.Errorf("remounted %q, want the target's new share", mounts[1].Source)
	}
	if len(unmounts) != 1 || !unmounts[0].lazy {
		t.Errorf("unmount calls = %+v, want one lazy detach of the old share", unmounts)
	}
}

// SPEC.md §5: "Try multichannel when the user enables it per-target; fall
// back gracefully if the server rejects it."
func TestMultichannelFallsBackWhenTheServerRejectsIt(t *testing.T) {
	fake := newFakeMounter()
	fake.mountFunc = func(spec MountSpec) error {
		if hasOptionIn(spec, "multichannel") {
			return classifyMountFailure(spec.Source, 32, mountErrnoOutput(95, "Operation not supported"), nil)
		}
		return nil
	}

	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, func(tg *store.Target) { tg.Multichannel = true })

	_, release, err := mgr.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	mounts, _, _ := fake.snapshot()
	if len(mounts) < 2 {
		t.Fatalf("mount attempts = %d, want the multichannel ladder followed by a retry without it", len(mounts))
	}
	if !hasOptionIn(mounts[0], "multichannel") {
		t.Error("the first attempt did not try multichannel, though the target enables it")
	}

	final := mounts[len(mounts)-1]
	if hasOptionIn(final, "multichannel") {
		t.Error("the successful mount still carried multichannel")
	}
	// multichannel needs SMB 3.x, so the retry has to restart the ladder at
	// the top rather than continue down it.
	if got := dialectOf(final); got != "3.1.1" {
		t.Errorf("the fallback mounted at vers=%s, want the ladder restarted at 3.1.1", got)
	}
}

// Concurrent users of one target must share a single mount and leave the
// refcount at zero.
func TestConcurrentAcquireAndRelease(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, nil)
	tgt := testTarget(t, db, nil)

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := mgr.Acquire(context.Background(), tgt)
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Acquire: %v", err)
	}
	if mounts, _, _ := fake.snapshot(); len(mounts) != 1 {
		t.Errorf("mount calls = %d, want 1: concurrent users must share one mount", len(mounts))
	}
	if got := mgr.Refs(tgt.ID); got != 0 {
		t.Errorf("refs = %d, want 0 after every holder released", got)
	}
}

// Forgetting a target that is still referenced would orphan its mount: the
// holder's release would resurrect a fresh entry with mounted=false, which
// the reaper skips, so nothing would ever unmount the share.
func TestForgetRefusesWhileReferenced(t *testing.T) {
	fake := newFakeMounter()
	mgr, db, _ := newTestManager(t, fake, func(c *Config) { c.IdleGrace = 20 * time.Millisecond })
	tgt := testTarget(t, db, nil)

	_, release, err := mgr.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	mgr.Forget(tgt.ID)

	if got := mgr.Refs(tgt.ID); got != 1 {
		t.Fatalf("refs = %d, want 1: Forget dropped an entry that was still in use", got)
	}

	release()
	time.Sleep(30 * time.Millisecond)
	mgr.reapOnce()

	if _, unmounts, _ := fake.snapshot(); len(unmounts) != 1 {
		t.Fatalf("unmount calls = %d, want 1: the mount was orphaned", len(unmounts))
	}
}

// CLAUDE.md: no panics in library code.
func TestShutdownIsIdempotent(t *testing.T) {
	fake := newFakeMounter()
	mgr, _, _ := newTestManager(t, fake, nil)
	mgr.Start()

	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// Shutdown must not hang waiting for a reaper that was never started.
func TestShutdownWithoutStart(t *testing.T) {
	fake := newFakeMounter()
	mgr, _, _ := newTestManager(t, fake, nil)

	done := make(chan error, 1)
	go func() { done <- mgr.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown blocked waiting for a reaper that was never started")
	}
}

// Shutdown must not wait on a target whose mount is still in progress: a
// mutex is not cancellable, so waiting would blow the shutdown deadline.
func TestShutdownDoesNotWaitForAnInFlightMount(t *testing.T) {
	fake := newFakeMounter()
	releaseMount := make(chan struct{})
	fake.mountFunc = func(MountSpec) error {
		<-releaseMount
		return nil
	}

	mgr, db, _ := newTestManager(t, fake, func(c *Config) { c.MountTimeout = 10 * time.Second })
	tgt := testTarget(t, db, nil)
	mgr.Start()

	mounting := make(chan struct{})
	go func() {
		close(mounting)
		if _, release, err := mgr.Acquire(context.Background(), tgt); err == nil {
			release()
		}
	}()
	<-mounting
	time.Sleep(50 * time.Millisecond) // let Acquire take the entry lock

	done := make(chan error, 1)
	go func() { done <- mgr.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown blocked on an in-flight mount; SIGTERM would exceed its deadline")
	}
	close(releaseMount)
}

// Advanced mount options must not be able to defeat the guarantees the rest
// of the system depends on.
func TestForbiddenMountOptionsAreRejected(t *testing.T) {
	tests := []struct {
		name     string
		override string
		wantErr  string
	}{
		{"password on the command line", "password=hunter2", "not allowed"},
		{"short password alias", "cache=none,pass=hunter2", "not allowed"},
		{"redirected credentials file", "credentials=/tmp/evil", "not allowed"},
		{"hard defeats the never-hang rule", "hard", "hang indefinitely"},
		{"sharesock reopens credential sharing", "sharesock", "credentials"},
		{"case insensitive", "PASSWORD=hunter2", "not allowed"},
		{"harmless options still pass", "cache=none,noserverino,rsize=1048576", ""},
		{"nosharesock is not sharesock", "nosharesock", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tgt := store.Target{
				Name: "x", Type: store.TargetSMB, Host: "10.0.0.1", Share: "media",
				MountOptsOverride: tt.override,
			}
			err := tgt.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() accepted %q", tt.override)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// The ladder must not be walked for failures a different dialect cannot fix.
func TestDialectRetryableIgnoresNonMountErrors(t *testing.T) {
	if dialectRetryable(errors.New("could not write the credentials file")) {
		t.Error("an internal error was treated as worth retrying at another dialect")
	}
	if !dialectRetryable(&MountError{Kind: KindDialect}) {
		t.Error("a dialect rejection should be retryable")
	}
	if dialectRetryable(&MountError{Kind: KindAuth}) {
		t.Error("an authentication failure should not be retried at another dialect")
	}
}

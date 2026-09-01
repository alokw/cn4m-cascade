package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// syncEvents collects events from the worker goroutines.
type syncEvents struct {
	mu     sync.Mutex
	events []Event
}

func (s *syncEvents) sink() EventSink {
	return func(e Event) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.events = append(s.events, e)
	}
}

func (s *syncEvents) byLevel(level store.EventLevel) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Event
	for _, e := range s.events {
		if e.Level == level {
			out = append(out, e)
		}
	}
	return out
}

// syncTrees scans, diffs and executes, the way a real run does.
func syncTrees(t *testing.T, src, dst string, opts ExecOptions, diffOpts DiffOptions) (*ExecResult, *Plan, *syncEvents) {
	t.Helper()
	ctx := context.Background()

	srcScan, err := (&Scanner{}).Scan(ctx, src)
	if err != nil {
		t.Fatalf("scanning the source: %v", err)
	}
	dstScan, err := (&Scanner{}).Scan(ctx, dst)
	if err != nil {
		t.Fatalf("scanning the destination: %v", err)
	}

	plan := Diff(srcScan, dstScan, diffOpts)
	events := &syncEvents{}
	exec := &Executor{Copier: fastCopier(), OnEvent: events.sink()}

	res, err := exec.Execute(ctx, src, dst, plan, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res, plan, events
}

func mirrorExec() (ExecOptions, DiffOptions) {
	return ExecOptions{Workers: 4, OnError: store.ErrorPolicySkip, DeletePolicy: store.DeletePolicySkipDeletes},
		DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second}
}

func TestExecuteMirrorsATree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "a.txt", "dir/b.txt", "dir/deep/c.txt", "ünïcodé-📁.txt", "empty/")
	writeTree(t, dst, "extra.txt", "extradir/gone.txt")

	execOpts, diffOpts := mirrorExec()
	res, _, _ := syncTrees(t, src, dst, execOpts, diffOpts)

	if res.Failed() {
		t.Fatalf("failures: %+v", res.Failures)
	}
	if res.FilesCopied != 4 {
		t.Errorf("copied %d files, want 4", res.FilesCopied)
	}
	if res.FilesDeleted != 2 {
		t.Errorf("deleted %d files, want 2", res.FilesDeleted)
	}

	assertTreesMatch(t, src, dst)
}

// The exit criterion: a second run of an already-synced tree must do nothing.
func TestRerunIsANoOp(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "a.txt", "dir/b.txt", "dir/deep/c.txt", "big.bin")
	if err := os.WriteFile(filepath.Join(src, "big.bin"), make([]byte, 5<<20), 0o644); err != nil {
		t.Fatalf("writing big.bin: %v", err)
	}

	execOpts, diffOpts := mirrorExec()

	first, _, _ := syncTrees(t, src, dst, execOpts, diffOpts)
	if first.FilesCopied != 4 {
		t.Fatalf("first run copied %d files, want 4", first.FilesCopied)
	}

	second, plan, _ := syncTrees(t, src, dst, execOpts, diffOpts)
	if !plan.Empty() {
		t.Fatalf("the re-run planned %d actions, want none: %+v", len(plan.Actions), plan.Actions)
	}
	if second.FilesCopied != 0 {
		t.Errorf("the re-run copied %d files, want 0", second.FilesCopied)
	}
}

func TestExecuteUpdateModeLeavesExtrasAlone(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "new.txt")
	writeTree(t, dst, "keepme.txt")

	res, _, _ := syncTrees(t, src, dst,
		ExecOptions{Workers: 2, OnError: store.ErrorPolicySkip, DeletePolicy: store.DeletePolicySkipDeletes},
		DiffOptions{Mode: store.ModeUpdate, Tolerance: 2 * time.Second})

	if res.FilesDeleted != 0 {
		t.Errorf("update mode deleted %d files", res.FilesDeleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "keepme.txt")); err != nil {
		t.Errorf("update mode removed an existing file: %v", err)
	}
}

// Copy failures withhold deletions under the default policy.
func TestFailedCopyWithholdsDeletions(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not prevent writes")
	}

	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "ok.txt", "locked/blocked.txt")
	writeTree(t, dst, "extra.txt", "locked/")

	// The destination directory cannot be written into, so its copy fails.
	locked := filepath.Join(dst, "locked")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	execOpts, diffOpts := mirrorExec()
	res, _, events := syncTrees(t, src, dst, execOpts, diffOpts)

	if !res.Failed() {
		t.Fatal("the blocked copy did not fail")
	}
	if !res.DeletionsSkipped {
		t.Fatal("deletions were not withheld after a copy failure")
	}
	if res.FilesDeleted != 0 {
		t.Errorf("deleted %d files despite a failure", res.FilesDeleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "extra.txt")); err != nil {
		t.Error("an extraneous file was deleted even though a copy failed")
	}
	if len(events.byLevel(store.LevelWarn)) == 0 {
		t.Error("nothing was logged to explain the withheld deletions")
	}
}

// ...unless the job opts into deleting anyway.
func TestDeletePolicyProceedDeletesDespiteFailures(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not prevent writes")
	}

	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "ok.txt", "locked/blocked.txt")
	writeTree(t, dst, "extra.txt", "locked/")

	locked := filepath.Join(dst, "locked")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, _, _ := syncTrees(t, src, dst,
		ExecOptions{Workers: 2, OnError: store.ErrorPolicySkip, DeletePolicy: store.DeletePolicyProceed},
		DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second})

	if res.DeletionsSkipped {
		t.Error("deletions were withheld even though the policy is proceed")
	}
	if _, err := os.Stat(filepath.Join(dst, "extra.txt")); !os.IsNotExist(err) {
		t.Error("the extraneous file survived under delete_policy=proceed")
	}
}

func TestOnErrorAbortStopsTheRun(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not prevent writes")
	}

	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "locked/blocked.txt")
	for i := range 50 {
		writeTree(t, src, fmt.Sprintf("f%02d.txt", i))
	}
	writeTree(t, dst, "locked/")

	locked := filepath.Join(dst, "locked")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, _, _ := syncTrees(t, src, dst,
		ExecOptions{Workers: 1, OnError: store.ErrorPolicyAbort, DeletePolicy: store.DeletePolicySkipDeletes},
		DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second})

	if !res.Aborted {
		t.Fatal("on_error=abort did not stop the run")
	}
	if res.FilesCopied >= 50 {
		t.Errorf("copied %d files after aborting; the run should have stopped early", res.FilesCopied)
	}
}

// A file that disappears between scan and copy is a warning, not a failure.
func TestVanishedSourceFileIsAWarning(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "here.txt", "gone.txt")

	ctx := context.Background()
	srcScan, err := (&Scanner{}).Scan(ctx, src)
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	dstScan, err := (&Scanner{}).Scan(ctx, dst)
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}

	// Remove it after the scan, before the copy.
	if err := os.Remove(filepath.Join(src, "gone.txt")); err != nil {
		t.Fatalf("removing: %v", err)
	}

	events := &syncEvents{}
	exec := &Executor{Copier: fastCopier(), OnEvent: events.sink()}
	res, err := exec.Execute(ctx, src, dst,
		Diff(srcScan, dstScan, DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second}),
		ExecOptions{Workers: 2, OnError: store.ErrorPolicySkip, DeletePolicy: store.DeletePolicySkipDeletes})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if res.Failed() {
		t.Errorf("a vanished source file was treated as a failure: %+v", res.Failures)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", res.Skipped)
	}
	if len(events.byLevel(store.LevelWarn)) != 1 {
		t.Errorf("warnings = %d, want 1", len(events.byLevel(store.LevelWarn)))
	}
}

func TestExecuteIsCancellable(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for i := range 100 {
		writeTree(t, src, fmt.Sprintf("f%03d.bin", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	srcScan, err := (&Scanner{}).Scan(context.Background(), src)
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	dstScan, err := (&Scanner{}).Scan(context.Background(), dst)
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	plan := Diff(srcScan, dstScan, DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second})

	exec := &Executor{Copier: fastCopier()}
	cancel()

	res, _ := exec.Execute(ctx, src, dst, plan,
		ExecOptions{Workers: 4, OnError: store.ErrorPolicySkip, DeletePolicy: store.DeletePolicySkipDeletes})
	if res.FilesCopied == 100 {
		t.Error("a cancelled execution copied every file")
	}
}

func TestExecuteUpdatesTheTracker(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "a.txt", "b.txt", "c.txt")

	ctx := context.Background()
	srcScan, _ := (&Scanner{}).Scan(ctx, src)
	dstScan, _ := (&Scanner{}).Scan(ctx, dst)
	plan := Diff(srcScan, dstScan, DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second})

	tracker := NewTracker(time.Now())
	tracker.SetTotals(int64(plan.Copies), plan.CopyBytes)

	exec := &Executor{Copier: fastCopier(), Tracker: tracker}
	if _, err := exec.Execute(ctx, src, dst, plan,
		ExecOptions{Workers: 2, OnError: store.ErrorPolicySkip, DeletePolicy: store.DeletePolicySkipDeletes}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	snap := tracker.Snapshot(time.Now())
	if snap.FilesDone != 3 {
		t.Errorf("tracker files done = %d, want 3", snap.FilesDone)
	}
	if snap.BytesDone != plan.CopyBytes {
		t.Errorf("tracker bytes done = %d, want %d", snap.BytesDone, plan.CopyBytes)
	}
	if len(snap.InFlight) != 0 {
		t.Errorf("files still in flight after completion: %+v", snap.InFlight)
	}
}

// assertTreesMatch checks that dst holds exactly what src does, with matching
// sizes and modification times.
func assertTreesMatch(t *testing.T, src, dst string) {
	t.Helper()
	ctx := context.Background()

	srcScan, err := (&Scanner{}).Scan(ctx, src)
	if err != nil {
		t.Fatalf("scanning the source: %v", err)
	}
	dstScan, err := (&Scanner{}).Scan(ctx, dst)
	if err != nil {
		t.Fatalf("scanning the destination: %v", err)
	}

	for rel, srcEntry := range srcScan.Entries {
		dstEntry, ok := dstScan.Entries[rel]
		if !ok {
			t.Errorf("%s is missing from the destination", rel)
			continue
		}
		if srcEntry.IsDir != dstEntry.IsDir {
			t.Errorf("%s: directory-ness differs", rel)
			continue
		}
		if srcEntry.IsDir {
			continue
		}
		if srcEntry.Size != dstEntry.Size {
			t.Errorf("%s: size %d vs %d", rel, srcEntry.Size, dstEntry.Size)
		}
		if !srcEntry.ModTime.Equal(dstEntry.ModTime) {
			t.Errorf("%s: mtime %v vs %v", rel, srcEntry.ModTime, dstEntry.ModTime)
		}
	}
	for rel := range dstScan.Entries {
		if _, ok := srcScan.Entries[rel]; !ok {
			t.Errorf("%s is at the destination but not the source", rel)
		}
	}
}

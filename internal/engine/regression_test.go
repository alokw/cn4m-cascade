package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

func updateOpts() DiffOptions {
	return DiffOptions{Mode: store.ModeUpdate, Tolerance: 2 * time.Second}
}

// SPEC.md §1: "Update — copy new/newer files to destination, never delete."
// The type-conflict branches used to sit outside the mirror guard, so update
// mode would delete a destination file or directory.
func TestUpdateModeNeverDeletesEvenOnTypeConflicts(t *testing.T) {
	tests := []struct {
		name string
		src  map[string]int64
		dst  map[string]int64
	}{
		{
			name: "a file where the source has a directory",
			src:  map[string]int64{"thing/": 0},
			dst:  map[string]int64{"thing": 10},
		},
		{
			name: "a directory where the source has a file",
			src:  map[string]int64{"thing": 10},
			dst:  map[string]int64{"thing/": 0},
		},
		{
			name: "an extraneous file",
			src:  map[string]int64{"a.txt": 1},
			dst:  map[string]int64{"a.txt": 1, "extra.txt": 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Diff(tree(tt.src), tree(tt.dst), updateOpts())

			for _, a := range plan.Actions {
				if a.Kind == ActionDelete || a.Kind == ActionRmDir {
					t.Fatalf("update mode planned %s on %q: it must never delete", a.Kind, a.RelPath)
				}
			}
		})
	}
}

// Update copies new and newer files only; a newer destination is left alone.
func TestUpdateModeOnlyCopiesNewerFiles(t *testing.T) {
	tests := []struct {
		name       string
		srcMTime   time.Time
		dstMTime   time.Time
		srcSize    int64
		dstSize    int64
		wantCopied bool
	}{
		{"source newer", base.Add(time.Hour), base, 10, 10, true},
		{"source older", base, base.Add(time.Hour), 10, 10, false},
		{"source older but bigger", base, base.Add(time.Hour), 99, 10, false},
		{"same age, different size", base, base, 99, 10, false},
		{"identical", base, base, 10, 10, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &ScanResult{Entries: map[string]Entry{
				"f.txt": {RelPath: "f.txt", Size: tt.srcSize, ModTime: tt.srcMTime},
			}}
			dst := &ScanResult{Entries: map[string]Entry{
				"f.txt": {RelPath: "f.txt", Size: tt.dstSize, ModTime: tt.dstMTime},
			}}

			plan := Diff(src, dst, updateOpts())
			copied := counts(plan)[ActionCopy] == 1

			if copied != tt.wantCopied {
				t.Fatalf("copied = %v, want %v", copied, tt.wantCopied)
			}
			if !copied && tt.dstSize != tt.srcSize && len(plan.Conflicts) == 0 {
				t.Error("a skipped file was not reported as a conflict")
			}
		})
	}
}

// Mirror still overwrites regardless of direction: it makes the destination
// match the source exactly.
func TestMirrorCopiesOlderSourceOverNewerDestination(t *testing.T) {
	src := &ScanResult{Entries: map[string]Entry{
		"f.txt": {RelPath: "f.txt", Size: 10, ModTime: base},
	}}
	dst := &ScanResult{Entries: map[string]Entry{
		"f.txt": {RelPath: "f.txt", Size: 10, ModTime: base.Add(time.Hour)},
	}}

	if counts(Diff(src, dst, mirrorOpts()))[ActionCopy] != 1 {
		t.Fatal("mirror did not overwrite a newer destination file")
	}
}

// On a case-insensitive destination, a case-only rename used to plan a copy
// and a delete that folded onto the same file — the copy landed and was then
// deleted, leaving nothing at all.
func TestCaseOnlyRenameDoesNotDeleteTheFileJustCopied(t *testing.T) {
	src := tree(map[string]int64{"Report.txt": 10})
	dst := tree(map[string]int64{"report.txt": 10})

	opts := mirrorOpts()
	opts.CaseInsensitiveDest = true
	plan := Diff(src, dst, opts)

	for _, a := range plan.Actions {
		if a.Kind == ActionDelete && strings.EqualFold(a.RelPath, "report.txt") {
			t.Fatalf("planned to delete %q, which is the file the copy of %q lands on", a.RelPath, "Report.txt")
		}
	}
	if len(plan.Conflicts) == 0 {
		t.Error("the case difference was not reported")
	}
}

// Two source paths differing only in case would both write to one
// destination name, and which survived was a race.
func TestCaseCollidingSourcePathsAreNotBothCopied(t *testing.T) {
	src := tree(map[string]int64{"a.txt": 1, "A.txt": 2})
	dst := tree(map[string]int64{})

	opts := mirrorOpts()
	opts.CaseInsensitiveDest = true
	plan := Diff(src, dst, opts)

	if got := counts(plan)[ActionCopy]; got != 1 {
		t.Fatalf("copies = %d, want 1: colliding paths must not both be copied", got)
	}
	if len(plan.Conflicts) == 0 {
		t.Fatal("the collision was not reported")
	}
}

// A case-sensitive destination keeps exact-match semantics.
func TestCaseSensitiveDestinationTreatsCaseAsDistinct(t *testing.T) {
	src := tree(map[string]int64{"a.txt": 1, "A.txt": 2})
	dst := tree(map[string]int64{})

	if got := counts(Diff(src, dst, mirrorOpts()))[ActionCopy]; got != 2 {
		t.Fatalf("copies = %d, want 2 on a case-sensitive destination", got)
	}
}

// A source that lists as empty is far more likely to be a dropped mount than
// a request to delete the entire destination.
func TestEmptySourceDoesNotWipeTheDestination(t *testing.T) {
	src := tree(map[string]int64{})
	dst := tree(map[string]int64{"a.txt": 1, "b/": 0, "b/c.txt": 2})

	plan := Diff(src, dst, mirrorOpts())

	if got := counts(plan); got[ActionDelete] != 0 || got[ActionRmDir] != 0 {
		t.Fatalf("an empty source planned %v: the destination would have been wiped", got)
	}
	if !plan.DeletionsBlocked {
		t.Fatal("deletions were not reported as withheld")
	}
	if !strings.Contains(plan.BlockedReason, "not really mounted") {
		t.Errorf("reason %q does not explain the likely cause", plan.BlockedReason)
	}
}

// Both trees empty is a legitimate no-op, not a blocked deletion.
func TestEmptySourceAndEmptyDestinationIsANoOp(t *testing.T) {
	plan := Diff(tree(map[string]int64{}), tree(map[string]int64{}), mirrorOpts())

	if !plan.Empty() || plan.DeletionsBlocked {
		t.Fatalf("two empty trees produced %+v", plan)
	}
}

// A file vanishing between listing and stat is routine on a live tree. It
// used to be recorded as a scan error, which blocked every deletion and made
// every run partial.
func TestVanishedSourceFileDoesNotBlockDeletions(t *testing.T) {
	src := &ScanResult{
		Entries:  map[string]Entry{"kept.txt": {RelPath: "kept.txt", Size: 1, ModTime: base}},
		Vanished: []string{"gone.txt"},
	}
	dst := tree(map[string]int64{"kept.txt": 1, "extra.txt": 2})

	if src.Incomplete() {
		t.Fatal("a vanished file marked the scan incomplete")
	}

	plan := Diff(src, dst, mirrorOpts())
	if counts(plan)[ActionDelete] != 1 {
		t.Fatalf("deletions were withheld because a file vanished: %+v", plan)
	}
	if plan.DeletionsBlocked {
		t.Error("deletions were reported as blocked")
	}
}

// A symlink at the destination must never be written through: a mkdir or a
// rename would follow it and land outside the destination root.
func TestDestinationSymlinkIsClearedNotFollowed(t *testing.T) {
	src := tree(map[string]int64{"linked/": 0, "linked/inner.txt": 5})
	dst := &ScanResult{Entries: map[string]Entry{
		"linked": {RelPath: "linked", IsSymlink: true},
	}}

	plan := Diff(src, dst, mirrorOpts())

	var cleared bool
	for _, a := range plan.Actions {
		if a.RelPath == "linked" && a.Unblock {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("the destination symlink was not cleared before writing: %+v", plan.Actions)
	}

	// And the clearing must be ordered before the directory is created.
	for _, a := range plan.Actions {
		if a.Kind == ActionMkDir && a.RelPath == "linked" {
			t.Fatal("mkdir on the symlink path is ordered before it was cleared")
		}
		if a.Unblock && a.RelPath == "linked" {
			break
		}
	}
}

// Update mode reports a destination symlink rather than removing it.
func TestUpdateModeLeavesDestinationSymlinksAlone(t *testing.T) {
	src := tree(map[string]int64{"linked": 5})
	dst := &ScanResult{Entries: map[string]Entry{
		"linked": {RelPath: "linked", IsSymlink: true},
	}}

	plan := Diff(src, dst, updateOpts())

	for _, a := range plan.Actions {
		if a.Kind == ActionDelete || a.Kind == ActionRmDir {
			t.Fatalf("update mode planned %s on a symlink", a.Kind)
		}
	}
	if len(plan.Conflicts) == 0 {
		t.Error("the symlink conflict was not reported")
	}
}

// A source symlink is skipped and reported, never copied.
func TestSourceSymlinkIsSkipped(t *testing.T) {
	src := &ScanResult{Entries: map[string]Entry{
		"link": {RelPath: "link", IsSymlink: true},
	}}

	plan := Diff(src, tree(map[string]int64{}), mirrorOpts())

	if counts(plan)[ActionCopy] != 0 {
		t.Fatal("a source symlink was copied")
	}
	if len(plan.Conflicts) == 0 {
		t.Error("the skipped symlink was not reported")
	}
}

// Blocking removals must be ordered before the creates that depend on them,
// and must not be skipped by the delete guard when copies fail.
func TestTypeConflictsAreResolvedOnDisk(t *testing.T) {
	tests := []struct {
		name     string
		srcTree  []string
		setupDst func(t *testing.T, dst string)
		check    func(t *testing.T, dst string)
	}{
		{
			name:    "a file blocking a directory",
			srcTree: []string{"thing/inner.txt"},
			setupDst: func(t *testing.T, dst string) {
				if err := os.WriteFile(filepath.Join(dst, "thing"), []byte("in the way"), 0o644); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			},
			check: func(t *testing.T, dst string) {
				info, err := os.Stat(filepath.Join(dst, "thing"))
				if err != nil {
					t.Fatalf("thing is missing: %v", err)
				}
				if !info.IsDir() {
					t.Error("thing is still a file")
				}
				if _, err := os.Stat(filepath.Join(dst, "thing", "inner.txt")); err != nil {
					t.Errorf("thing/inner.txt was not copied: %v", err)
				}
			},
		},
		{
			name:    "a directory blocking a file",
			srcTree: []string{"thing"},
			setupDst: func(t *testing.T, dst string) {
				if err := os.Mkdir(filepath.Join(dst, "thing"), 0o755); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			},
			check: func(t *testing.T, dst string) {
				info, err := os.Stat(filepath.Join(dst, "thing"))
				if err != nil {
					t.Fatalf("thing is missing: %v", err)
				}
				if info.IsDir() {
					t.Error("thing is still a directory")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			writeTree(t, src, tt.srcTree...)
			tt.setupDst(t, dst)

			execOpts, diffOpts := mirrorExec()
			res, _, _ := syncTrees(t, src, dst, execOpts, diffOpts)

			if res.Failed() {
				t.Fatalf("failures: %+v", res.Failures)
			}
			tt.check(t, dst)

			// And it must be idempotent, not stuck failing forever.
			_, plan, _ := syncTrees(t, src, dst, execOpts, diffOpts)
			if !plan.Empty() {
				t.Errorf("the re-run still planned work: %+v", plan.Actions)
			}
		})
	}
}

// A destination symlink is cleared on disk rather than followed.
func TestDestinationSymlinkIsRemovedOnDisk(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	outside := t.TempDir()

	writeTree(t, src, "linked/inner.txt")
	if err := os.Symlink(outside, filepath.Join(dst, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	execOpts, diffOpts := mirrorExec()
	if res, _, _ := syncTrees(t, src, dst, execOpts, diffOpts); res.Failed() {
		t.Fatalf("failures: %+v", res.Failures)
	}

	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("files were written outside the destination root: %v (err %v)", entries, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "linked", "inner.txt")); err != nil {
		t.Errorf("the file was not copied into the destination: %v", err)
	}
}

// Bytes from a permanently failed copy must not be left in the totals.
func TestFailedCopyDoesNotLeavePhantomProgress(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the blocker: %v", err)
	}

	tests := []struct {
		name   string
		copier *Copier
	}{
		{"retries exhausted", fastCopier()},
		{
			name: "share declared dead",
			copier: &Copier{
				Backoff:     []time.Duration{time.Millisecond},
				ShouldRetry: func(error) bool { return false },
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var net int64
			_, err := tt.copier.Copy(context.Background(), src,
				filepath.Join(blocker, "dst.bin"), time.Time{}, func(n int64) { net += n })
			if err == nil {
				t.Fatal("Copy succeeded unexpectedly")
			}
			if net != 0 {
				t.Errorf("net progress = %d, want 0", net)
			}
		})
	}
}

// SPEC.md §6.3 asks for three attempts, not four.
func TestCopyMakesThreeAttempts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the blocker: %v", err)
	}

	res, err := fastCopier().Copy(context.Background(), src,
		filepath.Join(blocker, "dst.bin"), time.Time{}, nil)
	if err == nil {
		t.Fatal("Copy succeeded unexpectedly")
	}
	if res.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", res.Attempts)
	}
}

// Deletions can never be undone by re-running, so they are logged
// individually regardless of the job's per-file logging setting.
func TestDeletionsAreAlwaysLoggedIndividually(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "keep.txt")
	writeTree(t, dst, "keep.txt", "gone-one.txt", "gone-two.txt", "olddir/inner.txt")

	execOpts, diffOpts := mirrorExec()
	execOpts.LogEveryFile = false // explicitly off

	res, _, events := syncTrees(t, src, dst, execOpts, diffOpts)
	if res.Failed() {
		t.Fatalf("failures: %+v", res.Failures)
	}

	logged := map[string]bool{}
	for _, e := range events.byLevel(store.LevelInfo) {
		if strings.HasPrefix(e.Message, "deleted") {
			logged[e.RelPath] = true
		}
	}
	for _, want := range []string{"gone-one.txt", "gone-two.txt", "olddir/inner.txt"} {
		if !logged[want] {
			t.Errorf("%s was deleted without an individual log entry", want)
		}
	}

	// The removed directory is recorded too.
	var sawDirRemoval bool
	for _, e := range events.byLevel(store.LevelInfo) {
		if e.RelPath == "olddir" && strings.Contains(e.Message, "removed the directory") {
			sawDirRemoval = true
		}
	}
	if !sawDirRemoval {
		t.Error("the removed directory was not logged")
	}
}

// An overwrite replaces data that was already there, so it is logged even
// when per-file logging is off. A brand-new file is not, because a 100k-file
// first run would otherwise write 100k rows.
func TestOverwritesAreLoggedButNewFilesAreNot(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, "changed.txt", "brand-new.txt")
	writeTree(t, dst, "changed.txt")

	// Make the destination copy differ so it is planned as an overwrite.
	if err := os.WriteFile(filepath.Join(dst, "changed.txt"), []byte("different length entirely"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	execOpts, diffOpts := mirrorExec()
	execOpts.LogEveryFile = false

	res, _, events := syncTrees(t, src, dst, execOpts, diffOpts)
	if res.Failed() {
		t.Fatalf("failures: %+v", res.Failures)
	}

	var sawUpdate, sawNew bool
	for _, e := range events.byLevel(store.LevelInfo) {
		if e.RelPath == "changed.txt" && strings.HasPrefix(e.Message, "updated") {
			sawUpdate = true
		}
		if e.RelPath == "brand-new.txt" {
			sawNew = true
		}
	}
	if !sawUpdate {
		t.Error("an overwrite was not logged individually")
	}
	if sawNew {
		t.Error("a brand-new file was logged despite log_every_file being off")
	}

	// With the setting on, new files are logged too.
	src2, dst2 := t.TempDir(), t.TempDir()
	writeTree(t, src2, "brand-new.txt")

	execOpts.LogEveryFile = true
	_, _, events2 := syncTrees(t, src2, dst2, execOpts, diffOpts)

	var sawNewWhenAsked bool
	for _, e := range events2.byLevel(store.LevelInfo) {
		if e.RelPath == "brand-new.txt" {
			sawNewWhenAsked = true
		}
	}
	if !sawNewWhenAsked {
		t.Error("log_every_file did not log a new file")
	}
}

// CLAUDE.md hard rule: a caller must never be trapped in a syscall against a
// dead share. The bound is on how long the caller waits, not on the syscall.
func TestBoundedGivesUpWhenTheOperationNeverReturns(t *testing.T) {
	started := time.Now()

	_, err := bounded(context.Background(), 100*time.Millisecond, "a call that never returns",
		func() (int, error) {
			time.Sleep(10 * time.Second)
			return 1, nil
		})

	if err == nil {
		t.Fatal("bounded waited for an operation that never returned")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("bounded blocked for %v; the caller must not be trapped", elapsed)
	}
	if !strings.Contains(err.Error(), "did not respond in time") {
		t.Errorf("error %q does not explain the timeout", err)
	}
}

// A copy that stops moving bytes is abandoned rather than waited on forever,
// and the stall is classified as a transport failure so the run aborts.
func TestCopyStallIsTreatedAsATransportFailure(t *testing.T) {
	if !TransportError(ErrStalled) {
		t.Fatal("a stalled copy is not classified as a transport failure, so the run would not abort")
	}
	if !retryable(ErrStalled) {
		t.Error("a stalled copy should be retryable: a brief hang may recover")
	}
}

package engine

import (
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

var base = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// tree builds a ScanResult from a compact description. A trailing "/" marks a
// directory; otherwise the value is the file size.
func tree(entries map[string]int64, errs ...ScanError) *ScanResult {
	res := &ScanResult{Entries: map[string]Entry{}, Errors: errs}
	for path, size := range entries {
		if path[len(path)-1] == '/' {
			rel := path[:len(path)-1]
			res.Entries[rel] = Entry{RelPath: rel, IsDir: true}
			res.Dirs++
			continue
		}
		res.Entries[path] = Entry{RelPath: path, Size: size, ModTime: base}
		res.Files++
		res.Bytes += size
	}
	return res
}

func mirrorOpts() DiffOptions {
	return DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second}
}

// counts returns how many actions of each kind a plan holds.
func counts(p *Plan) map[ActionKind]int {
	out := map[ActionKind]int{}
	for _, a := range p.Actions {
		out[a.Kind]++
	}
	return out
}

func TestDiffIdenticalTreesIsANoOp(t *testing.T) {
	src := tree(map[string]int64{"a/": 0, "a/one.txt": 10, "two.txt": 20})
	dst := tree(map[string]int64{"a/": 0, "a/one.txt": 10, "two.txt": 20})

	plan := Diff(src, dst, mirrorOpts())

	if !plan.Empty() {
		t.Fatalf("an already-synced tree produced %d actions: %+v", len(plan.Actions), plan.Actions)
	}
	if plan.CopyBytes != 0 {
		t.Errorf("copy bytes = %d, want 0", plan.CopyBytes)
	}
}

func TestDiffMirror(t *testing.T) {
	src := tree(map[string]int64{
		"keep.txt":    10,
		"new.txt":     30,
		"changed.txt": 40,
		"newdir/":     0,
	})
	dst := tree(map[string]int64{
		"keep.txt":       10,
		"changed.txt":    99, // different size
		"extra.txt":      50,
		"extradir/":      0,
		"extradir/x.txt": 5,
	})

	plan := Diff(src, dst, mirrorOpts())
	got := counts(plan)

	if got[ActionCopy] != 2 {
		t.Errorf("copies = %d, want 2 (new.txt, changed.txt)", got[ActionCopy])
	}
	if got[ActionMkDir] != 1 {
		t.Errorf("mkdirs = %d, want 1 (newdir)", got[ActionMkDir])
	}
	if got[ActionDelete] != 2 {
		t.Errorf("deletes = %d, want 2 (extra.txt, extradir/x.txt)", got[ActionDelete])
	}
	if got[ActionRmDir] != 1 {
		t.Errorf("rmdirs = %d, want 1 (extradir)", got[ActionRmDir])
	}
	if plan.CopyBytes != 70 {
		t.Errorf("copy bytes = %d, want 70", plan.CopyBytes)
	}
}

// Update never deletes (SPEC.md §1). The conflict cases live in
// regression_test.go.
func TestDiffUpdateNeverDeletes(t *testing.T) {
	src := tree(map[string]int64{"new.txt": 30})
	dst := tree(map[string]int64{"extra.txt": 50, "extradir/": 0})

	plan := Diff(src, dst, DiffOptions{Mode: store.ModeUpdate, Tolerance: 2 * time.Second})
	got := counts(plan)

	if got[ActionDelete] != 0 || got[ActionRmDir] != 0 {
		t.Fatalf("update mode planned removals: %+v", plan.Actions)
	}
	if got[ActionCopy] != 1 {
		t.Errorf("copies = %d, want 1", got[ActionCopy])
	}
}

func TestDiffComparison(t *testing.T) {
	tests := []struct {
		name       string
		srcSize    int64
		dstSize    int64
		srcMTime   time.Time
		dstMTime   time.Time
		ignoreDST  bool
		wantCopied bool
	}{
		{
			name: "identical", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base, wantCopied: false,
		},
		{
			name: "size differs", srcSize: 10, dstSize: 11,
			srcMTime: base, dstMTime: base, wantCopied: true,
		},
		{
			name: "mtime within tolerance", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base.Add(1500 * time.Millisecond), wantCopied: false,
		},
		{
			name: "mtime at the tolerance boundary", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base.Add(2 * time.Second), wantCopied: false,
		},
		{
			name: "mtime beyond tolerance", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base.Add(5 * time.Second), wantCopied: true,
		},
		{
			name: "source older, still beyond tolerance", srcSize: 10, dstSize: 10,
			srcMTime: base.Add(-5 * time.Second), dstMTime: base, wantCopied: true,
		},
		{
			name: "whole hour off without the DST toggle", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base.Add(time.Hour), wantCopied: true,
		},
		{
			name: "whole hour off with the DST toggle", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base.Add(time.Hour), ignoreDST: true, wantCopied: false,
		},
		{
			name: "whole hour the other way with the DST toggle", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base.Add(-time.Hour), ignoreDST: true, wantCopied: false,
		},
		{
			name: "DST toggle does not excuse a size difference", srcSize: 10, dstSize: 20,
			srcMTime: base, dstMTime: base.Add(time.Hour), ignoreDST: true, wantCopied: true,
		},
		{
			name: "DST toggle does not excuse an arbitrary offset", srcSize: 10, dstSize: 10,
			srcMTime: base, dstMTime: base.Add(37 * time.Minute), ignoreDST: true, wantCopied: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &ScanResult{Entries: map[string]Entry{
				"f.txt": {RelPath: "f.txt", Size: tt.srcSize, ModTime: tt.srcMTime},
			}}
			dst := &ScanResult{Entries: map[string]Entry{
				"f.txt": {RelPath: "f.txt", Size: tt.dstSize, ModTime: tt.dstMTime},
			}}

			plan := Diff(src, dst, DiffOptions{
				Mode: store.ModeMirror, Tolerance: 2 * time.Second, IgnoreDSTHour: tt.ignoreDST,
			})

			copied := counts(plan)[ActionCopy] == 1
			if copied != tt.wantCopied {
				t.Fatalf("copied = %v, want %v (plan: %+v)", copied, tt.wantCopied, plan.Actions)
			}
		})
	}
}

// The guard that keeps a mirror from eating data when the source could not be
// read in full.
func TestDiffBlocksDeletionsWhenTheSourceScanIsIncomplete(t *testing.T) {
	src := tree(map[string]int64{"kept.txt": 10}, ScanError{RelPath: "unreadable", Err: errUnreadable{}})
	dst := tree(map[string]int64{
		"kept.txt":            10,
		"unreadable/":         0,
		"unreadable/data.bin": 500,
		"other.txt":           20,
	})

	plan := Diff(src, dst, mirrorOpts())
	got := counts(plan)

	if got[ActionDelete] != 0 || got[ActionRmDir] != 0 {
		t.Fatalf("deletions were planned from an incomplete source scan: %+v", plan.Actions)
	}
	if !plan.DeletionsBlocked {
		t.Error("the plan did not report that deletions were withheld")
	}
	if plan.BlockedReason == "" {
		t.Error("no reason was recorded for withholding deletions")
	}

	// The actions are gone, but the scale of the refusal has to survive them:
	// "refusing to delete 2 files" is a different message from "deletions
	// blocked", and the count is the only record left once the actions are
	// discarded. The source holds only kept.txt, so "other.txt" and
	// "unreadable/data.bin" are the extraneous files and "unreadable/" the
	// extraneous directory.
	if plan.WithheldDeletes != 2 {
		t.Errorf("withheld deletes = %d, want 2", plan.WithheldDeletes)
	}
	if plan.WithheldRmDirs != 1 {
		t.Errorf("withheld rmdirs = %d, want 1", plan.WithheldRmDirs)
	}
}

// Withholding is only reported when there was actually something to withhold.
func TestDiffIncompleteScanWithNothingToDelete(t *testing.T) {
	src := tree(map[string]int64{"a.txt": 1}, ScanError{RelPath: "x", Err: errUnreadable{}})
	dst := tree(map[string]int64{"a.txt": 1})

	plan := Diff(src, dst, mirrorOpts())
	if plan.DeletionsBlocked {
		t.Error("reported blocked deletions when there were none to make")
	}
	if plan.WithheldDeletes != 0 || plan.WithheldRmDirs != 0 {
		t.Errorf("counted withheld removals when there were none: %d deletes, %d rmdirs",
			plan.WithheldDeletes, plan.WithheldRmDirs)
	}
}

// Parents before children when creating; children before parents when
// removing (SPEC.md §6.1 step 7).
func TestDiffOrdering(t *testing.T) {
	src := tree(map[string]int64{"a/": 0, "a/b/": 0, "a/b/c/": 0})
	dst := tree(map[string]int64{"x/": 0, "x/y/": 0, "x/y/z/": 0})

	plan := Diff(src, dst, mirrorOpts())

	var mkdirs, rmdirs []string
	for _, a := range plan.Actions {
		switch a.Kind {
		case ActionMkDir:
			mkdirs = append(mkdirs, a.RelPath)
		case ActionRmDir:
			rmdirs = append(rmdirs, a.RelPath)
		}
	}

	wantMk := []string{"a", "a/b", "a/b/c"}
	for i, want := range wantMk {
		if mkdirs[i] != want {
			t.Fatalf("mkdir order = %v, want %v", mkdirs, wantMk)
		}
	}
	wantRm := []string{"x/y/z", "x/y", "x"}
	for i, want := range wantRm {
		if rmdirs[i] != want {
			t.Fatalf("rmdir order = %v, want %v", rmdirs, wantRm)
		}
	}
}

// Every directory must be created before anything is copied into it.
func TestDiffCreatesDirectoriesBeforeCopies(t *testing.T) {
	src := tree(map[string]int64{"newdir/": 0, "newdir/file.txt": 10})
	dst := tree(map[string]int64{})

	plan := Diff(src, dst, mirrorOpts())

	var sawMkDir bool
	for _, a := range plan.Actions {
		switch a.Kind {
		case ActionMkDir:
			sawMkDir = true
		case ActionCopy:
			if !sawMkDir {
				t.Fatalf("copy of %s is ordered before its directory is created", a.RelPath)
			}
		}
	}
}

// A file where the source has a directory, and vice versa.
func TestDiffTypeConflicts(t *testing.T) {
	t.Run("file blocking a directory", func(t *testing.T) {
		src := tree(map[string]int64{"thing/": 0})
		dst := tree(map[string]int64{"thing": 10})

		got := counts(Diff(src, dst, mirrorOpts()))
		if got[ActionDelete] != 1 || got[ActionMkDir] != 1 {
			t.Fatalf("plan = %v, want the file deleted and the directory created", got)
		}
	})

	t.Run("directory blocking a file", func(t *testing.T) {
		src := tree(map[string]int64{"thing": 10})
		dst := tree(map[string]int64{"thing/": 0})

		got := counts(Diff(src, dst, mirrorOpts()))
		if got[ActionRmDir] != 1 || got[ActionCopy] != 1 {
			t.Fatalf("plan = %v, want the directory removed and the file copied", got)
		}
	})
}

func TestDiffRecordsReasons(t *testing.T) {
	src := tree(map[string]int64{"new.txt": 10, "changed.txt": 20})
	dst := tree(map[string]int64{"changed.txt": 99, "gone.txt": 5})

	for _, a := range Diff(src, dst, mirrorOpts()).Actions {
		if a.Reason == "" {
			t.Errorf("action %s %s has no reason recorded", a.Kind, a.RelPath)
		}
	}
}

type errUnreadable struct{}

func (errUnreadable) Error() string { return "permission denied" }

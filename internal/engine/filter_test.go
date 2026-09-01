package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// testMatcher excludes any path containing one of its substrings, which is
// enough to exercise the engine's side of filtering without pulling in the
// pattern language.
type testMatcher struct {
	excluded []string
	degraded bool
}

func (m testMatcher) Admits(relPath string, _ bool) bool {
	for _, e := range m.excluded {
		if strings.Contains(relPath, e) {
			return false
		}
	}
	return true
}

func (m testMatcher) PrunesDir(relPath string) bool { return !m.Admits(relPath, true) }

func (m testMatcher) Degraded() (bool, string) { return m.degraded, "test" }

// The headline guarantee: an excluded path is invisible on both sides, so
// mirror never deletes it from the destination.
//
// Filtering only the source would make every excluded path look extraneous,
// and adding a rule to skip copying a directory would silently destroy the
// backup of it.
func TestFilteredPathsAreNeverDeleted(t *testing.T) {
	src := tree(map[string]int64{
		"keep.txt":       10,
		"cache/":         0,
		"cache/blob.bin": 999,
	})
	dst := tree(map[string]int64{
		"keep.txt":       10,
		"cache/":         0,
		"cache/blob.bin": 999,
		"cache/old.bin":  50,
	})

	opts := mirrorOpts()
	opts.Filter = testMatcher{excluded: []string{"cache"}}
	plan := Diff(src, dst, opts)

	for _, a := range plan.Actions {
		if strings.Contains(a.RelPath, "cache") {
			t.Errorf("planned %s on excluded path %q", a.Kind, a.RelPath)
		}
	}
	if !plan.Empty() {
		t.Errorf("plan should be empty, got %+v", plan.Actions)
	}
}

// Excluded paths are not copied either.
func TestFilteredPathsAreNeverCopied(t *testing.T) {
	src := tree(map[string]int64{"keep.txt": 10, "skip.tmp": 20})
	dst := tree(map[string]int64{})

	opts := mirrorOpts()
	opts.Filter = testMatcher{excluded: []string{".tmp"}}
	plan := Diff(src, dst, opts)

	if got := counts(plan)[ActionCopy]; got != 1 {
		t.Fatalf("copies = %d, want 1", got)
	}
	for _, a := range plan.Actions {
		if strings.Contains(a.RelPath, ".tmp") {
			t.Errorf("planned %s on excluded path %q", a.Kind, a.RelPath)
		}
	}
}

// An in-scope destination file that the source genuinely lacks is still
// deleted — filtering narrows the scope, it does not disable mirroring.
func TestMirrorStillDeletesInScopeExtras(t *testing.T) {
	src := tree(map[string]int64{"keep.txt": 10})
	dst := tree(map[string]int64{"keep.txt": 10, "extra.txt": 20, "skip.tmp": 30})

	opts := mirrorOpts()
	opts.Filter = testMatcher{excluded: []string{".tmp"}}
	plan := Diff(src, dst, opts)

	var deleted []string
	for _, a := range plan.Actions {
		if a.Kind == ActionDelete {
			deleted = append(deleted, a.RelPath)
		}
	}
	if len(deleted) != 1 || deleted[0] != "extra.txt" {
		t.Fatalf("deleted %v, want just extra.txt", deleted)
	}
}

// A filter that had to drop a rule must not delete anything: the chain is
// narrower than configured, so paths the dropped rule protected now look
// extraneous.
func TestDegradedFilterBlocksDeletions(t *testing.T) {
	src := tree(map[string]int64{"keep.txt": 10})
	dst := tree(map[string]int64{"keep.txt": 10, "cache/blob.bin": 500, "extra.txt": 20})

	opts := mirrorOpts()
	opts.Filter = testMatcher{degraded: true}
	plan := Diff(src, dst, opts)

	if got := counts(plan); got[ActionDelete] != 0 || got[ActionRmDir] != 0 {
		t.Fatalf("a degraded filter planned removals: %v", got)
	}
	if !plan.DeletionsBlocked {
		t.Fatal("deletions were not reported as withheld")
	}
	if !strings.Contains(plan.BlockedReason, "could not be loaded") {
		t.Errorf("reason %q does not explain the cause", plan.BlockedReason)
	}
}

// An include rule must still create the directories its files need, even
// though the rule never matches a directory itself.
func TestIncludeRuleStillCreatesParentDirectories(t *testing.T) {
	src := tree(map[string]int64{"photos/": 0, "photos/a.jpg": 10, "photos/b.txt": 20})
	dst := tree(map[string]int64{})

	opts := mirrorOpts()
	opts.Filter = includeOnly{suffix: ".jpg"}
	plan := Diff(src, dst, opts)

	var mkdirs, copies []string
	for _, a := range plan.Actions {
		switch a.Kind {
		case ActionMkDir:
			mkdirs = append(mkdirs, a.RelPath)
		case ActionCopy:
			copies = append(copies, a.RelPath)
		}
	}

	if len(copies) != 1 || copies[0] != "photos/a.jpg" {
		t.Fatalf("copies = %v, want just photos/a.jpg", copies)
	}
	if len(mkdirs) != 1 || mkdirs[0] != "photos" {
		t.Fatalf("mkdirs = %v, want photos: the copy would fail with no directory to write into", mkdirs)
	}
	// And the directory must be ordered before the copy.
	for _, a := range plan.Actions {
		if a.Kind == ActionCopy {
			t.Fatalf("the copy is ordered before its directory: %+v", plan.Actions)
		}
		if a.Kind == ActionMkDir {
			break
		}
	}
}

// includeOnly admits files with a suffix and nothing else, the shape that
// broke directory creation.
type includeOnly struct{ suffix string }

func (i includeOnly) Admits(relPath string, isDir bool) bool {
	if isDir {
		return false
	}
	return strings.HasSuffix(relPath, i.suffix)
}
func (i includeOnly) PrunesDir(string) bool    { return false }
func (i includeOnly) Degraded() (bool, string) { return false, "" }

// A nil filter admits everything, so unfiltered jobs behave as before.
func TestNilFilterAdmitsEverything(t *testing.T) {
	src := tree(map[string]int64{"a.txt": 1})
	dst := tree(map[string]int64{"b.txt": 2})

	got := counts(Diff(src, dst, mirrorOpts()))
	if got[ActionCopy] != 1 || got[ActionDelete] != 1 {
		t.Fatalf("plan = %v, want one copy and one delete", got)
	}
}

// An excluded directory is never descended into: the point of pruning is not
// paying the latency of listing it at all.
func TestScannerPrunesExcludedDirectories(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"keep/one.txt",
		"cache/a.bin",
		"cache/deep/b.bin",
		"cache/deep/deeper/c.bin",
	)

	matcher := testMatcher{excluded: []string{"cache"}}
	scanner := &Scanner{Prune: func(relDir string) bool { return matcher.PrunesDir(relDir) }}

	res, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for path := range res.Entries {
		if strings.HasPrefix(path, "cache") {
			t.Errorf("%q was scanned despite being pruned", path)
		}
	}
	if _, ok := res.Entries["keep/one.txt"]; !ok {
		t.Error("the rest of the tree was not scanned")
	}
	if len(res.Pruned) != 1 || res.Pruned[0] != "cache" {
		t.Errorf("pruned = %v, want [cache]", res.Pruned)
	}
	// Nothing beneath the pruned directory was even counted.
	if res.Files != 1 {
		t.Errorf("files = %d, want 1", res.Files)
	}
}

// Pruning the source walk must not make mirror delete the pruned subtree at
// the destination: the pruned paths are excluded on both sides.
func TestPrunedSourceSubtreeIsNotDeletedAtTheDestination(t *testing.T) {
	src := &ScanResult{
		Entries: map[string]Entry{"keep.txt": {RelPath: "keep.txt", Size: 1, ModTime: base}},
		Pruned:  []string{"cache"},
	}
	dst := tree(map[string]int64{"keep.txt": 1, "cache/": 0, "cache/blob.bin": 500})

	opts := DiffOptions{Mode: store.ModeMirror, Tolerance: 2 * time.Second,
		Filter: testMatcher{excluded: []string{"cache"}}}

	plan := Diff(src, dst, opts)
	for _, a := range plan.Actions {
		if strings.Contains(a.RelPath, "cache") {
			t.Fatalf("planned %s on a pruned path %q; the destination subtree would be destroyed", a.Kind, a.RelPath)
		}
	}
}

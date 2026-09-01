package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTree materialises a set of relative paths. A path ending in "/" is a
// directory; anything else is a file whose contents are its path.
func writeTree(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if len(p) > 0 && p[len(p)-1] == '/' {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", p, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

func TestScanFindsEverything(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"top.txt",
		"empty/",
		"a/one.txt",
		"a/b/two.txt",
		"a/b/c/three.txt",
		"ünïcodé-📁.txt",
	)

	res, err := (&Scanner{}).Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Files != 5 {
		t.Errorf("files = %d, want 5", res.Files)
	}
	if res.Dirs != 4 {
		t.Errorf("dirs = %d, want 4 (empty, a, a/b, a/b/c)", res.Dirs)
	}
	if res.Incomplete() {
		t.Errorf("scan reported errors: %v", res.Errors)
	}

	for _, want := range []string{"top.txt", "a/one.txt", "a/b/c/three.txt", "ünïcodé-📁.txt"} {
		if _, ok := res.Entries[want]; !ok {
			t.Errorf("missing entry %q", want)
		}
	}
	if e := res.Entries["a/b"]; !e.IsDir {
		t.Error("a/b was not recorded as a directory")
	}
	if got := res.Entries["top.txt"].Size; got != int64(len("top.txt")) {
		t.Errorf("size of top.txt = %d, want %d", got, len("top.txt"))
	}
	if res.Bytes == 0 {
		t.Error("byte total is zero")
	}
}

func TestScanEmptyRoot(t *testing.T) {
	res, err := (&Scanner{}).Scan(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Entries) != 0 || res.Files != 0 {
		t.Fatalf("empty tree produced %+v", res)
	}
	if res.Incomplete() {
		t.Error("an empty directory should not count as incomplete")
	}
}

// v1 policy: symlinks are not synced, and are logged (SPEC.md §13). They are
// still recorded as entries so that a symlink sitting at a *destination* is
// visible to the differ — omitting it would let a later mkdir or rename
// follow the link and write outside the destination root.
func TestScanRecordsSymlinksWithoutFollowingThem(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "real.txt", "dir/inner.txt")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "dir"), filepath.Join(root, "dirlink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res, err := (&Scanner{}).Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, name := range []string{"link.txt", "dirlink"} {
		entry, ok := res.Entries[name]
		if !ok {
			t.Errorf("%s is missing from the entries; a destination symlink must be visible", name)
			continue
		}
		if !entry.IsSymlink {
			t.Errorf("%s was not marked as a symlink", name)
		}
		if entry.IsDir {
			t.Errorf("%s was recorded as a directory", name)
		}
	}

	// Never descended into.
	if _, ok := res.Entries["dirlink/inner.txt"]; ok {
		t.Error("the scan followed a symlinked directory")
	}
	if len(res.Symlinks) != 2 {
		t.Errorf("symlinks recorded = %v, want 2", res.Symlinks)
	}
	// And never counted as real content.
	if res.Files != 2 {
		t.Errorf("files = %d, want 2 (symlinks must not be counted)", res.Files)
	}
	if res.Dirs != 1 {
		t.Errorf("dirs = %d, want 1 (the symlinked directory must not be counted)", res.Dirs)
	}
}

// A file removed between the listing and the stat is routine on a live tree
// and must not mark the scan incomplete — that flag blocks mirror deletions.
func TestScanSeparatesVanishedFilesFromRealErrors(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "stays.txt")

	// A dangling symlink stats as ErrNotExist, which is the same signal a
	// file deleted mid-scan produces, without needing to win a race.
	if err := os.Symlink(filepath.Join(root, "never-existed"), filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res, err := (&Scanner{}).Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Incomplete() {
		t.Errorf("scan reported errors: %v", res.Errors)
	}
}

// An unreadable directory must not fail the whole scan, but must mark the
// result incomplete — that flag is what blocks mirror deletions.
func TestScanRecordsUnreadableDirectories(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not prevent reads")
	}

	root := t.TempDir()
	writeTree(t, root, "readable/file.txt", "locked/hidden.txt")
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, err := (&Scanner{}).Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan returned an error for one unreadable directory: %v", err)
	}

	if !res.Incomplete() {
		t.Fatal("an unreadable directory did not mark the scan incomplete")
	}
	if _, ok := res.Entries["readable/file.txt"]; !ok {
		t.Error("the rest of the tree was not scanned")
	}
}

func TestScanIsCancellable(t *testing.T) {
	root := t.TempDir()
	for i := range 200 {
		writeTree(t, root, fmt.Sprintf("d%03d/f.txt", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&Scanner{}).Scan(ctx, root)
	if err == nil {
		t.Fatal("a cancelled scan returned a result; a partial tree must never look complete")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestScanReportsProgress(t *testing.T) {
	root := t.TempDir()
	for i := range 20 {
		writeTree(t, root, fmt.Sprintf("d%02d/f.txt", i))
	}

	var calls int
	scanner := &Scanner{OnProgress: func(files, dirs, bytes int64) { calls++ }}
	if _, err := scanner.Scan(context.Background(), root); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if calls == 0 {
		t.Error("OnProgress was never called")
	}
}

func TestScanWorkerCounts(t *testing.T) {
	root := t.TempDir()
	for i := range 50 {
		writeTree(t, root, fmt.Sprintf("d%02d/a/b/f.txt", i))
	}

	for _, workers := range []int{0, 1, 4, 16} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			res, err := (&Scanner{Workers: workers}).Scan(context.Background(), root)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if res.Files != 50 {
				t.Errorf("files = %d, want 50", res.Files)
			}
			if res.Dirs != 150 {
				t.Errorf("dirs = %d, want 150", res.Dirs)
			}
		})
	}
}

func TestScanRecordsModTime(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "f.txt")

	when := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(root, "f.txt"), when, when); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	res, err := (&Scanner{}).Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := res.Entries["f.txt"].ModTime; !got.Equal(when) {
		t.Errorf("mtime = %v, want %v", got, when)
	}
}

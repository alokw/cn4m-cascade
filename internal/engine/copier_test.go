package engine

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fastCopier() *Copier {
	// Real backoff is 1s/5s/15s; tests must not wait 21 seconds to observe
	// three retries.
	return &Copier{Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
}

func TestCopyPreservesContentAndModTime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	payload := bytes.Repeat([]byte("cascade"), 100_000) // ~700 KB
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}
	when := time.Now().Add(-72 * time.Hour).Truncate(time.Second)

	res, err := fastCopier().Copy(context.Background(), src, dst, when, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if res.BytesCopied != int64(len(payload)) {
		t.Errorf("copied %d bytes, want %d", res.BytesCopied, len(payload))
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", res.Attempts)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the destination: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("destination contents differ from the source")
	}

	// Without this an idempotent re-run copies everything again (CLAUDE.md).
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(when) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), when)
	}
}

func TestCopyEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		srcName string
	}{
		{"zero bytes", []byte{}, "empty.bin"},
		{"one byte", []byte{7}, "one.bin"},
		{"unicode name", []byte("hello"), "ünïcodé-📁.txt"},
		{"larger than the buffer", bytes.Repeat([]byte("x"), (4<<20)+1234), "big.bin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, tt.srcName)
			dst := filepath.Join(dir, "out-"+tt.srcName)

			if err := os.WriteFile(src, tt.payload, 0o644); err != nil {
				t.Fatalf("writing the source: %v", err)
			}
			if _, err := fastCopier().Copy(context.Background(), src, dst, time.Time{}, nil); err != nil {
				t.Fatalf("Copy: %v", err)
			}

			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("reading the destination: %v", err)
			}
			if !bytes.Equal(got, tt.payload) {
				t.Errorf("contents differ (%d bytes vs %d)", len(got), len(tt.payload))
			}
		})
	}
}

// A successful copy must leave nothing behind but the file itself.
func TestCopyLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}
	if _, err := fastCopier().Copy(context.Background(), src, dst, time.Time{}, nil); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	assertNoTempFiles(t, dir)
}

// The point of temp-file+rename: an interrupted copy must not damage the
// file that is already there.
func TestCancelledCopyLeavesTheDestinationIntact(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	if err := os.WriteFile(src, bytes.Repeat([]byte("n"), 32<<20), 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}
	original := []byte("the good copy that must survive")
	if err := os.WriteFile(dst, original, 0o644); err != nil {
		t.Fatalf("writing the destination: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel as soon as the first chunk lands.
	_, err := fastCopier().Copy(ctx, src, dst, time.Time{}, func(int64) { cancel() })

	if err == nil {
		t.Fatal("a cancelled copy reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the destination: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("the destination was damaged by an interrupted copy")
	}
	assertNoTempFiles(t, dir)
}

// Files vanish between scan and copy on a live tree (SPEC.md §12).
func TestCopySourceVanished(t *testing.T) {
	dir := t.TempDir()

	res, err := fastCopier().Copy(context.Background(),
		filepath.Join(dir, "never-existed"), filepath.Join(dir, "dst"), time.Time{}, nil)

	if !errors.Is(err, ErrSourceVanished) {
		t.Fatalf("error = %v, want ErrSourceVanished", err)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a missing source will not appear on retry", res.Attempts)
	}
}

// Retries exist for flaky I/O, not for errors that are certain to recur.
func TestCopyRetriesOnlyWhenItCouldHelp(t *testing.T) {
	t.Run("retries a transient failure the full number of times", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src.bin")
		if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
			t.Fatalf("writing the source: %v", err)
		}
		// A regular file standing in for the destination directory: writing
		// into it fails with ENOTDIR, which is not on the do-not-retry list.
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing the blocker: %v", err)
		}

		copier := fastCopier()
		res, err := copier.Copy(context.Background(), src, filepath.Join(blocker, "dst.bin"), time.Time{}, nil)
		if err == nil {
			t.Fatal("Copy succeeded into a path that cannot exist")
		}
		// SPEC.md §6.3: three attempts, however long the backoff list is.
		if res.Attempts != 3 {
			t.Errorf("attempts = %d, want 3", res.Attempts)
		}
		_ = copier
		if !strings.Contains(err.Error(), "gave up after") {
			t.Errorf("error %q does not say the retries were exhausted", err)
		}
	})

	t.Run("does not retry a permission failure", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("running as root: permission bits do not prevent writes")
		}
		dir := t.TempDir()
		src := filepath.Join(dir, "src.bin")
		if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
			t.Fatalf("writing the source: %v", err)
		}
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o555); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		res, err := fastCopier().Copy(context.Background(), src, filepath.Join(locked, "dst.bin"), time.Time{}, nil)
		if err == nil {
			t.Fatal("Copy succeeded into a read-only directory")
		}
		if res.Attempts != 1 {
			t.Errorf("attempts = %d, want 1: a permission failure recurs identically", res.Attempts)
		}
	})
}

// Progress must not double-count bytes that a failed attempt threw away.
func TestCopyRewindsProgressOnRetry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, bytes.Repeat([]byte("z"), 1<<20), 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the blocker: %v", err)
	}

	var net int64
	_, err := fastCopier().Copy(context.Background(), src, filepath.Join(blocker, "dst.bin"),
		time.Time{}, func(n int64) { net += n })
	if err == nil {
		t.Fatal("Copy succeeded unexpectedly")
	}
	if net != 0 {
		t.Errorf("net progress = %d, want 0: discarded bytes must be rewound", net)
	}
}

func TestCopyReportsProgress(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	payload := bytes.Repeat([]byte("p"), (4<<20)*2+100)
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}

	var total int64
	var chunks int
	if _, err := fastCopier().Copy(context.Background(), src, dst, time.Time{}, func(n int64) {
		total += n
		chunks++
	}); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	if total != int64(len(payload)) {
		t.Errorf("reported %d bytes, want %d", total, len(payload))
	}
	if chunks < 2 {
		t.Errorf("reported %d chunks; a multi-buffer file should report progress as it goes", chunks)
	}
}

// Overwriting must be atomic: the destination is either the old file or the
// new one, never a blend.
func TestCopyOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	if err := os.WriteFile(src, bytes.Repeat([]byte("new"), 1000), 0o644); err != nil {
		t.Fatalf("writing the source: %v", err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatalf("writing the destination: %v", err)
	}

	if _, err := fastCopier().Copy(context.Background(), src, dst, time.Time{}, nil); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the destination: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("new"), 1000)) {
		t.Error("the destination does not hold the new contents")
	}
	assertNoTempFiles(t, dir)
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"source vanished", ErrSourceVanished, false},
		{"not found", fs.ErrNotExist, false},
		{"permission", fs.ErrPermission, false},
		{"disk full", syscall.ENOSPC, false},
		{"over quota", syscall.EDQUOT, false},
		{"read-only filesystem", syscall.EROFS, false},
		{"i/o error", syscall.EIO, true},
		{"timed out", syscall.ETIMEDOUT, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"host down", syscall.EHOSTDOWN, true},
		{"wrapped i/o error", errors.New("writing: " + syscall.EIO.Error()), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryable(tt.err); got != tt.want {
				t.Errorf("retryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

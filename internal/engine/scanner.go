// Package engine is the sync engine: it scans, diffs and copies. It only
// ever sees local filesystem roots handed to it by internal/storage, and
// never learns whether a root is a CIFS mount or a bind mount (SPEC.md §4).
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Entry is one file or directory found by a scan, addressed by its path
// relative to the scan root.
type Entry struct {
	RelPath   string
	Size      int64
	ModTime   time.Time
	IsDir     bool
	IsSymlink bool
}

// ScanError is a directory or file the scan could not read.
type ScanError struct {
	RelPath string
	Err     error
}

func (e ScanError) Error() string { return fmt.Sprintf("%s: %v", e.RelPath, e.Err) }

// ScanResult is a complete listing of one tree.
type ScanResult struct {
	// Entries is keyed by relative path. The root itself is not included.
	Entries map[string]Entry

	Files int64
	Dirs  int64
	Bytes int64

	// Symlinks counts what was skipped. v1 policy is skip and log
	// (SPEC.md §13).
	Symlinks []string

	// Vanished lists files that disappeared between being listed and being
	// stat'd. On a live tree this is routine (SPEC.md §12) and must not be
	// confused with an unreadable directory: treating it as one would block
	// every mirror deletion and mark every run partial.
	Vanished []string

	// Errors lists directories that could not be read.
	Errors []ScanError

	// Pruned lists directories the walk skipped because a filter excluded
	// them. They were never listed, so nothing beneath them is known.
	Pruned []string

	// Truncated is set when the walk stopped at Scanner.Limit, so the
	// listing is a sample and not the whole tree.
	Truncated bool
}

// Incomplete reports whether any part of the tree could not be read.
//
// This is the single most important flag in a mirror: a source directory
// that failed to list makes everything beneath it look extraneous at the
// destination, and mirroring that would delete live data. Deletions are
// blocked whenever it is set, regardless of the job's delete policy.
func (r *ScanResult) Incomplete() bool { return len(r.Errors) > 0 }

// ScanProgress is called as the scan discovers entries, so the UI can show
// "files/dirs discovered per second" before totals are known (SPEC.md §6.1.1).
//
// It is called from the walker goroutines but never concurrently with
// itself, and each call happens-after the previous one — so an
// implementation may touch ordinary variables without synchronising.
type ScanProgress func(files, dirs, bytes int64)

// Scanner walks a tree with bounded parallelism. SMB metadata operations are
// latency-bound rather than bandwidth-bound, so several walkers in flight is
// a large win over a sequential walk (SPEC.md §6.1).
type Scanner struct {
	// Workers bounds concurrent directory reads. Defaults to DefaultScanWorkers.
	Workers int
	// OnProgress, if set, is called periodically with running totals.
	OnProgress ScanProgress
	// OpTimeout bounds each individual directory read or stat. Zero means
	// DefaultOpTimeout. Nothing here may block forever on a dead share
	// (CLAUDE.md).
	OpTimeout time.Duration

	// Limit stops the walk once this many entries have been found. Zero
	// means no limit. It exists for the filter-test preview, which needs a
	// sample rather than a full listing of a 100k-file tree.
	Limit int

	// Prune, if set, is asked before descending into a directory. Skipping
	// an excluded subtree is where filtering pays for itself: the walk never
	// pays the latency of listing it (SPEC.md §6.1 step 4).
	//
	// Only rules that apply to every destination may prune, because the
	// source is scanned once and shared across all of them.
	Prune func(relDir string) bool
}

// DefaultScanWorkers is the middle of SPEC.md §6.1's suggested 8–16.
const DefaultScanWorkers = 12

// Scan walks root and returns everything beneath it.
//
// A failure to read one directory does not fail the scan: it is recorded in
// Errors and the rest of the tree is still walked, so the caller can report
// exactly what was unreadable. Cancellation, on the other hand, returns an
// error — a partial tree must never be mistaken for a complete one.
func (s *Scanner) Scan(ctx context.Context, root string) (*ScanResult, error) {
	workers := s.Workers
	if workers <= 0 {
		workers = DefaultScanWorkers
	}

	var (
		mu     sync.Mutex
		result = &ScanResult{Entries: map[string]Entry{}}

		// Progress callbacks get their own lock so that serialising them
		// does not contend with recording entries.
		progressMu sync.Mutex

		files, dirs, bytes atomic.Int64
		truncated          atomic.Bool
		// sem bounds concurrent readdir calls only. It is deliberately not
		// held while waiting for child directories: a parent holding a slot
		// while its children queue for one would deadlock.
		sem = make(chan struct{}, workers)
		// spawn bounds how many *goroutines* exist, separately from how
		// many reads are in flight. Without it a tree with 100k
		// directories would create 100k goroutines, nearly all parked.
		// Overflow is walked inline on the current goroutine instead, which
		// cannot deadlock because no semaphore slot is held across the call.
		spawn = make(chan struct{}, workers*4)
		wg    sync.WaitGroup
	)

	// walkAsync runs walk on a new goroutine when the budget allows, and
	// inline when it does not.
	var walk func(relDir string)
	walkAsync := func(relDir string) {
		select {
		case spawn <- struct{}{}:
			wg.Add(1)
			go func() {
				defer func() {
					<-spawn
					wg.Done()
				}()
				walk(relDir)
			}()
		default:
			walk(relDir)
		}
	}

	walk = func(relDir string) {
		if ctx.Err() != nil {
			return
		}

		sem <- struct{}{}
		entries, err := boundedReadDir(ctx, s.OpTimeout, filepath.Join(root, relDir))
		<-sem

		if err != nil {
			mu.Lock()
			result.Errors = append(result.Errors, ScanError{RelPath: relDir, Err: err})
			mu.Unlock()
			return
		}

		for _, de := range entries {
			if ctx.Err() != nil {
				return
			}
			if s.Limit > 0 && files.Load()+dirs.Load() >= int64(s.Limit) {
				truncated.Store(true)
				return
			}

			relPath := de.Name()
			if relDir != "" {
				relPath = relDir + "/" + de.Name()
			}

			// Symlinks are not synced in v1: their semantics over SMB are
			// messy and following one risks copying a tree twice. They are
			// still recorded as entries, because a symlink sitting at the
			// destination has to be visible to the differ — omitting it
			// would let a mkdir or a rename follow the link and write
			// outside the destination root entirely.
			if de.Type()&fs.ModeSymlink != 0 {
				mu.Lock()
				result.Symlinks = append(result.Symlinks, relPath)
				result.Entries[relPath] = Entry{RelPath: relPath, IsSymlink: true}
				mu.Unlock()
				continue
			}

			if de.IsDir() {
				if s.Prune != nil && s.Prune(relPath) {
					mu.Lock()
					result.Pruned = append(result.Pruned, relPath)
					mu.Unlock()
					continue
				}

				mu.Lock()
				result.Entries[relPath] = Entry{RelPath: relPath, IsDir: true}
				mu.Unlock()
				dirs.Add(1)

				walkAsync(relPath)
				continue
			}

			// Info() is a stat on most filesystems, and on SMB it is the
			// expensive part of a scan — which is what the worker pool is
			// for.
			info, err := boundedInfo(ctx, s.OpTimeout, de, filepath.Join(root, relPath))
			if err != nil {
				mu.Lock()
				if errors.Is(err, fs.ErrNotExist) {
					// Routine on a live tree: the file was deleted between
					// the listing and the stat. Not a hole in our knowledge
					// of the source, so it must not block deletions.
					result.Vanished = append(result.Vanished, relPath)
				} else {
					result.Errors = append(result.Errors, ScanError{RelPath: relPath, Err: err})
				}
				mu.Unlock()
				continue
			}

			entry := Entry{
				RelPath: relPath,
				Size:    info.Size(),
				ModTime: info.ModTime(),
			}
			mu.Lock()
			result.Entries[relPath] = entry
			mu.Unlock()

			files.Add(1)
			bytes.Add(entry.Size)
		}

		if s.OnProgress != nil {
			progressMu.Lock()
			s.OnProgress(files.Load(), dirs.Load(), bytes.Load())
			progressMu.Unlock()
		}
	}

	walkAsync("")
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s was cancelled: %w", root, err)
	}

	result.Truncated = truncated.Load()
	result.Files = files.Load()
	result.Dirs = dirs.Load()
	result.Bytes = bytes.Load()
	return result, nil
}

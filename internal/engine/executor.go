package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// Event is something worth recording in the run log.
type Event struct {
	Level   store.EventLevel
	RelPath string
	Message string
}

// EventSink receives events as they happen. It may be called from several
// worker goroutines at once and must be safe for concurrent use.
type EventSink func(Event)

// Failure is one action that did not succeed.
type Failure struct {
	RelPath string
	Kind    ActionKind
	Err     error
}

// ExecOptions are the per-job policies that govern execution.
type ExecOptions struct {
	Workers      int
	OnError      store.ErrorPolicy
	DeletePolicy store.DeletePolicy
	// LogEveryFile writes an event per copied file. Off by default: a
	// 100k-file run would otherwise write 100k rows (SPEC.md §6.5).
	LogEveryFile bool
}

// ExecResult summarises what an execution actually did.
type ExecResult struct {
	DirsCreated  int64
	FilesCopied  int64
	BytesCopied  int64
	FilesDeleted int64
	DirsRemoved  int64
	Skipped      int64

	Failures []Failure

	// DeletionsSkipped is set when removals were planned but withheld
	// because copies failed and the job's delete policy is skip_deletes.
	DeletionsSkipped bool
	// Aborted is set when the run stopped early, either through
	// on_error=abort or because the share went away.
	Aborted bool
	// ShareUnreachable is set when the destination stopped responding.
	ShareUnreachable bool
}

// Failed reports whether anything went wrong.
func (r *ExecResult) Failed() bool { return len(r.Failures) > 0 }

// Executor carries out a plan against one destination.
type Executor struct {
	Copier  *Copier
	Tracker *Tracker
	OnEvent EventSink

	// CheckDestination, if set, is called after a transport-level copy
	// failure to decide whether the share itself has gone away. A dead
	// share aborts the run immediately: retrying thousands of queued files
	// against a server that is not answering would take hours, and
	// SPEC.md §5 calls for aborting with a clear error instead.
	CheckDestination func(ctx context.Context) error

	// OpTimeout bounds each individual filesystem operation. Zero means
	// DefaultOpTimeout.
	OpTimeout time.Duration
}

// Execute runs the plan: directories first (top-down), then file copies in
// parallel, then removals bottom-up (SPEC.md §6.1 step 7).
func (e *Executor) Execute(ctx context.Context, srcRoot, dstRoot string, plan *Plan, opts ExecOptions) (*ExecResult, error) {
	if opts.Workers <= 0 {
		opts.Workers = store.DefaultWorkers
	}
	if e.Copier == nil {
		e.Copier = &Copier{}
	}

	result := &ExecResult{}
	unblocks, mkdirs, copies, deletes, rmdirs := partition(plan)

	// on_error=abort stops everything in flight at the first failure.
	execCtx, abort := context.WithCancel(ctx)
	defer abort()

	// Anything of the wrong type sitting on a path has to go first: every
	// mkdir and copy beneath it would otherwise fail, and the corrective
	// removal in the trailing delete pass would then be skipped because
	// those very failures tripped the delete guard — leaving the conflict
	// unresolved on every future run.
	if err := e.clearBlockers(execCtx, dstRoot, unblocks, result); err != nil {
		return result, err
	}

	if err := e.makeDirs(execCtx, dstRoot, mkdirs, result); err != nil {
		return result, err
	}

	if err := e.runCopies(execCtx, srcRoot, dstRoot, copies, opts, result, abort); err != nil {
		return result, err
	}

	// Deletions are the irreversible half of a mirror. If copies failed, the
	// destination is not in the state the plan assumed, so under the default
	// policy nothing is removed: extra files at the destination are fixed by
	// the next clean run, a wrong deletion is not.
	if result.Failed() && opts.DeletePolicy == store.DeletePolicySkipDeletes && (len(deletes) > 0 || len(rmdirs) > 0) {
		result.DeletionsSkipped = true
		e.emit(Event{
			Level: store.LevelWarn,
			Message: fmt.Sprintf(
				"%d file(s) failed to copy, so %d deletion(s) were skipped; set delete_policy to \"proceed\" to delete anyway",
				len(result.Failures), len(deletes)+len(rmdirs)),
		})
		return result, ctx.Err()
	}

	// Announce removals before making any of them. Deletions are the only
	// irreversible thing a run does, so the log says what is about to
	// happen and not merely what happened.
	if len(deletes) > 0 || len(rmdirs) > 0 {
		var bytes int64
		for _, a := range deletes {
			bytes += a.Size
		}
		e.emit(Event{Level: store.LevelWarn, Message: fmt.Sprintf(
			"about to delete %d file(s) totalling %d bytes and remove %d director(y/ies) from the destination; "+
				"each one is logged individually below",
			len(deletes), bytes, len(rmdirs))})
	}

	if err := e.runDeletes(execCtx, dstRoot, deletes, opts, result, abort); err != nil {
		return result, err
	}
	e.removeDirs(execCtx, dstRoot, rmdirs, result)

	e.summariseLockedFiles(result)

	return result, ctx.Err()
}

// summariseLockedFiles reports files skipped because another process held
// them open, once, at the end (SPEC.md §6.2).
//
// Each one is already logged individually where it happened. This exists
// because that is not enough on its own: a run of ten thousand files buries
// three locked ones, and the operator needs to know at a glance that the
// destination is not fully in sync and why. The count is derived from the
// failures already recorded rather than tracked separately, so it cannot drift
// from what the run actually reported.
func (e *Executor) summariseLockedFiles(result *ExecResult) {
	locked := make([]string, 0, 4)
	for _, f := range result.Failures {
		if errors.Is(f.Err, ErrDestinationLocked) {
			locked = append(locked, f.RelPath)
		}
	}
	if len(locked) == 0 {
		return
	}

	// Named, not just counted, up to a limit: "3 files were locked" without
	// saying which ones leaves the operator grepping. Past a handful the list
	// stops being readable and the individual entries above are the record.
	const named = 5
	shown := locked
	suffix := ""
	if len(shown) > named {
		shown = shown[:named]
		suffix = fmt.Sprintf(" (and %d more, listed individually above)", len(locked)-named)
	}
	e.emit(Event{Level: store.LevelWarn, Message: fmt.Sprintf(
		"%d file(s) were not copied because another process had the destination open: %s%s; "+
			"they are not retried, and the next run will pick them up",
		len(locked), strings.Join(shown, ", "), suffix)})
}

func (e *Executor) makeDirs(ctx context.Context, dstRoot string, actions []Action, result *ExecResult) error {
	if e.Tracker != nil && len(actions) > 0 {
		e.Tracker.SetPhase(PhaseCopying)
	}

	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 0755 rather than the source's mode: SMB uid/gid mapping makes
		// permission preservation largely meaningless (SPEC.md §13).
		if err := boundedMkdirAll(ctx, e.OpTimeout, filepath.Join(dstRoot, a.RelPath)); err != nil {
			result.Failures = append(result.Failures, Failure{RelPath: a.RelPath, Kind: ActionMkDir, Err: err})
			e.emit(Event{Level: store.LevelError, RelPath: a.RelPath,
				Message: fmt.Sprintf("could not create the directory: %v", err)})
			continue
		}
		result.DirsCreated++
	}
	return nil
}

// runCopies drives the worker pool. Copies are where the throughput is, so
// this is the only stage that runs wide.
func (e *Executor) runCopies(ctx context.Context, srcRoot, dstRoot string, actions []Action, opts ExecOptions, result *ExecResult, abort context.CancelFunc) error {
	if len(actions) == 0 {
		return nil
	}
	if e.Tracker != nil {
		e.Tracker.SetPhase(PhaseCopying)
	}

	var (
		mu          sync.Mutex
		bytesCopied atomic.Int64
		filesCopied atomic.Int64
		skipped     atomic.Int64
		work        = make(chan Action)
		wg          sync.WaitGroup
		abortedFlag atomic.Bool
		shareGone   atomic.Bool
		confirmMu   sync.Mutex
	)

	// Stop retrying as soon as the share is confirmed dead. Without this,
	// every queued file burns its whole retry budget against a server that
	// will not answer any of them, and the run never ends.
	if e.CheckDestination != nil {
		e.Copier.ShouldRetry = func(err error) bool {
			if !TransportError(err) {
				return true
			}
			if shareGone.Load() {
				return false
			}
			if e.confirmShareGone(ctx, &confirmMu) {
				shareGone.Store(true)
				return false
			}
			return true
		}
		defer func() { e.Copier.ShouldRetry = nil }()
	}

	worker := func() {
		defer wg.Done()
		for a := range work {
			if ctx.Err() != nil {
				return
			}

			src := filepath.Join(srcRoot, a.RelPath)
			dst := filepath.Join(dstRoot, a.RelPath)

			if e.Tracker != nil {
				e.Tracker.StartFile(a.RelPath, a.Size, nowFunc())
			}

			res, err := e.Copier.Copy(ctx, src, dst, a.ModTime, func(n int64) {
				bytesCopied.Add(n)
				if e.Tracker != nil {
					e.Tracker.AddFileBytes(a.RelPath, n)
				}
			})

			switch {
			case err == nil:
				filesCopied.Add(1)
				if e.Tracker != nil {
					e.Tracker.FinishFile(a.RelPath, true)
				}
				// An overwrite replaces data that was already there, so it
				// is always recorded. A brand-new file is only recorded when
				// the job asks: a 100k-file first run would otherwise write
				// 100k rows (SPEC.md §6.5).
				if a.Overwrite || opts.LogEveryFile {
					verb := "copied"
					if a.Overwrite {
						verb = "updated"
					}
					e.emit(Event{Level: store.LevelInfo, RelPath: a.RelPath,
						Message: fmt.Sprintf("%s, %d bytes (%s)", verb, res.BytesCopied, a.Reason)})
				}

			case errors.Is(err, ErrSourceVanished):
				// Expected on a live tree: not a failure, just a warning.
				skipped.Add(1)
				if e.Tracker != nil {
					e.Tracker.FinishFile(a.RelPath, true)
				}
				e.emit(Event{Level: store.LevelWarn, RelPath: a.RelPath,
					Message: "skipped: the file no longer exists at the source"})

			case errors.Is(err, context.Canceled):
				if e.Tracker != nil {
					e.Tracker.FinishFile(a.RelPath, false)
				}
				return

			default:
				if e.Tracker != nil {
					e.Tracker.FinishFile(a.RelPath, false)
				}
				mu.Lock()
				result.Failures = append(result.Failures, Failure{RelPath: a.RelPath, Kind: ActionCopy, Err: err})
				mu.Unlock()

				e.emit(Event{Level: store.LevelError, RelPath: a.RelPath,
					Message: fmt.Sprintf("copy failed: %v", err)})

				// A dead share is not a per-file problem: no policy makes
				// it sensible to keep going.
				if errors.Is(err, ErrShareUnreachable) {
					shareGone.Store(true)
					abortedFlag.Store(true)
					abort()
					return
				}
				if opts.OnError == store.ErrorPolicyAbort {
					abortedFlag.Store(true)
					abort()
					return
				}
			}
		}
	}

	for range opts.Workers {
		wg.Add(1)
		go worker()
	}

feed:
	for _, a := range actions {
		select {
		case work <- a:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()

	result.FilesCopied = filesCopied.Load()
	result.BytesCopied = bytesCopied.Load()
	result.Skipped = skipped.Load()
	result.Aborted = abortedFlag.Load()

	if result.Aborted {
		message := "aborting the run: this job is set to stop on the first error"
		if shareGone.Load() {
			result.ShareUnreachable = true
			message = "aborting the run: the destination stopped responding"
		}
		e.emit(Event{Level: store.LevelError, Message: message})
		// The caller distinguishes an abort from a cancellation by the flag,
		// so this is not an error return.
		return nil
	}
	return nil
}

// confirmShareGone probes the destination several times before concluding
// that it is dead.
//
// A single failed probe is not proof: a brief network hiccup would fail one,
// and aborting an hours-long sync over a blip is far worse than spending a
// few extra seconds making sure. The mutex keeps a pool of stuck workers
// from all probing at once.
func (e *Executor) confirmShareGone(ctx context.Context, mu *sync.Mutex) bool {
	const (
		attempts = 3
		gap      = 2 * time.Second
	)

	mu.Lock()
	defer mu.Unlock()

	for attempt := range attempts {
		if e.CheckDestination(ctx) == nil {
			return false
		}
		if attempt == attempts-1 {
			break
		}
		select {
		case <-time.After(gap):
		case <-ctx.Done():
			return true
		}
	}
	return true
}

func (e *Executor) runDeletes(ctx context.Context, dstRoot string, actions []Action, opts ExecOptions, result *ExecResult, abort context.CancelFunc) error {
	if len(actions) == 0 {
		return nil
	}
	if e.Tracker != nil {
		e.Tracker.SetPhase(PhaseDeleting)
	}

	var (
		mu          sync.Mutex
		deleted     atomic.Int64
		work        = make(chan Action)
		wg          sync.WaitGroup
		abortedFlag atomic.Bool
	)

	worker := func() {
		defer wg.Done()
		for a := range work {
			if ctx.Err() != nil {
				return
			}
			err := boundedRemove(ctx, e.OpTimeout, filepath.Join(dstRoot, a.RelPath))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				mu.Lock()
				result.Failures = append(result.Failures, Failure{RelPath: a.RelPath, Kind: ActionDelete, Err: err})
				mu.Unlock()
				e.emit(Event{Level: store.LevelError, RelPath: a.RelPath,
					Message: fmt.Sprintf("could not delete: %v", err)})
				if opts.OnError == store.ErrorPolicyAbort {
					abortedFlag.Store(true)
					abort()
					return
				}
				continue
			}
			deleted.Add(1)
			if e.Tracker != nil {
				e.Tracker.AddDeleted(1)
			}
			// Always logged, regardless of the job's setting: a deletion is
			// the one action that cannot be undone by re-running, so it must
			// never be summarised away.
			e.emit(Event{Level: store.LevelInfo, RelPath: a.RelPath,
				Message: fmt.Sprintf("deleted %d bytes: %s", a.Size, a.Reason)})
		}
	}

	for range opts.Workers {
		wg.Add(1)
		go worker()
	}

feed:
	for _, a := range actions {
		select {
		case work <- a:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()

	result.FilesDeleted = deleted.Load()
	if abortedFlag.Load() {
		result.Aborted = true
	}
	return nil
}

// removeDirs runs sequentially and bottom-up: a directory can only go once
// its contents have.
func (e *Executor) removeDirs(ctx context.Context, dstRoot string, actions []Action, result *ExecResult) {
	for _, a := range actions {
		if ctx.Err() != nil {
			return
		}
		err := boundedRemove(ctx, e.OpTimeout, filepath.Join(dstRoot, a.RelPath))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// A non-empty directory here usually means something inside it
			// failed to delete, which is already recorded. Warn rather than
			// fail again.
			e.emit(Event{Level: store.LevelWarn, RelPath: a.RelPath,
				Message: fmt.Sprintf("could not remove the directory: %v", err)})
			continue
		}
		result.DirsRemoved++
		e.emit(Event{Level: store.LevelInfo, RelPath: a.RelPath,
			Message: "removed the directory: " + a.Reason})
	}
}

func (e *Executor) emit(ev Event) {
	if e.OnEvent != nil {
		e.OnEvent(ev)
	}
}

// clearBlockers removes files and directories that stand where something of
// the other type belongs. It runs before any create or copy.
func (e *Executor) clearBlockers(ctx context.Context, dstRoot string, actions []Action, result *ExecResult) error {
	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := boundedRemove(ctx, e.OpTimeout, filepath.Join(dstRoot, a.RelPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			result.Failures = append(result.Failures, Failure{RelPath: a.RelPath, Kind: a.Kind, Err: err})
			e.emit(Event{Level: store.LevelError, RelPath: a.RelPath,
				Message: fmt.Sprintf("could not clear the path (%s): %v", a.Reason, err)})
			continue
		}
		if a.Kind == ActionRmDir {
			result.DirsRemoved++
		} else {
			result.FilesDeleted++
		}
		e.emit(Event{Level: store.LevelWarn, RelPath: a.RelPath,
			Message: "cleared the path at the destination: " + a.Reason})
	}
	return nil
}

// partition splits a plan into its stages. Diff already ordered each group.
func partition(plan *Plan) (unblocks, mkdirs, copies, deletes, rmdirs []Action) {
	for _, a := range plan.Actions {
		if a.Unblock {
			unblocks = append(unblocks, a)
			continue
		}
		switch a.Kind {
		case ActionMkDir:
			mkdirs = append(mkdirs, a)
		case ActionCopy:
			copies = append(copies, a)
		case ActionDelete:
			deletes = append(deletes, a)
		case ActionRmDir:
			rmdirs = append(rmdirs, a)
		}
	}
	return unblocks, mkdirs, copies, deletes, rmdirs
}

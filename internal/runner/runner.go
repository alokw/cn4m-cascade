// Package runner orchestrates a sync run: it resolves storage, scans, diffs,
// executes, and records everything to the database.
//
// It sits above internal/engine so the engine itself stays protocol-agnostic
// and free of any knowledge of targets, mounts or persistence (SPEC.md §4).
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/storage"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// Errors callers map onto HTTP status codes.
var (
	ErrAlreadyRunning = errors.New("a run of this job is already in progress")
	ErrNotRunning     = errors.New("that run is not in progress")
)

// progressInterval is how often in-memory progress is flushed to the
// database and buffered events are written.
const progressInterval = time.Second

// Runner starts and tracks runs.
type Runner struct {
	db       *store.DB
	provider *storage.Provider
	log      *slog.Logger

	mu     sync.Mutex
	active map[string]*handle // by run ID
	byJob  map[string]string  // job ID -> run ID, enforcing one run per job
	wg     sync.WaitGroup
}

type handle struct {
	runID   string
	jobID   string
	cancel  context.CancelFunc
	tracker *engine.Tracker
	// cancelled distinguishes a user cancellation from a failure.
	cancelled bool
}

// New builds a Runner.
func New(db *store.DB, provider *storage.Provider, log *slog.Logger) *Runner {
	return &Runner{
		db:       db,
		provider: provider,
		log:      log,
		active:   map[string]*handle{},
		byJob:    map[string]string{},
	}
}

// Start begins a run in the background and returns its record immediately.
//
// The run does not inherit the caller's context: an HTTP request finishing
// must not cancel a sync that may take hours.
func (r *Runner) Start(ctx context.Context, job *store.Job) (*store.Run, error) {
	if len(job.Destinations) == 0 {
		return nil, errors.New("this job has no destination")
	}

	srcTarget, err := r.db.GetTarget(ctx, job.SourceTargetID)
	if err != nil {
		return nil, fmt.Errorf("loading the source target of job %q: %w", job.Name, err)
	}
	dest := job.Destinations[0]
	dstTarget, err := r.db.GetTarget(ctx, dest.DestTargetID)
	if err != nil {
		return nil, fmt.Errorf("loading the destination target of job %q: %w", job.Name, err)
	}

	r.mu.Lock()
	if _, running := r.byJob[job.ID]; running {
		r.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	// Reserve the slot before the run row exists, so two concurrent triggers
	// cannot both get past this check.
	r.byJob[job.ID] = ""
	r.mu.Unlock()

	run, err := r.db.CreateRun(ctx, job.ID, store.TriggerManual, []string{dest.DestTargetID})
	if err != nil {
		r.mu.Lock()
		delete(r.byJob, job.ID)
		r.mu.Unlock()
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := &handle{
		runID:   run.ID,
		jobID:   job.ID,
		cancel:  cancel,
		tracker: engine.NewTracker(time.Now()),
	}

	// The wait group is incremented before the handle is published, so
	// Shutdown can never observe an empty group while a run is still being
	// set up and exit with a row stuck in "running".
	r.wg.Add(1)

	r.mu.Lock()
	r.active[run.ID] = h
	r.byJob[job.ID] = run.ID
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer cancel()
		r.execute(runCtx, h, job, run, srcTarget, dstTarget, dest)
	}()

	return run, nil
}

// Cancel stops a run in progress.
func (r *Runner) Cancel(runID string) error {
	r.mu.Lock()
	h, ok := r.active[runID]
	if ok {
		h.cancelled = true
	}
	r.mu.Unlock()

	if !ok {
		return ErrNotRunning
	}
	h.cancel()
	return nil
}

// Progress returns the live snapshot of a run, if it is still in progress.
func (r *Runner) Progress(runID string) (engine.Snapshot, bool) {
	r.mu.Lock()
	h, ok := r.active[runID]
	r.mu.Unlock()

	if !ok {
		return engine.Snapshot{}, false
	}
	return h.tracker.Snapshot(time.Now()), true
}

// Shutdown cancels every run in progress and waits for them to record their
// final state (SPEC.md §10: running jobs are marked cancelled).
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	for _, h := range r.active {
		h.cancelled = true
		h.cancel()
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("runs did not finish shutting down in time: %w", ctx.Err())
	}
}

// execute is the whole pipeline for one run (SPEC.md §6.1).
func (r *Runner) execute(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcTarget, dstTarget *store.Target, dest store.JobDestination) {
	log := r.log.With("run_id", run.ID, "job", job.Name)

	// Finalisation must survive cancellation: a cancelled run still has to
	// record that it was cancelled.
	finalCtx := context.WithoutCancel(ctx)
	events := newEventBuffer(finalCtx, r.db, run.ID, dest.DestTargetID, log)
	defer func() {
		events.Flush(finalCtx)
		r.finish(run.ID, job.ID)
	}()

	events.Add(engine.Event{Level: store.LevelInfo,
		Message: fmt.Sprintf("run started: %s → %s (%s)", srcTarget.Describe(), dstTarget.Describe(), job.Mode)})

	// 1. Resolve both sides.
	srcRoot, _, releaseSrc, err := r.resolve(ctx, srcTarget, job.SourceSubpath)
	if err != nil {
		r.fail(finalCtx, run, dest, events, fmt.Sprintf("source unavailable: %v", err))
		return
	}
	defer releaseSrc()

	dstRoot, dstStorage, releaseDst, err := r.resolve(ctx, dstTarget, dest.DestSubpath)
	if err != nil {
		// The availability gate with its skip/abort policies is Phase 3; for
		// now an unreachable destination simply fails the run.
		r.fail(finalCtx, run, dest, events, fmt.Sprintf("destination unavailable: %v", err))
		return
	}
	defer releaseDst()

	// 2. Scan both trees concurrently.
	h.tracker.SetPhase(engine.PhaseScanning)
	srcScan, dstScan, err := r.scanBoth(ctx, h, srcRoot, dstRoot)
	if err != nil {
		if ctx.Err() != nil {
			r.cancelled(finalCtx, run, dest, events)
			return
		}
		r.fail(finalCtx, run, dest, events, fmt.Sprintf("scan failed: %v", err))
		return
	}

	events.Add(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
		"scan complete: source %d files / %d dirs / %d bytes, destination %d files / %d dirs",
		srcScan.Files, srcScan.Dirs, srcScan.Bytes, dstScan.Files, dstScan.Dirs)})

	for _, link := range srcScan.Symlinks {
		events.Add(engine.Event{Level: store.LevelWarn, RelPath: link,
			Message: "skipped a symlink at the source: symlinks are not synced in this version"})
	}
	for _, link := range dstScan.Symlinks {
		events.Add(engine.Event{Level: store.LevelWarn, RelPath: link,
			Message: "found a symlink at the destination: nothing is written through it"})
	}
	for _, gone := range srcScan.Vanished {
		events.Add(engine.Event{Level: store.LevelWarn, RelPath: gone,
			Message: "the file disappeared from the source during the scan"})
	}
	for _, se := range srcScan.Errors {
		events.Add(engine.Event{Level: store.LevelError, RelPath: se.RelPath,
			Message: fmt.Sprintf("could not read at the source: %v", se.Err)})
	}
	for _, se := range dstScan.Errors {
		events.Add(engine.Event{Level: store.LevelError, RelPath: se.RelPath,
			Message: fmt.Sprintf("could not read at the destination: %v", se.Err)})
	}

	// 3. Diff.
	h.tracker.SetPhase(engine.PhasePlanning)
	plan := engine.Diff(srcScan, dstScan, engine.DiffOptions{
		Mode:          job.Mode,
		Tolerance:     job.Tolerance(),
		IgnoreDSTHour: job.IgnoreDSTHour,
		// SMB servers are usually case-insensitive (SPEC.md §6.5). Assuming
		// so is the safe direction: it only ever suppresses a deletion.
		CaseInsensitiveDest: dstTarget.Type == store.TargetSMB,
	})

	for _, c := range plan.Conflicts {
		events.Add(engine.Event{Level: store.LevelWarn, RelPath: c.RelPath, Message: c.Reason})
	}

	events.Add(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
		"plan: %d directories to create, %d files to copy (%d bytes), %d to delete, %d directories to remove",
		plan.MkDirs, plan.Copies, plan.CopyBytes, plan.Deletes, plan.RmDirs)})

	if plan.DeletionsBlocked {
		events.Add(engine.Event{Level: store.LevelError, Message: plan.BlockedReason})
	}

	if err := r.db.SetRunScanTotals(finalCtx, run.ID, srcScan.Files, plan.CopyBytes); err != nil {
		log.Error("could not record scan totals", "error", err)
	}
	if err := r.db.StartRunDestination(finalCtx, run.ID, dest.DestTargetID, int64(plan.Copies), plan.CopyBytes); err != nil {
		log.Error("could not start the destination record", "error", err)
	}
	h.tracker.SetTotals(int64(plan.Copies), plan.CopyBytes)

	// 4. Execute, flushing progress while it runs.
	stopFlusher := r.startFlusher(finalCtx, h, run.ID, dest.DestTargetID, events)

	exec := &engine.Executor{
		Copier:  &engine.Copier{},
		Tracker: h.tracker,
		OnEvent: events.Add,
		// SPEC.md §5: when the server goes offline mid-job the mount
		// manager marks the target unhealthy and the job aborts cleanly,
		// rather than grinding through every remaining file.
		CheckDestination: func(checkCtx context.Context) error {
			return dstStorage.Health(checkCtx)
		},
	}
	result, execErr := exec.Execute(ctx, srcRoot, dstRoot, plan, engine.ExecOptions{
		Workers:      job.Workers,
		OnError:      job.OnError,
		DeletePolicy: job.DeletePolicy,
		LogEveryFile: job.LogEveryFile,
	})

	stopFlusher()
	h.tracker.SetPhase(engine.PhaseDone)

	// 5. Finalise.
	snap := h.tracker.Snapshot(time.Now())
	if err := r.db.UpdateRunDestinationProgress(finalCtx, run.ID, dest.DestTargetID,
		snap.FilesDone, snap.FilesDeleted, snap.BytesDone); err != nil {
		log.Error("could not record final progress", "error", err)
	}

	if r.wasCancelled(run.ID) || errors.Is(execErr, context.Canceled) {
		r.cancelled(finalCtx, run, dest, events)
		return
	}

	status, destStatus, summary := classify(result, plan)
	events.Add(engine.Event{Level: levelFor(status), Message: fmt.Sprintf(
		"run %s: %d copied (%d bytes), %d deleted, %d skipped, %d failed",
		status, result.FilesCopied, result.BytesCopied, result.FilesDeleted,
		result.Skipped, len(result.Failures))})

	if err := r.db.FinishRunDestination(finalCtx, run.ID, dest.DestTargetID, destStatus, summary); err != nil {
		log.Error("could not finish the destination record", "error", err)
	}
	if err := r.db.FinishRun(finalCtx, run.ID, status, summary); err != nil {
		log.Error("could not finish the run record", "error", err)
	}
	log.Info("run finished", "status", status,
		"files_copied", result.FilesCopied, "bytes_copied", result.BytesCopied,
		"files_deleted", result.FilesDeleted, "failures", len(result.Failures))
}

// resolve mounts a target and returns its root plus a release function.
func (r *Runner) resolve(ctx context.Context, target *store.Target, subpath string) (string, storage.Storage, func(), error) {
	// The subpath is applied per job, so a copy of the target carries it
	// without mutating the stored record.
	scoped := *target
	scoped.Subpath = joinSubpaths(target.Subpath, subpath)

	st, err := r.provider.For(&scoped)
	if err != nil {
		return "", nil, nil, err
	}
	root, err := st.Resolve(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	return root, st, func() { _ = st.Release(context.WithoutCancel(ctx)) }, nil
}

// scanBoth walks source and destination at the same time; they are different
// servers, so there is no reason to wait for one before starting the other.
func (r *Runner) scanBoth(ctx context.Context, h *handle, srcRoot, dstRoot string) (*engine.ScanResult, *engine.ScanResult, error) {
	var (
		srcScan, dstScan *engine.ScanResult
		srcErr, dstErr   error
		wg               sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := &engine.Scanner{OnProgress: func(files, dirs, _ int64) {
			h.tracker.SetScanProgress(files, dirs)
		}}
		srcScan, srcErr = scanner.Scan(ctx, srcRoot)
	}()
	go func() {
		defer wg.Done()
		dstScan, dstErr = (&engine.Scanner{}).Scan(ctx, dstRoot)
	}()
	wg.Wait()

	if srcErr != nil {
		return nil, nil, fmt.Errorf("scanning the source: %w", srcErr)
	}
	if dstErr != nil {
		return nil, nil, fmt.Errorf("scanning the destination: %w", dstErr)
	}
	return srcScan, dstScan, nil
}

// startFlusher pushes progress and buffered events to the database at a
// steady cadence. The returned function stops it and waits.
func (r *Runner) startFlusher(ctx context.Context, h *handle, runID, destTargetID string, events *eventBuffer) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				now := time.Now()
				h.tracker.Sample(now)
				snap := h.tracker.Snapshot(now)

				if err := r.db.UpdateRunDestinationProgress(ctx, runID, destTargetID,
					snap.FilesDone, snap.FilesDeleted, snap.BytesDone); err != nil {
					r.log.Warn("could not flush progress", "run_id", runID, "error", err)
				}
				events.Flush(ctx)
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// classify turns an execution result into the run's final status.
func classify(result *engine.ExecResult, plan *engine.Plan) (store.RunStatus, store.DestStatus, string) {
	switch {
	case result.ShareUnreachable:
		return store.RunFailed, store.DestFailed,
			"the destination stopped responding mid-run; the run was aborted"

	case result.Aborted:
		return store.RunFailed, store.DestFailed,
			fmt.Sprintf("stopped after %d error(s); this job aborts on the first error", len(result.Failures))

	case plan.DeletionsBlocked && len(result.Failures) > 0:
		return store.RunPartial, store.DestFailed,
			plan.BlockedReason + "; also " + summarise(result.Failures)

	case plan.DeletionsBlocked:
		return store.RunPartial, store.DestSuccess, plan.BlockedReason

	case len(result.Failures) > 0:
		return store.RunPartial, store.DestFailed, summarise(result.Failures)

	case result.DeletionsSkipped:
		return store.RunPartial, store.DestSuccess, "deletions were skipped because copies failed"

	default:
		return store.RunSuccess, store.DestSuccess, ""
	}
}

// summarise builds a short error_summary from the first few failures.
func summarise(failures []engine.Failure) string {
	const show = 3

	parts := make([]string, 0, show+1)
	for i, f := range failures {
		if i == show {
			parts = append(parts, fmt.Sprintf("and %d more", len(failures)-show))
			break
		}
		parts = append(parts, fmt.Sprintf("%s: %v", f.RelPath, f.Err))
	}
	return fmt.Sprintf("%d file(s) failed — %s", len(failures), strings.Join(parts, "; "))
}

func levelFor(status store.RunStatus) store.EventLevel {
	switch status {
	case store.RunSuccess:
		return store.LevelInfo
	case store.RunPartial:
		return store.LevelWarn
	default:
		return store.LevelError
	}
}

func (r *Runner) fail(ctx context.Context, run *store.Run, dest store.JobDestination, events *eventBuffer, message string) {
	events.Add(engine.Event{Level: store.LevelError, Message: message})
	if err := r.db.FinishRunDestination(ctx, run.ID, dest.DestTargetID, store.DestFailed, message); err != nil {
		r.log.Error("could not finish the destination record", "run_id", run.ID, "error", err)
	}
	if err := r.db.FinishRun(ctx, run.ID, store.RunFailed, message); err != nil {
		r.log.Error("could not finish the run record", "run_id", run.ID, "error", err)
	}
}

func (r *Runner) cancelled(ctx context.Context, run *store.Run, dest store.JobDestination, events *eventBuffer) {
	const message = "the run was cancelled"

	events.Add(engine.Event{Level: store.LevelWarn, Message: message})
	if err := r.db.FinishRunDestination(ctx, run.ID, dest.DestTargetID, store.DestCancelled, message); err != nil {
		r.log.Error("could not finish the destination record", "run_id", run.ID, "error", err)
	}
	if err := r.db.FinishRun(ctx, run.ID, store.RunCancelled, message); err != nil {
		r.log.Error("could not finish the run record", "run_id", run.ID, "error", err)
	}
}

func (r *Runner) wasCancelled(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.active[runID]
	return ok && h.cancelled
}

func (r *Runner) finish(runID, jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, runID)
	delete(r.byJob, jobID)
}

func joinSubpaths(targetSubpath, jobSubpath string) string {
	switch {
	case targetSubpath == "":
		return jobSubpath
	case jobSubpath == "":
		return targetSubpath
	default:
		return targetSubpath + "/" + jobSubpath
	}
}

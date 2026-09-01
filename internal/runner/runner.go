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
	"github.com/alokw/cn4m-cascade/internal/filter"
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
	runID    string
	jobID    string
	cancel   context.CancelFunc
	progress *progress
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
	for _, d := range job.Destinations {
		if _, err := r.db.GetTarget(ctx, d.DestTargetID); err != nil {
			return nil, fmt.Errorf("loading a destination target of job %q: %w", job.Name, err)
		}
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

	run, err := r.db.CreateRun(ctx, job.ID, store.TriggerManual, destTargetIDs(job))
	if err != nil {
		r.mu.Lock()
		delete(r.byJob, job.ID)
		r.mu.Unlock()
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	destIDs := make([]string, 0, len(job.Destinations))
	for _, d := range job.Destinations {
		destIDs = append(destIDs, d.DestTargetID)
	}

	h := &handle{
		runID:    run.ID,
		jobID:    job.ID,
		cancel:   cancel,
		progress: newProgress(time.Now(), destIDs),
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
		r.execute(runCtx, h, job, run, srcTarget)
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
func (r *Runner) Progress(runID string) (RunSnapshot, bool) {
	r.mu.Lock()
	h, ok := r.active[runID]
	r.mu.Unlock()

	if !ok {
		return RunSnapshot{}, false
	}
	return h.progress.snapshot(time.Now()), true
}

// destTargetIDs lists a job's destinations in order.
func destTargetIDs(job *store.Job) []string {
	out := make([]string, 0, len(job.Destinations))
	for _, d := range job.Destinations {
		out = append(out, d.DestTargetID)
	}
	return out
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
func (r *Runner) execute(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcTarget *store.Target) {
	log := r.log.With("run_id", run.ID, "job", job.Name)

	// Finalisation must survive cancellation: a cancelled run still has to
	// record that it was cancelled.
	finalCtx := context.WithoutCancel(ctx)
	events := newEventBuffer(finalCtx, r.db, run.ID, log)

	defer func() {
		events.Flush(finalCtx)
		r.finish(run.ID, job.ID)
	}()

	events.Add(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
		"run started: %s → %d destination(s), mode %s", srcTarget.Describe(), len(job.Destinations), job.Mode)})

	// 1. Resolve the filter chains. Rule files are read now, not when the
	//    job was saved, because they are live configuration (SPEC.md §6.5).
	chains, pruneChain, err := r.resolveChains(ctx, job, events)
	if err != nil {
		r.failRun(finalCtx, job, run, events, fmt.Sprintf("filters could not be resolved: %v", err))
		return
	}

	// 2. Resolve the source.
	srcRoot, _, releaseSrc, err := r.resolve(ctx, srcTarget, job.SourceSubpath)
	if err != nil {
		r.failRun(finalCtx, job, run, events, fmt.Sprintf("source unavailable: %v", err))
		return
	}
	defer releaseSrc()

	// 3. Scan the source once and share it across destinations. Only
	//    job-scoped rules may prune it, because a per-target rule would
	//    corrupt the listing the other destinations see.
	h.progress.setPhase(engine.PhaseScanning)
	srcScan, err := r.scanSource(ctx, h, srcRoot, pruneChain)
	if err != nil {
		if ctx.Err() != nil {
			r.cancelRun(finalCtx, job, run, events)
			return
		}
		r.failRun(finalCtx, job, run, events, fmt.Sprintf("scanning the source failed: %v", err))
		return
	}

	events.Add(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
		"source scan complete: %d files / %d dirs / %d bytes%s",
		srcScan.Files, srcScan.Dirs, srcScan.Bytes, prunedSuffix(srcScan))})

	for _, link := range srcScan.Symlinks {
		events.Add(engine.Event{Level: store.LevelWarn, RelPath: link,
			Message: "skipped a symlink at the source: symlinks are not synced in this version"})
	}
	for _, gone := range srcScan.Vanished {
		events.Add(engine.Event{Level: store.LevelWarn, RelPath: gone,
			Message: "the file disappeared from the source during the scan"})
	}
	for _, se := range srcScan.Errors {
		events.Add(engine.Event{Level: store.LevelError, RelPath: se.RelPath,
			Message: fmt.Sprintf("could not read at the source: %v", se.Err)})
	}

	if err := r.db.SetRunScanTotals(finalCtx, run.ID, srcScan.Files, srcScan.Bytes); err != nil {
		log.Error("could not record scan totals", "error", err)
	}

	// 4. Fan out.
	stopFlusher := r.startFlusher(finalCtx, h, run.ID)
	outcomes := r.runDestinations(ctx, h, job, run, srcRoot, srcScan, chains, events, log)
	stopFlusher()

	h.progress.setPhase(engine.PhaseDone)
	r.flushProgress(finalCtx, h, run.ID)

	// 5. Finalise.
	if r.wasCancelled(run.ID) {
		r.cancelRun(finalCtx, job, run, events)
		return
	}

	status, summary := classifyRun(outcomes, job.UnavailablePolicy)
	events.Add(engine.Event{Level: levelFor(status), Message: fmt.Sprintf(
		"run %s across %d destination(s): %s", status, len(outcomes), summary)})

	if err := r.db.FinishRun(finalCtx, run.ID, status, summary); err != nil {
		log.Error("could not finish the run record", "error", err)
	}
	log.Info("run finished", "status", status, "destinations", len(outcomes))
}

// destOutcome is what happened to one destination.
type destOutcome struct {
	targetID string
	status   store.DestStatus
	summary  string
	// unavailable marks a destination that could not be resolved at all, as
	// opposed to one that failed while working.
	unavailable bool
	// degraded marks a destination that completed, but did less than the
	// job asked — deletions withheld, or a filter rule dropped. It reads as
	// success on its own row, so the run has to surface it.
	degraded bool
}

// runDestinations executes every destination, sequentially by default.
func (r *Runner) runDestinations(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcRoot string, srcScan *engine.ScanResult, chains map[string]*filter.Chain, events *eventBuffer, log *slog.Logger) []destOutcome {
	outcomes := make([]destOutcome, len(job.Destinations))

	work := func(i int, dest store.JobDestination) {
		outcomes[i] = r.runOneDestination(ctx, h, job, run, srcRoot, srcScan, chains[dest.DestTargetID], dest, events, log)
	}

	if !job.ParallelDestinations {
		for i, dest := range job.Destinations {
			// An abort policy stops the remaining destinations too.
			if i > 0 && job.UnavailablePolicy == store.PolicyAbort && unavailableEarlier(outcomes[:i]) {
				outcomes[i] = destOutcome{targetID: dest.DestTargetID, status: store.DestCancelled,
					summary: "not attempted: an earlier destination was unavailable and this job aborts"}
				continue
			}
			work(i, dest)
		}
		return outcomes
	}

	// Parallel fan-out multiplies read load on the source share, which is
	// why it is opt-in (SPEC.md §6.1 step 7).
	var wg sync.WaitGroup
	for i, dest := range job.Destinations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work(i, dest)
		}()
	}
	wg.Wait()
	return outcomes
}

// runOneDestination is resolve → availability gate → scan → diff → execute
// for a single destination.
func (r *Runner) runOneDestination(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcRoot string, srcScan *engine.ScanResult, chain *filter.Chain, dest store.JobDestination, events *eventBuffer, log *slog.Logger) destOutcome {
	finalCtx := context.WithoutCancel(ctx)
	destID := dest.DestTargetID
	sink := events.sinkFor(destID)
	tracker := h.progress.tracker(destID)

	outcome := destOutcome{targetID: destID}

	dstTarget, err := r.db.GetTarget(ctx, destID)
	if err != nil {
		return r.destFailed(finalCtx, run, destID, sink, h, fmt.Sprintf("the destination target could not be loaded: %v", err))
	}

	// Availability gate (SPEC.md §6.1 step 2). The interactive prompt lands
	// with the UI in Phase 4; here the policy is skip or abort.
	dstRoot, dstStorage, releaseDst, err := r.resolve(ctx, dstTarget, dest.DestSubpath)
	if err != nil {
		message := fmt.Sprintf("destination %s is unavailable: %v", dstTarget.Describe(), err)

		if job.UnavailablePolicy == store.PolicyAbort {
			sink(engine.Event{Level: store.LevelError, Message: message + " — this job aborts on an unavailable destination"})
			failed := r.destFailed(finalCtx, run, destID, sink, h, message)
			failed.unavailable = true
			// Stop anything already running for the other destinations too:
			// abort means the run fails immediately, not after the rest
			// finish.
			h.cancel()
			return failed
		}

		sink(engine.Event{Level: store.LevelWarn, Message: message + " — skipping it and continuing"})
		h.progress.setStatus(destID, store.DestSkippedUnavailable)
		if err := r.db.FinishRunDestination(finalCtx, run.ID, destID, store.DestSkippedUnavailable, message); err != nil {
			log.Error("could not record a skipped destination", "dest_target_id", destID, "error", err)
		}
		outcome.status, outcome.summary, outcome.unavailable = store.DestSkippedUnavailable, message, true
		return outcome
	}
	defer releaseDst()

	h.progress.setStatus(destID, store.DestRunning)

	// The destination is scanned with the same chain, so an excluded path is
	// invisible on both sides and can never be mistaken for extraneous.
	dstScan, err := (&engine.Scanner{
		Prune: func(relDir string) bool { return chain.PrunesDir(relDir) },
	}).Scan(ctx, dstRoot)
	if err != nil {
		if ctx.Err() != nil {
			h.progress.setStatus(destID, store.DestCancelled)
			outcome.status = store.DestCancelled
			return outcome
		}
		return r.destFailed(finalCtx, run, destID, sink, h, fmt.Sprintf("scanning the destination failed: %v", err))
	}
	for _, link := range dstScan.Symlinks {
		sink(engine.Event{Level: store.LevelWarn, RelPath: link,
			Message: "found a symlink at the destination: nothing is written through it"})
	}

	plan := engine.Diff(srcScan, dstScan, engine.DiffOptions{
		Mode:          job.Mode,
		Tolerance:     job.Tolerance(),
		IgnoreDSTHour: job.IgnoreDSTHour,
		// SMB servers are usually case-insensitive (SPEC.md §6.5). Assuming
		// so is the safe direction: it only ever suppresses a deletion.
		CaseInsensitiveDest: dstTarget.Type == store.TargetSMB,
		Filter:              chain,
	})

	for _, c := range plan.Conflicts {
		sink(engine.Event{Level: store.LevelWarn, RelPath: c.RelPath, Message: c.Reason})
	}
	for _, counts := range chain.Counts() {
		sink(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
			"filter %q: admitted %d, excluded %d", counts.Description, counts.Admitted, counts.Excluded)})
	}
	sink(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
		"plan: %d directories to create, %d files to copy (%d bytes), %d to delete, %d directories to remove",
		plan.MkDirs, plan.Copies, plan.CopyBytes, plan.Deletes, plan.RmDirs)})
	if plan.DeletionsBlocked {
		sink(engine.Event{Level: store.LevelError, Message: plan.BlockedReason})
	}

	if err := r.db.StartRunDestination(finalCtx, run.ID, destID, int64(plan.Copies), plan.CopyBytes); err != nil {
		log.Error("could not start the destination record", "dest_target_id", destID, "error", err)
	}
	tracker.SetTotals(int64(plan.Copies), plan.CopyBytes)
	h.progress.markPlanned(destID)

	exec := &engine.Executor{
		Copier:  &engine.Copier{},
		Tracker: tracker,
		OnEvent: sink,
		// SPEC.md §5: when the server goes offline mid-job the run aborts
		// cleanly rather than grinding through every remaining file.
		CheckDestination: func(checkCtx context.Context) error { return dstStorage.Health(checkCtx) },
	}
	result, execErr := exec.Execute(ctx, srcRoot, dstRoot, plan, engine.ExecOptions{
		Workers:      job.Workers,
		OnError:      job.OnError,
		DeletePolicy: job.DeletePolicy,
		LogEveryFile: job.LogEveryFile,
	})

	if r.wasCancelled(run.ID) || errors.Is(execErr, context.Canceled) {
		h.progress.setStatus(destID, store.DestCancelled)
		if err := r.db.FinishRunDestination(finalCtx, run.ID, destID, store.DestCancelled, "the run was cancelled"); err != nil {
			log.Error("could not record a cancelled destination", "dest_target_id", destID, "error", err)
		}
		outcome.status = store.DestCancelled
		return outcome
	}

	status, summary, degraded := classifyDestination(result, plan)
	sink(engine.Event{Level: levelForDest(status), Message: fmt.Sprintf(
		"destination %s: %d copied (%d bytes), %d deleted, %d skipped, %d failed",
		status, result.FilesCopied, result.BytesCopied, result.FilesDeleted,
		result.Skipped, len(result.Failures))})

	h.progress.setStatus(destID, status)
	if err := r.db.FinishRunDestination(finalCtx, run.ID, destID, status, summary); err != nil {
		log.Error("could not finish the destination record", "dest_target_id", destID, "error", err)
	}

	outcome.status, outcome.summary, outcome.degraded = status, summary, degraded
	return outcome
}

// scanSource walks the source once, pruned only by job-scoped rules.
func (r *Runner) scanSource(ctx context.Context, h *handle, root string, pruneChain *filter.Chain) (*engine.ScanResult, error) {
	scanner := &engine.Scanner{
		OnProgress: func(files, dirs, _ int64) { h.progress.setScanProgress(files, dirs) },
		Prune:      func(relDir string) bool { return pruneChain.PrunesDir(relDir) },
	}
	return scanner.Scan(ctx, root)
}

func (r *Runner) destFailed(ctx context.Context, run *store.Run, destID string, sink engine.EventSink, h *handle, message string) destOutcome {
	sink(engine.Event{Level: store.LevelError, Message: message})
	h.progress.setStatus(destID, store.DestFailed)
	if err := r.db.FinishRunDestination(ctx, run.ID, destID, store.DestFailed, message); err != nil {
		r.log.Error("could not record a failed destination", "run_id", run.ID, "dest_target_id", destID, "error", err)
	}
	return destOutcome{targetID: destID, status: store.DestFailed, summary: message}
}

// unavailableEarlier reports whether a previous destination could not be
// reached. SPEC.md §6.1 step 2 scopes the abort policy to *unavailability*;
// a destination that resolved fine and merely had a file fail to copy is
// governed by the job's on_error setting instead.
func unavailableEarlier(outcomes []destOutcome) bool {
	for _, o := range outcomes {
		if o.unavailable {
			return true
		}
	}
	return false
}

func prunedSuffix(scan *engine.ScanResult) string {
	if len(scan.Pruned) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d director(y/ies) pruned by filters", len(scan.Pruned))
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

// startFlusher pushes progress and buffered events to the database at a
// steady cadence. The returned function stops it and waits.
func (r *Runner) startFlusher(ctx context.Context, h *handle, runID string) func() {
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
				h.progress.sample(time.Now())
				r.flushProgress(ctx, h, runID)
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// flushProgress writes each destination's in-memory counters to its row.
func (r *Runner) flushProgress(ctx context.Context, h *handle, runID string) {
	snap := h.progress.snapshot(time.Now())
	for _, dest := range snap.Destinations {
		if dest.Status == store.DestPending || dest.Status == store.DestSkippedUnavailable {
			continue
		}
		if err := r.db.UpdateRunDestinationProgress(ctx, runID, dest.DestTargetID,
			dest.FilesDone, dest.FilesDeleted, dest.BytesDone); err != nil {
			r.log.Warn("could not flush progress", "run_id", runID,
				"dest_target_id", dest.DestTargetID, "error", err)
		}
	}
}

// classifyDestination turns one destination's execution into its status.
func classifyDestination(result *engine.ExecResult, plan *engine.Plan) (status store.DestStatus, summary string, degraded bool) {
	switch {
	case result.ShareUnreachable:
		return store.DestFailed, "the destination stopped responding mid-run; it was abandoned", false

	case result.Aborted:
		return store.DestFailed,
			fmt.Sprintf("stopped after %d error(s); this job aborts on the first error", len(result.Failures)), false

	case plan.DeletionsBlocked && len(result.Failures) > 0:
		return store.DestFailed, plan.BlockedReason + "; also " + summarise(result.Failures), true

	case plan.DeletionsBlocked:
		// Everything asked for was copied, but deletions were withheld. That
		// is not a plain success: the destination still holds files the job
		// intended to remove, and the user needs to know why.
		return store.DestSuccess, plan.BlockedReason, true

	case len(result.Failures) > 0:
		return store.DestFailed, summarise(result.Failures), false

	case result.DeletionsSkipped:
		return store.DestSuccess, "deletions were skipped because copies failed", true

	default:
		return store.DestSuccess, "", false
	}
}

// classifyRun folds the destination outcomes into the run's status.
//
// SPEC.md §6.1: a run completing with at least one skipped or failed
// destination is `partial`. A run where *nothing* succeeded is reported as
// failed instead — calling that partial would overstate it.
func classifyRun(outcomes []destOutcome, policy store.UnavailablePolicy) (store.RunStatus, string) {
	var succeeded, failed, skipped, cancelled, degraded int
	var problems []string
	aborted := false

	for _, o := range outcomes {
		if o.unavailable && policy == store.PolicyAbort {
			aborted = true
		}
		if o.degraded {
			degraded++
			problems = append(problems, fmt.Sprintf("%s: %s", o.targetID, o.summary))
		}
		switch o.status {
		case store.DestSuccess:
			succeeded++
		case store.DestSkippedUnavailable:
			skipped++
			problems = append(problems, fmt.Sprintf("%s skipped (unavailable)", o.targetID))
		case store.DestCancelled:
			cancelled++
		default:
			failed++
			problems = append(problems, fmt.Sprintf("%s failed: %s", o.targetID, o.summary))
		}
	}

	summary := fmt.Sprintf("%d succeeded, %d failed, %d skipped", succeeded, failed, skipped)
	if len(problems) > 0 {
		summary += " — " + strings.Join(problems, "; ")
	}

	switch {
	case aborted:
		// SPEC.md §6.1: abort fails the whole run, however far the other
		// destinations happened to get.
		return store.RunFailed, summary
	case cancelled > 0 && succeeded == 0 && failed == 0:
		return store.RunCancelled, summary
	case succeeded == 0:
		// A run where nothing succeeded is reported failed rather than
		// partial: §6.1's "≥1 skipped/failed ⇒ partial" reads oddly when
		// the answer is "none of it worked" (D-38).
		return store.RunFailed, summary
	case failed > 0 || skipped > 0 || cancelled > 0 || degraded > 0:
		// A destination that completed but withheld deletions reads as
		// success on its own row; the run is the only place that can say
		// the whole thing did less than it was asked to.
		return store.RunPartial, summary
	default:
		return store.RunSuccess, summary
	}
}

func levelForDest(status store.DestStatus) store.EventLevel {
	if status == store.DestSuccess {
		return store.LevelInfo
	}
	return store.LevelError
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

// failRun ends a run that could not get as far as its destinations.
func (r *Runner) failRun(ctx context.Context, job *store.Job, run *store.Run, events *eventBuffer, message string) {
	events.Add(engine.Event{Level: store.LevelError, Message: message})

	for _, d := range job.Destinations {
		if err := r.db.FinishRunDestination(ctx, run.ID, d.DestTargetID, store.DestFailed, message); err != nil {
			r.log.Error("could not finish a destination record", "run_id", run.ID, "error", err)
		}
	}
	if err := r.db.FinishRun(ctx, run.ID, store.RunFailed, message); err != nil {
		r.log.Error("could not finish the run record", "run_id", run.ID, "error", err)
	}
}

// cancelRun records a run stopped at the user's request.
func (r *Runner) cancelRun(ctx context.Context, job *store.Job, run *store.Run, events *eventBuffer) {
	const message = "the run was cancelled"

	events.Add(engine.Event{Level: store.LevelWarn, Message: message})
	for _, d := range job.Destinations {
		if err := r.db.FinishRunDestination(ctx, run.ID, d.DestTargetID, store.DestCancelled, message); err != nil {
			r.log.Error("could not finish a destination record", "run_id", run.ID, "error", err)
		}
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

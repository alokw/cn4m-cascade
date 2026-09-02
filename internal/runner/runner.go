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
	"sync/atomic"
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

	// preview means the run plans its work and waits to be confirmed
	// before touching anything (SPEC.md §6.1 step 6).
	preview bool
	// parked is true while the run is holding a plan awaiting confirmation.
	// Read without the Runner lock, hence atomic.
	parked atomic.Bool
	// gate holds whatever the run is waiting for a human to answer.
	gate *gate

	// planned is what planning produced, published so the API can show a
	// human the individual paths a run intends to remove before they confirm
	// it (CLAUDE.md: deletions are never summarised). Guarded by Runner.mu.
	//
	// Only the slice header needs the lock: each plannedDest.plan is
	// immutable once the diff returns, and the outcome field that execution
	// writes is not read here.
	planned []*plannedDest
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
	return r.start(ctx, job, false)
}

// StartPreview plans the run and parks it until confirmed, without copying,
// deleting or creating anything (SPEC.md §6.1 step 6).
func (r *Runner) StartPreview(ctx context.Context, job *store.Job) (*store.Run, error) {
	return r.start(ctx, job, true)
}

func (r *Runner) start(ctx context.Context, job *store.Job, preview bool) (*store.Run, error) {
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
		preview:  preview,
		gate:     newGate(),
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

// publishPlan makes one destination's plan readable from outside the run, so
// the API can show a human exactly what a preview intends to remove.
func (r *Runner) publishPlan(h *handle, p *plannedDest) {
	if p == nil || p.plan == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h.planned = append(h.planned, p)
}

// PlanFor returns the planned actions for a run still held in this process,
// per destination and in the differ's order. It reports false once the run has
// finished: nothing persists a plan, and a plan for a run that already executed
// would invite showing a user work that has already happened.
func (r *Runner) PlanFor(runID string) ([]DestActions, bool) {
	r.mu.Lock()
	h, ok := r.active[runID]
	var planned []*plannedDest
	if ok {
		planned = append(planned, h.planned...)
	}
	r.mu.Unlock()

	if !ok {
		return nil, false
	}

	out := make([]DestActions, 0, len(planned))
	for _, p := range planned {
		out = append(out, actionsOf(p.targetID, p.plan))
	}
	return out, true
}

// ActiveForJob reports whether a job currently holds a run in this process,
// whether that run is working or parked awaiting confirmation. It is keyed by
// job ID: Progress is keyed by run ID and silently never matches a job ID,
// which is what let an edit race a live run.
func (r *Runner) ActiveForJob(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.byJob[jobID]
	return ok
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

	// 4. Plan the destinations — and, unless this is a preview, execute each
	//    one as soon as it is planned.
	//
	//    The plan→park→execute split exists *for preview*: a preview has to
	//    plan everything before it can show a human what it intends to do. A
	//    normal run has no such need, and deferring execution until every
	//    destination is planned is actively harmful, because the availability
	//    gate lives in the planning pass. Planning all destinations first
	//    means a `prompt` on one unavailable destination parks *every* other
	//    destination behind it for up to prompt_timeout_sec — a healthy
	//    destination copying nothing for ten minutes because an unrelated
	//    server is down. Executing inline restores what Phase 3 did and what
	//    D-53 and §6.1 describe: the destination parks, not the run.
	stopFlusher := r.startFlusher(finalCtx, h, run.ID)
	var execInline func(*plannedDest)
	if !h.preview {
		execInline = func(p *plannedDest) { r.executeInto(ctx, h, job, run, srcRoot, p, log) }
	}
	planned := r.planDestinations(ctx, h, job, run, srcRoot, srcScan, chains, events, log, execInline)
	defer releaseAll(planned)

	// 5. If this is a preview, hold the plan until a human confirms it.
	if h.preview {
		confirmed := r.awaitConfirmation(ctx, h, job, run, events, log)
		if !confirmed {
			stopFlusher()
			h.progress.setPhase(engine.PhaseDone)
			r.flushProgress(finalCtx, h, run.ID)
			// A run the user cancelled reads as cancelled; one that simply
			// went unanswered says so. Both end up cancelled, but the
			// reason a user sees should be the true one.
			if r.wasCancelled(run.ID) {
				r.cancelRun(finalCtx, job, run, events)
				return
			}
			r.abandonPreview(finalCtx, job, run, planned, events, log)
			return
		}
	}

	// 6. Execute. A non-preview run already executed inline in step 4; only a
	//    confirmed preview still has work held here.
	if h.preview {
		r.executePlanned(ctx, h, job, run, srcRoot, planned, log)
	}
	outcomes := outcomesOf(planned)
	stopFlusher()

	h.progress.setPhase(engine.PhaseDone)
	r.flushProgress(finalCtx, h, run.ID)

	// 7. Finalise.
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

// plannedDest is one destination that has been resolved, scanned and diffed.
// It holds its mount reference until the plan is executed or abandoned, which
// is what lets a previewed run park between planning and doing.
type plannedDest struct {
	dest     store.JobDestination
	targetID string
	chain    *filter.Chain
	sink     engine.EventSink
	tracker  *engine.Tracker

	root       string
	dstStorage storage.Storage
	release    func()
	plan       *engine.Plan

	// outcome is set when planning short-circuited — unavailable, failed,
	// or cancelled — and there is nothing left to execute.
	outcome *destOutcome
}

// releaseAll drops every mount reference a planning pass acquired. Planning
// holds them so that a confirmed preview executes against the same mounts it
// planned against; every exit path has to give them back.
func releaseAll(planned []*plannedDest) {
	for _, p := range planned {
		if p != nil && p.release != nil {
			p.release()
		}
	}
}

// outcomesOf collects what planning and execution decided, in job order.
func outcomesOf(planned []*plannedDest) []destOutcome {
	out := make([]destOutcome, len(planned))
	for i, p := range planned {
		if p.outcome != nil {
			out[i] = *p.outcome
		}
	}
	return out
}

// planDestinations plans every destination — resolve, availability gate, scan,
// diff. Sequential by default; parallel fan-out is opt-in.
//
// When execInline is non-nil each destination is also executed as soon as it is
// planned — sequentially, before the next destination is planned; in parallel,
// within that destination's own goroutine. Either way no destination waits on
// another destination's availability gate. execInline is nil only for a
// preview, which must plan everything before a human can confirm any of it.
func (r *Runner) planDestinations(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcRoot string, srcScan *engine.ScanResult, chains map[string]*filter.Chain, events *eventBuffer, log *slog.Logger, execInline func(*plannedDest)) []*plannedDest {
	planned := make([]*plannedDest, len(job.Destinations))

	work := func(i int, dest store.JobDestination) {
		p := r.planOneDestination(ctx, h, job, run, srcRoot, srcScan, chains[dest.DestTargetID], dest, events, log)
		planned[i] = p
		// Published before execution, not after: a non-preview run executes
		// inline, so waiting for planDestinations to return would hide the
		// plan until the whole run had already finished.
		r.publishPlan(h, p)
		if execInline != nil {
			execInline(p)
		}
	}

	if !job.ParallelDestinations {
		for i, dest := range job.Destinations {
			// An abort policy stops the remaining destinations too.
			if i > 0 && job.UnavailablePolicy == store.PolicyAbort && unavailableEarlierPlanned(planned[:i]) {
				planned[i] = &plannedDest{
					dest: dest, targetID: dest.DestTargetID,
					outcome: &destOutcome{targetID: dest.DestTargetID, status: store.DestCancelled,
						summary: "not attempted: an earlier destination was unavailable and this job aborts"},
				}
				continue
			}
			work(i, dest)
		}
		return planned
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
	return planned
}

// planOneDestination is resolve → availability gate → scan → diff for a single
// destination. It writes nothing.
func (r *Runner) planOneDestination(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcRoot string, srcScan *engine.ScanResult, chain *filter.Chain, dest store.JobDestination, events *eventBuffer, log *slog.Logger) *plannedDest {
	finalCtx := context.WithoutCancel(ctx)
	destID := dest.DestTargetID
	sink := events.sinkFor(destID)

	p := &plannedDest{
		dest:     dest,
		targetID: destID,
		chain:    chain,
		sink:     sink,
		tracker:  h.progress.tracker(destID),
	}

	dstTarget, err := r.db.GetTarget(ctx, destID)
	if err != nil {
		failed := r.destFailed(finalCtx, run, destID, sink, h, fmt.Sprintf("the destination target could not be loaded: %v", err))
		p.outcome = &failed
		return p
	}

	// Availability gate (SPEC.md §6.1 step 2): skip, abort, or ask.
	root, dstStorage, release, gated := r.resolveDestination(ctx, h, job, run, dest, dstTarget, sink, log)
	if gated != nil {
		p.outcome = gated
		return p
	}
	p.root, p.dstStorage, p.release = root, dstStorage, release

	h.progress.setStatus(destID, store.DestRunning)

	// The destination is scanned with the same chain, so an excluded path is
	// invisible on both sides and can never be mistaken for extraneous.
	dstScan, err := (&engine.Scanner{
		Prune: func(relDir string) bool { return chain.PrunesDir(relDir) },
	}).Scan(ctx, root)
	if err != nil {
		if ctx.Err() != nil {
			h.progress.setStatus(destID, store.DestCancelled)
			p.outcome = &destOutcome{targetID: destID, status: store.DestCancelled}
			return p
		}
		failed := r.destFailed(finalCtx, run, destID, sink, h, fmt.Sprintf("scanning the destination failed: %v", err))
		p.outcome = &failed
		return p
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
	p.plan = plan

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

	h.progress.setPlan(destID, destPlanOf(destID, plan))
	p.tracker.SetTotals(int64(plan.Copies), plan.CopyBytes)
	h.progress.markPlanned(destID)
	return p
}

// destPlanOf projects an engine plan onto what the API reports.
func destPlanOf(destID string, plan *engine.Plan) DestPlan {
	dp := DestPlan{
		DestTargetID:     destID,
		MkDirs:           plan.MkDirs,
		Copies:           plan.Copies,
		Deletes:          plan.Deletes,
		RmDirs:           plan.RmDirs,
		CopyBytes:        plan.CopyBytes,
		Replaces:         plan.Unblocks,
		Overwrites:       plan.Overwrites,
		DeletionsBlocked: plan.DeletionsBlocked,
		BlockedReason:    plan.BlockedReason,
		WithheldDeletes:  plan.WithheldDeletes,
		WithheldRmDirs:   plan.WithheldRmDirs,
	}
	for _, c := range plan.Conflicts {
		dp.Conflicts = append(dp.Conflicts, c.RelPath+": "+c.Reason)
	}
	return dp
}

// resolveDestination applies the availability gate, looping while a human asks
// to retry. It returns a non-nil outcome when the destination will not be used.
func (r *Runner) resolveDestination(ctx context.Context, h *handle, job *store.Job, run *store.Run, dest store.JobDestination, dstTarget *store.Target, sink engine.EventSink, log *slog.Logger) (string, storage.Storage, func(), *destOutcome) {
	finalCtx := context.WithoutCancel(ctx)
	destID := dest.DestTargetID

	for {
		root, dstStorage, release, err := r.resolve(ctx, dstTarget, dest.DestSubpath)
		if err == nil {
			return root, dstStorage, release, nil
		}

		message := fmt.Sprintf("destination %s is unavailable: %v", dstTarget.Describe(), err)
		policy := job.UnavailablePolicy

		if policy == store.PolicyPrompt {
			action, answered := r.askAboutDestination(ctx, h, job, run, destID, message, sink, log)
			if ctx.Err() != nil {
				h.progress.setStatus(destID, store.DestCancelled)
				return "", nil, nil, &destOutcome{targetID: destID, status: store.DestCancelled}
			}
			switch action {
			case PromptRetry:
				sink(engine.Event{Level: store.LevelInfo, Message: "retrying the destination at your request"})
				continue
			case PromptAbort:
				policy = store.PolicyAbort
			default:
				policy = store.PolicySkip
			}
			if !answered {
				message += fmt.Sprintf(" — no answer within %ds, falling back to %s",
					job.PromptTimeoutSec, job.PromptFallback)
			}
		}

		if policy == store.PolicyAbort {
			sink(engine.Event{Level: store.LevelError, Message: message + " — this job aborts on an unavailable destination"})
			failed := r.destFailed(finalCtx, run, destID, sink, h, message)
			failed.unavailable = true
			// Stop anything already running for the other destinations too:
			// abort means the run fails immediately, not after the rest
			// finish.
			h.cancel()
			return "", nil, nil, &failed
		}

		sink(engine.Event{Level: store.LevelWarn, Message: message + " — skipping it and continuing"})
		h.progress.setStatus(destID, store.DestSkippedUnavailable)
		if err := r.db.FinishRunDestination(finalCtx, run.ID, destID, store.DestSkippedUnavailable, message); err != nil {
			log.Error("could not record a skipped destination", "dest_target_id", destID, "error", err)
		}
		return "", nil, nil, &destOutcome{
			targetID: destID, status: store.DestSkippedUnavailable,
			summary: message, unavailable: true,
		}
	}
}

// askAboutDestination parks one destination and waits for a human. The run
// stays running and its other destinations carry on: only the one that cannot
// be reached waits.
func (r *Runner) askAboutDestination(ctx context.Context, h *handle, job *store.Job, run *store.Run, destID, message string, sink engine.EventSink, log *slog.Logger) (PromptAction, bool) {
	finalCtx := context.WithoutCancel(ctx)
	timeout := time.Duration(job.PromptTimeoutSec) * time.Second
	deadline := time.Now().Add(timeout)

	ch := h.gate.open(destID)
	h.progress.setStatus(destID, store.DestAwaitingPrompt)
	h.progress.setPrompt(destID, deadline, message)
	if err := r.db.SetRunDestinationStatus(finalCtx, run.ID, destID, store.DestAwaitingPrompt); err != nil {
		log.Error("could not record a waiting destination", "dest_target_id", destID, "error", err)
	}
	sink(engine.Event{Level: store.LevelWarn, Message: fmt.Sprintf(
		"%s — waiting up to %ds for a decision; with no answer this destination will %s",
		message, job.PromptTimeoutSec, job.PromptFallback)})

	action, answered := waitForPrompt(ctx, ch, timeout, job.PromptFallback)

	h.gate.close(destID)
	h.progress.clearPrompt(destID)

	if answered {
		sink(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
			"answered %q for this destination", action)})
	} else if ctx.Err() == nil {
		sink(engine.Event{Level: store.LevelWarn, Message: fmt.Sprintf(
			"no answer after %ds; falling back to %s", job.PromptTimeoutSec, job.PromptFallback)})
	}
	return action, answered
}

// executeInto runs one planned destination and records its outcome on it.
// Planning may already have short-circuited the destination (unavailable,
// failed, cancelled), in which case there is nothing left to execute.
func (r *Runner) executeInto(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcRoot string, p *plannedDest, log *slog.Logger) {
	if p.outcome != nil {
		return
	}
	outcome := r.executeOneDestination(ctx, h, job, run, srcRoot, p, log)
	p.outcome = &outcome
}

// executePlanned carries out plans that were held rather than executed inline,
// which today means a confirmed preview.
func (r *Runner) executePlanned(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcRoot string, planned []*plannedDest, log *slog.Logger) []destOutcome {
	work := func(p *plannedDest) {
		r.executeInto(ctx, h, job, run, srcRoot, p, log)
	}

	if !job.ParallelDestinations {
		for _, p := range planned {
			work(p)
		}
		return outcomesOf(planned)
	}

	var wg sync.WaitGroup
	for _, p := range planned {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work(p)
		}()
	}
	wg.Wait()
	return outcomesOf(planned)
}

// executeOneDestination carries out one already-computed plan.
func (r *Runner) executeOneDestination(ctx context.Context, h *handle, job *store.Job, run *store.Run, srcRoot string, p *plannedDest, log *slog.Logger) destOutcome {
	finalCtx := context.WithoutCancel(ctx)
	destID := p.targetID
	outcome := destOutcome{targetID: destID}

	if err := r.db.StartRunDestination(finalCtx, run.ID, destID, int64(p.plan.Copies), p.plan.CopyBytes); err != nil {
		log.Error("could not start the destination record", "dest_target_id", destID, "error", err)
	}
	h.progress.setStatus(destID, store.DestRunning)

	exec := &engine.Executor{
		Copier:  &engine.Copier{},
		Tracker: p.tracker,
		OnEvent: p.sink,
		// SPEC.md §5: when the server goes offline mid-job the run aborts
		// cleanly rather than grinding through every remaining file.
		CheckDestination: func(checkCtx context.Context) error { return p.dstStorage.Health(checkCtx) },
	}
	result, execErr := exec.Execute(ctx, srcRoot, p.root, p.plan, engine.ExecOptions{
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

	status, summary, degraded := classifyDestination(result, p.plan)
	p.sink(engine.Event{Level: levelForDest(status), Message: fmt.Sprintf(
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

// unavailableEarlierPlanned reports whether an earlier destination could not
// be reached, so an aborting job stops planning the rest.
//
// SPEC.md §6.1 step 2 scopes the abort policy to *unavailability*: a
// destination that resolved fine and merely had a file fail to copy is
// governed by the job's on_error setting instead.
func unavailableEarlierPlanned(planned []*plannedDest) bool {
	for _, p := range planned {
		if p != nil && p.outcome != nil && p.outcome.unavailable {
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

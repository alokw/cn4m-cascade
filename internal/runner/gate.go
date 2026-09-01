package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// PromptAction is a human's answer to an unavailable destination
// (SPEC.md §8's POST /api/runs/{id}/prompt).
type PromptAction string

const (
	// PromptSkip carries on without this destination.
	PromptSkip PromptAction = "skip"
	// PromptRetry tries to reach it again.
	PromptRetry PromptAction = "retry"
	// PromptAbort ends the whole run.
	PromptAbort PromptAction = "abort"
)

// ValidPromptAction reports whether s names an action.
func ValidPromptAction(s PromptAction) bool {
	switch s {
	case PromptSkip, PromptRetry, PromptAbort:
		return true
	}
	return false
}

// Errors the API maps onto status codes.
var (
	ErrNoSuchPrompt       = errors.New("that destination is not waiting for an answer")
	ErrNotAwaitingConfirm = errors.New("that run is not waiting to be confirmed")
)

// gate holds whatever a run is currently waiting for a human to answer.
//
// It has its own mutex rather than sharing the Runner's: a prompt can be
// outstanding for minutes, and the Runner's lock is taken on every progress
// poll. Entangling the two would make an unanswered prompt slow down the API.
type gate struct {
	mu sync.Mutex
	// prompts is keyed by destination target ID. A buffered channel per
	// prompt means answering never blocks the API handler, even if the
	// waiting goroutine has already given up and moved on.
	prompts map[string]chan PromptAction
	// confirm is closed when a previewed run is confirmed.
	confirm chan struct{}
	// confirmed guards against closing confirm twice.
	confirmed bool
}

func newGate() *gate {
	return &gate{prompts: map[string]chan PromptAction{}, confirm: make(chan struct{})}
}

// open registers a prompt for a destination and returns the channel to wait on.
func (g *gate) open(destTargetID string) chan PromptAction {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan PromptAction, 1)
	g.prompts[destTargetID] = ch
	return ch
}

// close removes a prompt once it has been answered or has timed out.
func (g *gate) close(destTargetID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.prompts, destTargetID)
}

// answer delivers a human's decision. It reports ErrNoSuchPrompt when nothing
// is waiting, which is what the API returns for a stale modal.
func (g *gate) answer(destTargetID string, action PromptAction) error {
	g.mu.Lock()
	ch, ok := g.prompts[destTargetID]
	g.mu.Unlock()

	if !ok {
		return ErrNoSuchPrompt
	}
	select {
	case ch <- action:
		return nil
	default:
		// Already answered. Treat a double-click as the no-op it is
		// rather than an error the user has to understand.
		return nil
	}
}

// markConfirmed releases a parked preview. Confirming twice is a no-op.
func (g *gate) markConfirmed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.confirmed {
		g.confirmed = true
		close(g.confirm)
	}
}

// AnswerPrompt records a human's answer to an unavailable destination.
func (r *Runner) AnswerPrompt(runID, destTargetID string, action PromptAction) error {
	if !ValidPromptAction(action) {
		return fmt.Errorf("action must be %q, %q or %q, got %q",
			PromptSkip, PromptRetry, PromptAbort, action)
	}

	r.mu.Lock()
	h, ok := r.active[runID]
	r.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	return h.gate.answer(destTargetID, action)
}

// Confirm releases a previewed run so it executes the plan it is holding.
func (r *Runner) Confirm(runID string) error {
	r.mu.Lock()
	h, ok := r.active[runID]
	r.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	if !h.preview {
		return ErrNotAwaitingConfirm
	}
	h.gate.markConfirmed()
	return nil
}

// AwaitingConfirmation reports whether a run is parked on a preview. The API
// uses it to answer "confirm" against the right run without racing the runner.
func (r *Runner) AwaitingConfirmation(jobID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	runID, ok := r.byJob[jobID]
	if !ok || runID == "" {
		return "", false
	}
	h, ok := r.active[runID]
	if !ok || !h.preview {
		return "", false
	}
	return runID, h.parked.Load()
}

// waitForPrompt blocks until a human answers, the wait runs out, or the run is
// cancelled.
//
// The wait is always bounded. A run may be unattended — from Phase 5 it may be
// started by cron with nobody watching at all — so "wait for a human" can never
// mean "wait forever" (SPEC.md §9's countdown to a fallback action).
func waitForPrompt(ctx context.Context, ch chan PromptAction, timeout time.Duration, fallback store.PromptFallback) (action PromptAction, answered bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case a := <-ch:
		return a, true
	case <-timer.C:
		if fallback == store.FallbackAbort {
			return PromptAbort, false
		}
		return PromptSkip, false
	case <-ctx.Done():
		// A cancelled run does not act on the destination at all.
		return PromptSkip, false
	}
}

// awaitConfirmation parks a previewed run until a human confirms it, the wait
// runs out, or the run is cancelled. It reports whether to go ahead.
//
// The plan is held rather than recomputed on confirmation: the user agreed to
// a specific set of changes, and re-diffing could execute something they never
// saw. The cost is that the plan is a snapshot — the executor already treats a
// file that vanished since planning as a normal event, not an error.
func (r *Runner) awaitConfirmation(ctx context.Context, h *handle, job *store.Job, run *store.Run, events *eventBuffer, log *slog.Logger) bool {
	finalCtx := context.WithoutCancel(ctx)
	timeout := time.Duration(job.PromptTimeoutSec) * time.Second
	deadline := time.Now().Add(timeout)

	h.parked.Store(true)
	defer h.parked.Store(false)

	h.progress.setConfirmDeadline(deadline)
	if err := r.db.SetRunStatus(finalCtx, run.ID, store.RunAwaitingConfirmation); err != nil {
		log.Error("could not park the run awaiting confirmation", "error", err)
	}
	events.Add(engine.Event{Level: store.LevelInfo, Message: fmt.Sprintf(
		"preview ready: waiting up to %ds for confirmation; nothing has been changed",
		job.PromptTimeoutSec)})
	// Flush now so the plan and the parked status are visible to a poller
	// immediately rather than at the next tick.
	events.Flush(finalCtx)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-h.gate.confirm:
		if err := r.db.SetRunStatus(finalCtx, run.ID, store.RunRunning); err != nil {
			log.Error("could not un-park the confirmed run", "error", err)
		}
		events.Add(engine.Event{Level: store.LevelInfo, Message: "confirmed: executing the previewed plan"})
		return true
	case <-timer.C:
		events.Add(engine.Event{Level: store.LevelWarn, Message: fmt.Sprintf(
			"no confirmation within %ds: cancelling the run without changing anything",
			job.PromptTimeoutSec)})
		return false
	case <-ctx.Done():
		return false
	}
}

// abandonPreview closes out a preview that was never confirmed. It is
// cancelled rather than failed: nothing was attempted, so nothing failed.
func (r *Runner) abandonPreview(ctx context.Context, job *store.Job, run *store.Run, planned []*plannedDest, events *eventBuffer, log *slog.Logger) {
	const summary = "the previewed plan was not confirmed; nothing was changed"

	for _, p := range planned {
		if p.outcome != nil {
			continue
		}
		if err := r.db.FinishRunDestination(ctx, run.ID, p.targetID, store.DestCancelled, summary); err != nil {
			log.Error("could not close out a previewed destination", "dest_target_id", p.targetID, "error", err)
		}
	}
	events.Add(engine.Event{Level: store.LevelInfo, Message: summary})
	if err := r.db.FinishRun(ctx, run.ID, store.RunCancelled, summary); err != nil {
		log.Error("could not finish the abandoned preview", "error", err)
	}
	log.Info("preview abandoned", "job", job.Name)
}

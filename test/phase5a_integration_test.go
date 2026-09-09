//go:build integration

package test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// scheduledJob builds a real SMB mirror job carrying a cron expression.
func scheduledJob(t *testing.T, h *harness, cron string, enabled bool) (jobID, srcRoot string) {
	t.Helper()

	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"scheduled.txt": 64})

	payload := map[string]any{
		"name":             uniqueName("sched"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"schedule_cron":    cron,
		"enabled":          enabled,
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	}
	return h.createJob(t, payload), srcRoot
}

// backdate forces a job to be due by moving its next firing into the past.
func backdate(t *testing.T, h *harness, jobID string, at time.Time) {
	t.Helper()
	if err := h.db.SetNextRun(context.Background(), jobID, at); err != nil {
		t.Fatalf("backdating the next run: %v", err)
	}
}

// Phase 5a exit criterion: a scheduled job fires and records trigger=schedule.
//
// Driven with an explicit `now` rather than by waiting: the point being proved
// is that the scheduler starts a *real* run through the real runner against
// real shares, and that does not need a wall clock to be convincing. The
// waiting version lives in TestSchedulerFiresOnItsOwnLoop below.
func TestScheduledRunStartsAndIsMarkedAsScheduled(t *testing.T) {
	h := newHarness(t, nil)
	jobID, _ := scheduledJob(t, h, "0 2 * * *", true)

	now := time.Now().UTC()
	backdate(t, h, jobID, now.Add(-time.Minute))

	h.scheduler.Tick(context.Background(), now)

	runs := h.runsForJob(t, jobID)
	if len(runs) != 1 {
		t.Fatalf("the schedule produced %d runs, want 1", len(runs))
	}
	if trigger, _ := runs[0]["trigger"].(string); trigger != string(store.TriggerSchedule) {
		t.Fatalf("trigger = %q, want %q", trigger, store.TriggerSchedule)
	}

	runID, _ := runs[0]["id"].(string)
	run := h.awaitRun(t, runID, 90*time.Second)
	if status, _ := run["status"].(string); status != string(store.RunSuccess) {
		t.Fatalf("the scheduled run finished as %v, want success: %v", status, run["error_summary"])
	}

	// And the firing time moved on, so the next tick does not run it again.
	job, err := h.db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !job.NextRunAt.After(now) {
		t.Fatalf("next_run_at = %v is still due at %v; the job would refire every tick",
			job.NextRunAt, now)
	}
}

// Phase 5a exit criterion: a disabled job never fires.
//
// `enabled` is the control that pauses a backup without discarding its
// expression, so a paused job that still ran would make it a lie.
func TestDisabledJobNeverFires(t *testing.T) {
	h := newHarness(t, nil)
	jobID, _ := scheduledJob(t, h, "0 2 * * *", false)

	now := time.Now().UTC()
	backdate(t, h, jobID, now.Add(-time.Hour))

	h.scheduler.Tick(context.Background(), now)

	if runs := h.runsForJob(t, jobID); len(runs) != 0 {
		t.Fatalf("a disabled job started %d run(s)", len(runs))
	}
}

// Phase 5a exit criterion: an invalid expression is refused at save, with a
// message that says what to type instead.
func TestInvalidCronIsRefusedWithALegibleMessage(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, _ := previewFixture(t, h)

	for _, expr := range []string{"* * *", "99 2 * * *", "every night please", "0 0 2 * * *"} {
		t.Run(expr, func(t *testing.T) {
			status, body := h.do(http.MethodPost, "/api/jobs", map[string]any{
				"name":             uniqueName("badcron"),
				"source_target_id": srcID,
				"source_subpath":   scope,
				"mode":             string(store.ModeMirror),
				"schedule_cron":    expr,
				"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
			})
			if status != http.StatusBadRequest {
				t.Fatalf("saving %q = %d, want 400: %v", expr, status, body)
			}
			msg := errorMessage(body)
			// Legible means it names what was typed and how the fields are
			// ordered — a bare parser error does neither (CLAUDE.md).
			if !strings.Contains(msg, "minute hour day-of-month") {
				t.Fatalf("the error does not explain the field order: %q", msg)
			}
		})
	}
}

// Phase 5a exit criterion: a fire while the previous run is still going is
// skipped rather than overlapped — and the job is still rescheduled, or it
// would be retried every tick for the whole duration of the run it collided
// with.
func TestOverlappingScheduledRunIsSkipped(t *testing.T) {
	h := newHarness(t, nil)
	jobID, srcRoot := scheduledJob(t, h, "* * * * *", true)

	// Enough files that the first run is still going when the second fires.
	seedManyFiles(t, srcRoot, 3000)

	first := h.runJob(t, jobID)
	h.awaitCopying(t, first, 60*time.Second)

	now := time.Now().UTC()
	backdate(t, h, jobID, now.Add(-time.Minute))
	h.scheduler.Tick(context.Background(), now)

	runs := h.runsForJob(t, jobID)
	if len(runs) != 1 {
		t.Fatalf("a scheduled fire during a running job produced %d runs, want the original 1", len(runs))
	}

	job, err := h.db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !job.NextRunAt.After(now) {
		t.Fatalf("a skipped fire left next_run_at = %v, still due at %v", job.NextRunAt, now)
	}

	h.awaitRun(t, first, 3*time.Minute)
}

// Phase 5a exit criterion: a firing missed while the server was down does not
// run late on startup.
//
// Several missed jobs all starting at boot — at an hour nobody chose, possibly
// while someone is mid-restore — is worse than skipping a night. Skipping
// silently would be worse than either, so the miss is logged.
func TestMissedScheduleDoesNotFireAtStartup(t *testing.T) {
	h := newHarness(t, nil)
	jobID, _ := scheduledJob(t, h, "0 2 * * *", true)

	backdate(t, h, jobID, time.Now().UTC().Add(-8*time.Hour))

	// Start drives the startup catch-up pass, then ticks. Shutdown waits for
	// it, so there is no sleep here.
	ctx, cancel := context.WithCancel(context.Background())
	h.scheduler.Start(ctx)
	shutdownCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	if err := h.scheduler.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("scheduler shutdown: %v", err)
	}
	cancel()

	if runs := h.runsForJob(t, jobID); len(runs) != 0 {
		t.Fatalf("startup started %d missed run(s); they must not fire late", len(runs))
	}

	job, err := h.db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !job.NextRunAt.After(time.Now().UTC()) {
		t.Fatalf("a missed firing left next_run_at = %v, which is still in the past", job.NextRunAt)
	}
	if !strings.Contains(h.logs.String(), "was missed while the server was not running") {
		t.Fatal("a missed schedule was skipped without saying so; a silent skip is indistinguishable from a job that never ran")
	}
}

// The scheduler fires on its own loop, with nobody calling Tick.
//
// This one really does wait, and it is the only one that does. Every test
// above drives Tick directly, which proves the decision logic but would pass
// just as happily if the loop were never started — so one honest end-to-end
// case earns its minute.
func TestSchedulerFiresOnItsOwnLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the scheduler's own tick")
	}

	h := newHarness(t, nil)
	jobID, _ := scheduledJob(t, h, "* * * * *", true)

	// Due now, so the next tick picks it up.
	backdate(t, h, jobID, time.Now().UTC().Add(-time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.scheduler.Start(ctx)

	// Startup's catch-up pass deliberately does not fire it, so the run must
	// come from a real tick: one TickInterval, plus room for the run to start.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if runs := h.runsForJob(t, jobID); len(runs) > 0 {
			runID, _ := runs[0]["id"].(string)
			run := h.awaitRun(t, runID, 90*time.Second)
			if status, _ := run["status"].(string); status != string(store.RunSuccess) {
				t.Fatalf("the run the loop started finished as %v", status)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("the scheduler's own loop never started a run")
}

// runsForJob lists a job's runs, newest first.
func (h *harness) runsForJob(t *testing.T, jobID string) []map[string]any {
	t.Helper()

	status, body := h.do(http.MethodGet, "/api/runs?limit=50&job_id="+jobID, nil)
	if status != http.StatusOK {
		t.Fatalf("listing runs = %d: %v", status, body)
	}
	raw, _ := body["runs"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// errorMessage digs the human-facing message out of the error envelope.
func errorMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if detail, _ := e["detail"].(string); detail != "" {
		return fmt.Sprintf("%s %s", msg, detail)
	}
	return msg
}

// A scheduled run must carry the job's own filter rules.
//
// This is the most dangerous thing that can go wrong in 5a, and it is invisible
// from every other test. `DueJobs` builds the *store.Job the scheduler hands to
// the runner; if it loads destinations but not filter rules, the run proceeds
// with an empty rule set. That is not merely "syncs too much" — for a mirror it
// is a deletion. An excluded path at the destination has no source counterpart,
// so with the exclusion gone the differ classifies it as extraneous and removes
// it (internal/engine/differ.go says exactly this: "a dropped rule widens
// scope, which in mirror mode turns previously excluded destination files into
// extraneous ones").
//
// The usual guard does not save it either. `deletionGuard` withholds deletions
// when the filter chain is *degraded*, and a chain whose rules were never
// loaded is not degraded — it is healthy and empty. So deletions run at full
// strength, and only on the scheduled path: the manual path calls GetJob, which
// does load them. The two disagree silently.
func TestScheduledRunAppliesTheJobsOwnFilters(t *testing.T) {
	h := newHarness(t, nil)

	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"keep.txt": 32})

	// A file that exists ONLY at the destination, inside an excluded folder.
	// The exclusion is the only thing protecting it.
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	protected := filepath.Join(dstRoot, "archive")
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatalf("creating the protected folder: %v", err)
	}
	victim := filepath.Join(protected, "old-backup.txt")
	if err := os.WriteFile(victim, []byte("must survive\n"), 0o644); err != nil {
		t.Fatalf("seeding the protected file: %v", err)
	}

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("sched-filters"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		// proceed, so nothing else is quietly withholding the deletion and
		// making this test pass for the wrong reason.
		"delete_policy": string(store.DeletePolicyProceed),
		"schedule_cron": "0 2 * * *",
		"enabled":       true,
		"filters": []map[string]any{{
			"scope": "job", "direction": "exclude", "source": "inline",
			"patterns": []string{"archive/"},
		}},
		"destinations": []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	now := time.Now().UTC()
	backdate(t, h, jobID, now.Add(-time.Minute))
	h.scheduler.Tick(context.Background(), now)

	runs := h.runsForJob(t, jobID)
	if len(runs) != 1 {
		t.Fatalf("the schedule produced %d runs, want 1", len(runs))
	}
	runID, _ := runs[0]["id"].(string)
	h.awaitRun(t, runID, 90*time.Second)

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the scheduled run deleted a file the job's own exclusion protected: %v.\n"+
			"The scheduled path is not loading filter_rules; the manual path does, via GetJob.", err)
	}
}

// Phase 5a exit criterion: a scheduled run against an unavailable destination
// reaches a terminal state with no human input.
//
// This is the criterion that makes unattended running safe at all. A prompt
// policy exists so a person can decide what to do about a dead NAS — but at
// 2am there is no person, and a run that waits for one would hold its mounts,
// keep `ActiveForJob` true, and cause every subsequent firing to be skipped
// for as long as it sat there. The configured fallback has to fire on its own.
//
// The destination is blackholed rather than merely misconfigured, because "the
// server is not answering" is the case that can block in the kernel; a wrong
// path fails fast and would prove much less.
func TestScheduledRunAgainstADeadDestinationEndsWithoutAnyone(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	srcID, _, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"unattended.txt": 32})

	// Samba B, so it can be cut off without disturbing the source on A.
	dstID := h.createTarget(smbTarget(uniqueName("dead"), sambaB(t), shareCredentialed, userName, userPassword))
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		t.Fatalf("creating the destination scope: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dstRoot) })

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("sched-dead"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		// prompt, with a short timeout and a skip fallback: the interactive
		// policy is the one that could hang an unattended run, so it is the
		// one worth proving cannot.
		"unavailable_policy": string(store.PolicyPrompt),
		"prompt_timeout_sec": 15,
		"prompt_fallback":    string(store.FallbackSkip),
		"schedule_cron":      "0 2 * * *",
		"enabled":            true,
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	blackhole(t, sambaB(t))

	now := time.Now().UTC()
	backdate(t, h, jobID, now.Add(-time.Minute))
	h.scheduler.Tick(context.Background(), now)

	runs := h.runsForJob(t, jobID)
	if len(runs) != 1 {
		t.Fatalf("the schedule produced %d runs, want 1", len(runs))
	}
	runID, _ := runs[0]["id"].(string)

	// Nobody answers the prompt. It must still finish.
	run := h.awaitRun(t, runID, 3*time.Minute)
	status, _ := run["status"].(string)
	switch status {
	case string(store.RunPartial), string(store.RunFailed):
	default:
		t.Fatalf("an unattended run against a dead destination ended as %q; "+
			"want partial or failed after the fallback", status)
	}

	// And the job is freed, or every later firing would be skipped for as long
	// as this one sat there.
	//
	// Polled rather than asserted outright, because "the run row is terminal"
	// and "the runner released the job" are deliberately two different
	// moments: `execute` writes the final status, then flushes its buffered
	// events, and only then does the deferred `finish` give up the slot.
	// awaitRun returns on the first of those, so asserting the second in the
	// same breath is a race — one this test lost, which is how the ordering
	// came to be written down here.
	//
	// The window is one event flush. Against a 30s scheduler tick the
	// practical consequence is nil; what matters is that it closes at all.
	deadline := time.Now().Add(30 * time.Second)
	for h.runner.ActiveForJob(jobID) {
		if time.Now().After(deadline) {
			t.Fatal("the job is still marked active 30s after its run finished; " +
				"every subsequent schedule would be skipped")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

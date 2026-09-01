package store

import (
	"context"
	"errors"
	"testing"
)

func startTestRun(t *testing.T, db *DB) (*Run, *Job, string) {
	t.Helper()
	ctx := context.Background()

	src, dst := testTargetPair(t, db)
	job := newTestJob(src, dst)
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	run, err := db.CreateRun(ctx, job.ID, TriggerManual, []string{dst})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run, job, dst
}

func TestRunLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	run, _, dst := startTestRun(t, db)

	if run.Status != RunRunning {
		t.Errorf("new run status = %q, want %q", run.Status, RunRunning)
	}
	if len(run.Destinations) != 1 || run.Destinations[0].Status != DestPending {
		t.Fatalf("new run destinations = %+v, want one pending row", run.Destinations)
	}

	if err := db.SetRunScanTotals(ctx, run.ID, 1200, 4096); err != nil {
		t.Fatalf("SetRunScanTotals: %v", err)
	}
	if err := db.StartRunDestination(ctx, run.ID, dst, 1200, 4096); err != nil {
		t.Fatalf("StartRunDestination: %v", err)
	}
	if err := db.UpdateRunDestinationProgress(ctx, run.ID, dst, 600, 3, 2048); err != nil {
		t.Fatalf("UpdateRunDestinationProgress: %v", err)
	}
	if err := db.FinishRunDestination(ctx, run.ID, dst, DestSuccess, ""); err != nil {
		t.Fatalf("FinishRunDestination: %v", err)
	}
	if err := db.FinishRun(ctx, run.ID, RunSuccess, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	loaded, err := db.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if loaded.Status != RunSuccess {
		t.Errorf("status = %q, want %q", loaded.Status, RunSuccess)
	}
	if !loaded.Terminal() {
		t.Error("a finished run should report Terminal")
	}
	if loaded.FinishedAt == nil {
		t.Error("finished_at was not set")
	}
	if loaded.FilesScanned != 1200 || loaded.BytesTotal != 4096 {
		t.Errorf("scan totals = %d files / %d bytes", loaded.FilesScanned, loaded.BytesTotal)
	}

	rd := loaded.Destinations[0]
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"status", rd.Status, DestSuccess},
		{"files_total", rd.FilesTotal, int64(1200)},
		{"files_done", rd.FilesDone, int64(600)},
		{"files_deleted", rd.FilesDeleted, int64(3)},
		{"bytes_done", rd.BytesDone, int64(2048)},
	} {
		if f.got != f.want {
			t.Errorf("destination %s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if rd.StartedAt == nil || rd.FinishedAt == nil {
		t.Error("destination timestamps were not set")
	}
}

func TestListRunsFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	run, job, _ := startTestRun(t, db)

	if err := db.FinishRun(ctx, run.ID, RunFailed, "boom"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	byJob, err := db.ListRuns(ctx, RunFilter{JobID: job.ID})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(byJob) != 1 {
		t.Fatalf("runs for the job = %d, want 1", len(byJob))
	}

	byStatus, err := db.ListRuns(ctx, RunFilter{Status: RunFailed})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(byStatus) != 1 {
		t.Fatalf("failed runs = %d, want 1", len(byStatus))
	}

	none, err := db.ListRuns(ctx, RunFilter{Status: RunSuccess})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("successful runs = %d, want 0", len(none))
	}
}

func TestGetMissingRun(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.GetRun(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRun of a missing id = %v, want ErrNotFound", err)
	}
}

// A run left "running" by a crashed process must not stay that way forever.
func TestReconcileInterruptedRuns(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	run, _, _ := startTestRun(t, db)

	n, err := db.ReconcileInterruptedRuns(ctx)
	if err != nil {
		t.Fatalf("ReconcileInterruptedRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d runs, want 1", n)
	}

	loaded, err := db.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if loaded.Status != RunFailed {
		t.Errorf("status = %q, want %q", loaded.Status, RunFailed)
	}
	if loaded.ErrorSummary == "" {
		t.Error("no explanation was recorded for the interrupted run")
	}
	if loaded.Destinations[0].Status != DestFailed {
		t.Errorf("destination status = %q, want %q", loaded.Destinations[0].Status, DestFailed)
	}
}

func TestRunEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	run, _, dst := startTestRun(t, db)

	events := []RunEvent{
		{RunID: run.ID, Level: LevelInfo, Message: "scan started"},
		{RunID: run.ID, Level: LevelWarn, DestTargetID: dst, RelPath: "a/link", Message: "skipped a symlink"},
		{RunID: run.ID, Level: LevelError, DestTargetID: dst, RelPath: "b/file.bin", Message: "copy failed"},
	}
	if err := db.AppendEvents(ctx, events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	all, err := db.ListEvents(ctx, EventFilter{RunID: run.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("events = %d, want 3", len(all))
	}
	if all[0].Message != "scan started" {
		t.Errorf("events are not in oldest-first order: %+v", all)
	}

	errorsOnly, err := db.ListEvents(ctx, EventFilter{RunID: run.ID, Level: LevelError})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(errorsOnly) != 1 || errorsOnly[0].RelPath != "b/file.bin" {
		t.Fatalf("errors-only filter returned %+v", errorsOnly)
	}

	counts, err := db.CountEventsByLevel(ctx, run.ID)
	if err != nil {
		t.Fatalf("CountEventsByLevel: %v", err)
	}
	if counts[LevelInfo] != 1 || counts[LevelWarn] != 1 || counts[LevelError] != 1 {
		t.Errorf("counts = %v", counts)
	}

	page, err := db.ListEvents(ctx, EventFilter{RunID: run.ID, Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page) != 2 || page[0].Message != "skipped a symlink" {
		t.Fatalf("paging returned %+v", page)
	}
}

// Deleting a run must take its events and destination rows with it.
func TestRunEventsCascade(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	run, _, _ := startTestRun(t, db)

	if err := db.AppendEvent(ctx, &RunEvent{RunID: run.ID, Level: LevelInfo, Message: "hello"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, run.ID); err != nil {
		t.Fatalf("deleting the run: %v", err)
	}

	events, err := db.ListEvents(ctx, EventFilter{RunID: run.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events survived their run: %+v", events)
	}
}

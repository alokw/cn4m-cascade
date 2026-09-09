package scheduler

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
	_ "modernc.org/sqlite"
)

// fakeRunner records what the scheduler asked it to start, and can pretend a
// job is already running.
type fakeRunner struct {
	mu      sync.Mutex
	started []string
	active  map[string]bool
	err     error
}

func (f *fakeRunner) StartScheduled(_ context.Context, job *store.Job) (*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.started = append(f.started, job.ID)
	return &store.Run{ID: "run-" + job.ID, JobID: job.ID}, nil
}

func (f *fakeRunner) ActiveForJob(jobID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[jobID]
}

func (f *fakeRunner) startedJobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

func testScheduler(t *testing.T) (*Scheduler, *store.DB, *fakeRunner) {
	s, db, runs, _ := testSchedulerAt(t)
	return s, db, runs
}

func testSchedulerAt(t *testing.T) (*Scheduler, *store.DB, *fakeRunner, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sched.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	runs := &fakeRunner{active: map[string]bool{}}
	return New(db, runs, slog.New(slog.NewTextHandler(io.Discard, nil))), db, runs, path
}

// seedJob writes a job with a source target, since jobs carry a foreign key.
func seedJob(t *testing.T, db *store.DB, name, cron string, enabled bool) *store.Job {
	t.Helper()
	ctx := context.Background()

	// Two separate roots: a job whose destination is its own source is
	// refused, and rightly so.
	src := &store.Target{Name: name + "-src", Type: store.TargetLocal, LocalPath: t.TempDir()}
	if err := db.CreateTarget(ctx, src); err != nil {
		t.Fatalf("creating the source target: %v", err)
	}
	dst := &store.Target{Name: name + "-dst", Type: store.TargetLocal, LocalPath: t.TempDir()}
	if err := db.CreateTarget(ctx, dst); err != nil {
		t.Fatalf("creating the destination target: %v", err)
	}

	job := &store.Job{
		Name:           name,
		SourceTargetID: src.ID,
		Mode:           store.ModeMirror,
		ScheduleCron:   cron,
		Enabled:        enabled,
		Destinations:   []store.JobDestination{{DestTargetID: dst.ID}},
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("creating job %q: %v", name, err)
	}
	return job
}

// due forces a job to be due by backdating its next firing.
func due(t *testing.T, db *store.DB, job *store.Job, at time.Time) {
	t.Helper()
	if err := db.SetNextRun(context.Background(), job.ID, at); err != nil {
		t.Fatalf("backdating the next run: %v", err)
	}
}

func TestTickStartsOnlyWhatIsDue(t *testing.T) {
	s, db, runs := testScheduler(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 2, 0, 30, 0, time.UTC)

	ready := seedJob(t, db, "ready", "0 2 * * *", true)
	due(t, db, ready, now.Add(-time.Minute))

	// Scheduled, enabled, but not yet due.
	later := seedJob(t, db, "later", "0 23 * * *", true)
	due(t, db, later, now.Add(time.Hour))

	// Scheduled but paused. This is the one that matters: `enabled` exists so
	// a backup can be paused without losing its expression, and a paused job
	// that still fires would make the control a lie.
	paused := seedJob(t, db, "paused", "0 2 * * *", false)
	due(t, db, paused, now.Add(-time.Minute))

	// No schedule at all.
	manual := seedJob(t, db, "manual", "", true)
	due(t, db, manual, now.Add(-time.Minute))

	s.Tick(ctx, now)

	started := runs.startedJobs()
	if len(started) != 1 || started[0] != ready.ID {
		t.Fatalf("started %v, want exactly the due job %s (later=%s paused=%s manual=%s)",
			started, ready.ID, later.ID, paused.ID, manual.ID)
	}
}

func TestTickAdvancesTheNextRunSoAJobDoesNotRefire(t *testing.T) {
	s, db, runs := testScheduler(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 2, 0, 30, 0, time.UTC)

	job := seedJob(t, db, "nightly", "0 2 * * *", true)
	due(t, db, job, now.Add(-time.Minute))

	s.Tick(ctx, now)
	// A second tick a moment later must not fire it again: the whole point of
	// advancing the clock is that a 30-second tick does not start the same
	// nightly job twice within its firing minute.
	s.Tick(ctx, now.Add(TickInterval))

	if started := runs.startedJobs(); len(started) != 1 {
		t.Fatalf("the job fired %d times across two ticks, want 1: %v", len(started), started)
	}

	stored, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC)
	if !stored.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at = %v, want tomorrow at %v", stored.NextRunAt, want)
	}
}

func TestOverlappingRunIsSkippedAndStillRescheduled(t *testing.T) {
	s, db, runs := testScheduler(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 2, 0, 30, 0, time.UTC)

	job := seedJob(t, db, "slow", "0 2 * * *", true)
	due(t, db, job, now.Add(-time.Minute))
	runs.active[job.ID] = true

	s.Tick(ctx, now)

	if started := runs.startedJobs(); len(started) != 0 {
		t.Fatalf("started %v while a run was already going; it should have been skipped", started)
	}

	// Rescheduling on the skip path is what stops the job being retried every
	// tick for the whole duration of the run it collided with.
	stored, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.NextRunAt.After(now) {
		t.Fatalf("a skipped job kept next_run_at = %v, which is still due at %v — "+
			"it would be retried every tick", stored.NextRunAt, now)
	}
}

func TestAFailedStartStillReschedules(t *testing.T) {
	s, db, runs := testScheduler(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 2, 0, 30, 0, time.UTC)

	job := seedJob(t, db, "broken", "0 2 * * *", true)
	due(t, db, job, now.Add(-time.Minute))
	runs.err = context.DeadlineExceeded

	s.Tick(ctx, now)

	stored, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.NextRunAt.After(now) {
		t.Fatalf("a job whose start failed kept next_run_at = %v; it would retry every tick forever",
			stored.NextRunAt)
	}
}

// A firing that came due while the process was down must NOT run on startup.
// Several missed jobs all starting at boot, at an hour nobody chose, is worse
// than skipping a night — but skipping silently is worse than either, so the
// job is rescheduled and the miss is logged.
func TestMissedFiringsDoNotRunAtStartup(t *testing.T) {
	s, db, runs := testScheduler(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	job := seedJob(t, db, "overnight", "0 2 * * *", true)
	due(t, db, job, time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)) // seven hours ago

	s.catchUpMissed(ctx)

	if started := runs.startedJobs(); len(started) != 0 {
		t.Fatalf("startup started %v; a missed schedule must not fire late", started)
	}

	stored, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC)
	if !stored.NextRunAt.Equal(want) {
		t.Fatalf("after a missed firing next_run_at = %v, want the next one at %v",
			stored.NextRunAt, want)
	}
}

// An unparseable expression can only reach the database by some route that
// bypasses validation, but if one does the job must not become permanently
// due and retried on every tick forever.
func TestAnUnparseableScheduleIsClearedRatherThanRetriedForever(t *testing.T) {
	s, db, runs, path := testSchedulerAt(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 2, 0, 30, 0, time.UTC)

	job := seedJob(t, db, "corrupt", "0 2 * * *", true)

	// Written through a second connection rather than through the store: the
	// store validates, which is exactly what this test needs to bypass, and
	// widening its API with an Exec just to reach this case would be a worse
	// trade than opening the file twice.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening a second connection: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx,
		`UPDATE jobs SET schedule_cron = 'not a schedule' WHERE id = ?`, job.ID); err != nil {
		t.Fatalf("corrupting the schedule: %v", err)
	}
	due(t, db, job, now.Add(-time.Minute))

	s.Tick(ctx, now)
	s.Tick(ctx, now.Add(TickInterval))

	if started := runs.startedJobs(); len(started) != 0 {
		t.Fatalf("started %v from an unparseable schedule", started)
	}
	stored, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.NextRunAt.IsZero() {
		t.Fatalf("next_run_at = %v, want it cleared so the job stops being due", stored.NextRunAt)
	}
}

func TestShutdownStopsTheLoop(t *testing.T) {
	s, _, _ := testScheduler(t)
	s.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

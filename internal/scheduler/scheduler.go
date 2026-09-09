// Package scheduler fires jobs on their cron schedules (SPEC.md §11, Phase 5a).
//
// The database is the source of truth, not an in-memory registry. Every tick
// asks SQLite which jobs are due and recomputes their next firing, so a job
// edited through any path is picked up by the next tick and there is no entry
// table to keep in step with the jobs table. That costs one indexed query every
// thirty seconds and removes the whole class of bug where the schedule someone
// sees is not the schedule that runs.
//
// This package deliberately owns no clock of its own beyond the injected `now`:
// the awkward parts of scheduling are all about time, and tests that have to
// sleep to reach them do not get written.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// TickInterval is how often the database is asked for due jobs.
//
// Comfortably below the one-minute granularity of a cron expression, so a
// firing minute cannot be stepped over. It is not a precision guarantee: a job
// scheduled for 02:00 starts within this window of 02:00, which is what a
// backup schedule means in practice and is worth being explicit about rather
// than implying to-the-second accuracy.
const TickInterval = 30 * time.Second

// Runner is the part of *runner.Runner this package needs. Narrow on purpose:
// the scheduler starts runs and asks whether one is already going, and must
// not grow the ability to cancel or inspect them.
type Runner interface {
	// StartScheduled, not Start: the trigger recorded on the run is what
	// later distinguishes an unattended run from one a person watched.
	StartScheduled(ctx context.Context, job *store.Job) (*store.Run, error)
	ActiveForJob(jobID string) bool
}

// Scheduler polls for due jobs and starts them.
type Scheduler struct {
	db     *store.DB
	runner Runner
	log    *slog.Logger

	// now is injectable so tests can reach 3am without waiting for it.
	//
	// It returns *local* time, not UTC, and that is load-bearing rather than
	// incidental: cron.ParseStandard builds a schedule whose location is
	// time.Local, and robfig evaluates the fields in the location of the
	// instant it is handed. Passing a UTC instant would silently make
	// "0 2 * * *" mean 2am UTC while the job editor promised 2am wherever TZ
	// says the server lives. The two agreed only because the container has no
	// TZ set; following the UI's own advice to set one would have broken it.
	now func() time.Time

	stop chan struct{}
	done chan struct{}
	// started guards Shutdown against a scheduler that was built but never
	// Started — the test harness holds one — which would otherwise block until
	// the caller's context expired. stopOnce keeps a second Shutdown from
	// closing an already-closed channel and panicking.
	started  bool
	stopOnce sync.Once
}

// New builds a scheduler. Nothing runs until Start.
func New(db *store.DB, runs Runner, log *slog.Logger) *Scheduler {
	return &Scheduler{
		db:     db,
		runner: runs,
		log:    log.With("component", "scheduler"),
		now:    time.Now,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start begins ticking. It returns immediately.
func (s *Scheduler) Start(ctx context.Context) {
	s.started = true
	go s.loop(ctx)
}

func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)

	// A pass on startup, which is what turns a firing missed while the process
	// was down into a log line and a rescheduled job rather than into silence.
	s.catchUpMissed(ctx)

	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.Tick(ctx, s.now())
		}
	}
}

// Shutdown stops the loop and waits for the current tick to finish.
//
// Safe to call more than once, and safe on a scheduler that was never started:
// both are cheap to support and both are easy to reach from a deferred
// cleanup, where the failure would be a panic or a thirty-second stall in
// something whose only job is to tidy up.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	if !s.started {
		return nil
	}
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// catchUpMissed handles firings that came due while the process was not
// running.
//
// **They do not fire.** A server that was off overnight would otherwise start
// every missed job the moment it came back — several at once, all at boot,
// none at the quiet hour they were scheduled for, and quite possibly while
// somebody is mid-restore. Missing one night is recoverable; a surprise
// stampede of unattended syncs at an unplanned time is the thing the schedule
// existed to avoid.
//
// So each missed job is logged by name and rescheduled forward. The log line is
// the whole point: a skipped backup that nobody can see is indistinguishable
// from one that never ran.
func (s *Scheduler) catchUpMissed(ctx context.Context) {
	now := s.now()
	due, err := s.db.DueJobs(ctx, now)
	if err != nil {
		s.log.Error("could not check for missed schedules at startup", "error", err)
		return
	}
	for _, job := range due {
		s.log.Warn("a scheduled run was missed while the server was not running; "+
			"it will not be started now, and the job is rescheduled",
			"job_id", job.ID, "job", job.Name, "was_due", job.NextRunAt, "schedule", job.ScheduleCron)
		s.reschedule(ctx, job, now)
	}
}

// Tick starts every job that is due as of `now`.
//
// Exported and taking an explicit time so tests can drive it against a real
// database and a real runner without waiting for a wall clock — the alternative
// is a suite that takes a minute per scheduling case and therefore covers one.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	due, err := s.db.DueJobs(ctx, now)
	if err != nil {
		s.log.Error("could not look for due jobs", "error", err)
		return
	}

	for _, job := range due {
		// Reschedule first, whatever happens next. If starting the run fails,
		// or the job is skipped, the firing time must still move forward —
		// otherwise the job stays due and every tick retries it for as long as
		// the condition lasts, which for an overlapping run means every thirty
		// seconds until it finishes.
		//
		// A schedule that will not parse also stops the run here rather than
		// only stopping future ones. The firing was genuinely due, but the
		// configuration that produced it is broken, and starting an unattended
		// sync from configuration nobody can read is the wrong way to fail.
		if !s.reschedule(ctx, job, now) {
			continue
		}

		if s.runner.ActiveForJob(job.ID) {
			// Skipped rather than queued: a job that cannot finish inside its
			// own interval is misconfigured, and quietly stacking runs turns
			// that into a slowly worsening problem instead of a visible one.
			s.log.Warn("skipped a scheduled run because the previous one is still going",
				"job_id", job.ID, "job", job.Name, "schedule", job.ScheduleCron)
			continue
		}

		run, err := s.runner.StartScheduled(ctx, job)
		if err != nil {
			// A run started between the check above and here is the benign
			// overlap case arriving by a different route, and reads as noise
			// at error level.
			if errors.Is(err, runner.ErrAlreadyRunning) {
				s.log.Warn("skipped a scheduled run because one started just before it",
					"job_id", job.ID, "job", job.Name)
				continue
			}
			s.log.Error("could not start a scheduled run",
				"job_id", job.ID, "job", job.Name, "error", err)
			continue
		}
		s.log.Info("started a scheduled run",
			"run_id", run.ID, "job_id", job.ID, "job", job.Name, "schedule", job.ScheduleCron)
	}
}

// reschedule advances a job's next firing past `now`, and reports whether the
// schedule made sense.
func (s *Scheduler) reschedule(ctx context.Context, job *store.Job, now time.Time) bool {
	next, err := store.NextRun(job.ScheduleCron, now)
	ok := err == nil
	if err != nil {
		// Validation rejects unparseable expressions at save time, so reaching
		// here means a row was written by some other route. Clear the firing
		// time rather than leaving the job permanently due and retried every
		// tick forever.
		s.log.Error("a job has a schedule that cannot be parsed; it will not run until it is fixed",
			"job_id", job.ID, "job", job.Name, "schedule", job.ScheduleCron, "error", err)
		next = time.Time{}
	}
	if err := s.db.SetNextRun(ctx, job.ID, next); err != nil {
		// A failed write means the job is still due, so the caller must not
		// start it: the next tick would find it due again and start a second
		// run from the same firing. If the first finished inside the tick
		// interval the overlap guard would not catch that either, and an
		// unattended job would quietly run twice.
		s.log.Error("could not record a job's next run time; skipping this firing "+
			"rather than risking it running twice",
			"job_id", job.ID, "job", job.Name, "error", err)
		return false
	}
	return ok
}

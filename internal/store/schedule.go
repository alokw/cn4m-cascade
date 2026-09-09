package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// MinInterval is the shortest repeat an "@every" expression may ask for.
//
// A minute, matching the granularity of a cron field, and comfortably above
// the scheduler's tick. Kept here rather than in the scheduler package so that
// validation does not depend on the thing it validates for.
const MinInterval = time.Minute

// Schedule is a compiled cron expression. Next(t) gives the first firing
// strictly after t.
type Schedule interface {
	Next(time.Time) time.Time
}

// ParseSchedule compiles a job's cron expression.
//
// Standard five-field cron (minute hour day-of-month month day-of-week) plus
// the "@daily"/"@every 1h" descriptors, which are worth keeping because they
// are the two most people actually want and the two hardest to get wrong.
//
// Seconds are deliberately not accepted. The scheduler reconciles on a tick
// measured in tens of seconds, so a sub-minute expression would silently not
// mean what it says — and a backup tool has no business running every second.
// That covers the six-field form, which ParseStandard rejects outright, and
// "@every 30s", which it does not: an interval shorter than the tick is
// refused below rather than accepted and quietly rounded up.
//
// **Expressions are evaluated in the server's local timezone**, which is UTC
// in a container unless TZ is set. This is what makes "0 2 * * *" mean 2am
// where the operator lives rather than 2am UTC, and it is why every caller
// must hand this package a local instant rather than a UTC one. It also means
// DST applies: on the day a clock jumps forward, a job scheduled inside the
// hour that does not exist does not run that day, and one scheduled inside a
// repeated hour runs once.
//
// The error is user-facing (CLAUDE.md): the library's message is precise but
// assumes you already know the field order, so the expression and the field
// order come with it.
func ParseSchedule(expr string) (Schedule, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("a schedule is required")
	}

	sched, err := cron.ParseStandard(trimmed)
	if err != nil {
		return nil, fmt.Errorf(
			"%q is not a valid schedule: %w. Use five fields — minute hour day-of-month month day-of-week — "+
				`for example "0 2 * * *" for 2am daily, or a shorthand like "@daily" or "@every 6h"`,
			trimmed, err)
	}
	// "@every 10s" parses happily and cannot be honoured: the scheduler looks
	// for due jobs on a tick far coarser than that, so it would fire once per
	// tick and not once per interval. Refusing it is better than accepting an
	// expression whose meaning we silently change.
	if every, ok := sched.(cron.ConstantDelaySchedule); ok && every.Delay < MinInterval {
		return nil, fmt.Errorf(
			"%q repeats every %s, which is more often than this can run. The shortest interval is %s",
			trimmed, every.Delay, MinInterval)
	}
	return sched, nil
}

// NextRun returns the first firing of expr strictly after `after`.
//
// The zero time means "never again", which a valid expression can genuinely
// produce: "0 0 30 2 *" is the 30th of February, parses cleanly, and never
// happens. Callers must treat a zero as unscheduled rather than as an error,
// or a job with an impossible date would be permanently due.
func NextRun(expr string, after time.Time) (time.Time, error) {
	sched, err := ParseSchedule(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after), nil
}

// formatNextRun stores a zero time as the empty string rather than as year
// 0001.
//
// The distinction matters: the scheduler asks for jobs whose next_run_at has
// passed, and "0001-01-01T00:00:00Z" has very much passed. The
// schedule_cron != ” guard on that query means a year-0001 value would not
// actually fire anything today, but storing a sentinel that reads as "due
// since the first century" is the kind of thing that becomes a real bug the
// moment someone writes a query without the guard.
func formatNextRun(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

// RefreshNextRun recomputes when this job should fire next.
//
// Called on every save as well as after each firing, so a job saved with a
// schedule shows its next run immediately rather than after the scheduler's
// next tick — and so changing the expression cannot leave the old expression's
// firing time behind.
//
// A job that is disabled or unscheduled has no next run, and says so with a
// zero time rather than a stale one. An expression that cannot be parsed
// leaves the field alone: Validate rejects those before a save reaches here,
// so this path only runs for expressions already known to be good.
func (j *Job) RefreshNextRun(now time.Time) {
	if !j.Enabled || j.ScheduleCron == "" {
		j.NextRunAt = time.Time{}
		return
	}
	if next, err := NextRun(j.ScheduleCron, now); err == nil {
		j.NextRunAt = next
	}
}

// DueJobs returns the scheduled jobs whose next firing has arrived, oldest
// first so a backlog is worked through in the order it accumulated.
//
// `now` is truncated to the second before it is compared. These are TEXT
// comparisons over RFC3339Nano, which omits trailing zero nanoseconds, so the
// strings vary in length: a stored "02:00:00Z" compares *greater* than a bind
// value of "02:00:00.123456789Z", because 'Z' sorts after '.'. Without the
// truncation a firing whose second the tick happens to land in would be missed
// and delayed a whole tick. Cron times are always whole seconds, so truncating
// can never move the comparison past one.
//
// Both guards are in the SQL rather than in Go: `enabled` and a non-empty
// expression are exactly the partial index from migration 0008, so the query
// the scheduler runs every tick reads only the rows that could possibly match
// rather than every job on the installation.
func (d *DB) DueJobs(ctx context.Context, now time.Time) ([]*Job, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+jobSelectColumns+` FROM jobs
		 WHERE enabled = 1 AND schedule_cron != '' AND next_run_at != '' AND next_run_at <= ?
		 ORDER BY next_run_at`, formatTime(now.UTC().Truncate(time.Second)))
	if err != nil {
		return nil, fmt.Errorf("looking for scheduled jobs that are due: %w", err)
	}
	defer rows.Close()

	jobs := []*Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a scheduled job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("looking for scheduled jobs that are due: %w", err)
	}

	// Destinations *and* filter rules, exactly as GetJob loads them.
	//
	// The filters are not optional and the omission is not a performance
	// question. `resolveChains` re-reads rule *files* at run time, but the rule
	// *rows* come from this struct — so a job handed over without them runs
	// with no filters at all. For a mirror that is a deletion, not merely
	// over-copying: an excluded path at the destination has no source
	// counterpart, so with the exclusion gone the differ calls it extraneous
	// and removes it.
	//
	// The usual protection does not apply either. `deletionGuard` withholds
	// deletions from a *degraded* chain, and a chain whose rules were never
	// loaded is not degraded — it is healthy and empty. Deletions run at full
	// strength.
	//
	// This was live and passing every test until a scheduled run was made to
	// delete a file its job's own exclusion protected
	// (TestScheduledRunAppliesTheJobsOwnFilters).
	for _, job := range jobs {
		if job.Destinations, err = d.jobDestinations(ctx, job.ID); err != nil {
			return nil, err
		}
		if job.Filters, err = d.ListFilterRules(ctx, job.ID); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// SetNextRun stores a job's next firing time on its own, without touching
// anything else.
//
// Deliberately not UpdateJob: that is a full replace which deletes and
// reinserts destinations and filter rules, re-minting their ids. Using it to
// advance a clock would churn a job's children every time a schedule fired,
// and would race a person editing the job in the UI at that moment.
func (d *DB) SetNextRun(ctx context.Context, jobID string, next time.Time) error {
	if _, err := d.sql.ExecContext(ctx,
		`UPDATE jobs SET next_run_at = ? WHERE id = ?`, formatNextRun(next), jobID); err != nil {
		return fmt.Errorf("recording the next run time of job %s: %w", jobID, err)
	}
	return nil
}

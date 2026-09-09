-- Cron scheduling (SPEC.md §11, Phase 5a).
--
-- `enabled` is separate from `schedule_cron` on purpose: pausing a nightly
-- backup for a week must not mean deleting the expression and retyping it from
-- memory, which is how a schedule comes back subtly wrong.
ALTER TABLE jobs ADD COLUMN schedule_cron TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;

-- next_run_at is derived from schedule_cron, and stored anyway.
--
-- Two reasons it is a column rather than a computation. After a restart, "was
-- a firing missed while we were down?" is only answerable against a time that
-- survived the restart; and a reader wanting to show "next scheduled run" can
-- do so without parsing cron. (The dashboard does not yet show it — that lands
-- with the rest of the Phase 5a UI once the Phase 4 browser pass is done.) A tick recomputes it for every
-- enabled job, so a value that disagrees with the expression corrects itself
-- within one tick rather than persisting as stale derived state.
--
-- Empty means unscheduled. Times are RFC3339 UTC, like every other timestamp
-- in this schema.
ALTER TABLE jobs ADD COLUMN next_run_at TEXT NOT NULL DEFAULT '';

-- Finding due jobs is the scheduler's only hot query and it runs every tick.
-- Partial index: unscheduled jobs are the majority on most installations and
-- are never due, so keeping them out of the index keeps it small.
CREATE INDEX idx_jobs_due ON jobs(next_run_at)
    WHERE enabled = 1 AND schedule_cron != '';

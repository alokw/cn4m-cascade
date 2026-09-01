-- Phase 2: jobs, single-destination runs, and the run/task log.
--
-- Columns for later phases are deliberately absent (D-8): schedule_cron and
-- api_trigger_token (Phase 5), unavailable_policy/prompt_*/parallel_destinations
-- (Phase 3), bwlimit_kbps (Phase 6).
--
-- Deviations from SPEC.md §7 `jobs`, approved 2026-08-31:
--   compare_tolerance_sec — §6.2 requires a configurable mtime tolerance
--   ignore_dst_hour       — §6.2 requires the ±1 hour DST toggle
--   on_error              — §6.3 makes skip-vs-abort a per-job setting
--   log_every_file        — §6.5's counts-not-dumps principle; a 100k-file run
--                           would otherwise write 100k rows to run_events
--   delete_policy         — §7 lists this column but §6 never defines it. It is
--                           given a meaning here: what to do about mirror
--                           deletions when the run had failures.
--
-- Enum columns that later phases will extend (mode, compare, status) carry no
-- CHECK constraint: SQLite cannot alter one without rebuilding the table, so
-- they are validated in Go instead.

CREATE TABLE jobs (
    id                    TEXT PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE,
    source_target_id      TEXT NOT NULL REFERENCES targets(id),
    source_subpath        TEXT NOT NULL DEFAULT '',
    mode                  TEXT NOT NULL,                       -- mirror | update
    compare               TEXT NOT NULL DEFAULT 'fast',        -- fast (content: Phase 6)
    compare_tolerance_sec INTEGER NOT NULL DEFAULT 2,
    ignore_dst_hour       INTEGER NOT NULL DEFAULT 0,
    workers               INTEGER NOT NULL DEFAULT 4,
    on_error              TEXT NOT NULL DEFAULT 'skip',        -- skip | abort
    delete_policy         TEXT NOT NULL DEFAULT 'skip_deletes',-- skip_deletes | proceed
    log_every_file        INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);

-- Its own table from the start even though Phase 2 allows exactly one row per
-- job: Phase 3 fans out to many, and storing the destination on `jobs` would
-- mean rewriting the schema then.
CREATE TABLE job_destinations (
    id             TEXT PRIMARY KEY,
    job_id         TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    dest_target_id TEXT NOT NULL REFERENCES targets(id),
    dest_subpath   TEXT NOT NULL DEFAULT '',
    position       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_job_destinations_job ON job_destinations(job_id);

CREATE TABLE runs (
    id            TEXT PRIMARY KEY,
    job_id        TEXT NOT NULL REFERENCES jobs(id),
    trigger       TEXT NOT NULL,                               -- manual (schedule|webhook: Phase 5)
    status        TEXT NOT NULL,                               -- running|success|partial|failed|cancelled
    started_at    TEXT NOT NULL,
    finished_at   TEXT,
    files_scanned INTEGER NOT NULL DEFAULT 0,
    bytes_total   INTEGER NOT NULL DEFAULT 0,
    error_summary TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_runs_job ON runs(job_id, started_at DESC);

CREATE TABLE run_destinations (
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    dest_target_id TEXT NOT NULL REFERENCES targets(id),
    status         TEXT NOT NULL,                              -- pending|running|success|failed|cancelled
    files_total    INTEGER NOT NULL DEFAULT 0,
    files_done     INTEGER NOT NULL DEFAULT 0,
    files_deleted  INTEGER NOT NULL DEFAULT 0,
    bytes_total    INTEGER NOT NULL DEFAULT 0,
    bytes_done     INTEGER NOT NULL DEFAULT 0,
    started_at     TEXT,
    finished_at    TEXT,
    error_summary  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, dest_target_id)
);

CREATE TABLE run_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    ts             TEXT NOT NULL,
    level          TEXT NOT NULL,                              -- info | warn | error
    dest_target_id TEXT NOT NULL DEFAULT '',
    relpath        TEXT NOT NULL DEFAULT '',
    message        TEXT NOT NULL
);
CREATE INDEX idx_run_events_run_level ON run_events(run_id, level);
CREATE INDEX idx_run_events_ts ON run_events(ts);

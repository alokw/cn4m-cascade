-- Phase 4a: session auth (SPEC.md §8), the interactive prompt policy
-- (SPEC.md §6.6), and the indexes the global log view needs (SPEC.md §8
-- /api/logs).
--
-- The prompt columns were reserved by 0003 and land here, as planned.
--
-- No CHECK on prompt_fallback or on run/destination status (D-20): SQLite
-- cannot alter a CHECK without rebuilding the table, and Phase 5 extends the
-- status range again. Validation lives in Go.

-- How long a run waits for a human before acting without one. The whole point
-- of the availability gate is that a run may be unattended, so the wait must
-- be bounded: nothing in this system may block forever.
ALTER TABLE jobs ADD COLUMN prompt_timeout_sec INTEGER NOT NULL DEFAULT 600;

-- What an unanswered prompt does. 'skip' leaves the destination alone and lets
-- the next run correct it; 'abort' ends the run. Skip is the default because
-- it is the recoverable direction.
ALTER TABLE jobs ADD COLUMN prompt_fallback TEXT NOT NULL DEFAULT 'skip';   -- skip | abort

-- Sessions hold only a hash of the token. A stolen database must not yield
-- live sessions, which is the same reason target passwords are encrypted
-- rather than stored (SPEC.md §5).
CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- GET /api/logs reads events across every run, filtered by level and time.
-- 0002 already indexes run_events(ts) for the time ordering; this covers the
-- errors-only view, which is the one a user leaves open.
CREATE INDEX idx_run_events_level_ts ON run_events(level, ts);

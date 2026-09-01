-- Phase 3: the filter chain (SPEC.md §6.5) and multi-destination fan-out.
--
-- Deferred to Phase 4 (D-8): prompt_timeout_sec and prompt_fallback, which
-- only mean anything once there is a UI to prompt in.
--
-- `unavailable_policy` carries no CHECK constraint (D-20): Phase 4 adds
-- 'prompt' to its range, and SQLite cannot alter a CHECK without rebuilding
-- the table. Validation lives in Go.

ALTER TABLE jobs ADD COLUMN unavailable_policy TEXT NOT NULL DEFAULT 'skip';   -- skip | abort (prompt: Phase 4)
ALTER TABLE jobs ADD COLUMN parallel_destinations INTEGER NOT NULL DEFAULT 0;

CREATE TABLE filter_rules (
    id              TEXT PRIMARY KEY,
    job_id          TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,

    -- scope 'job' applies to every destination; 'target' applies only to the
    -- destination named by scope_target_id, so two destinations of one job
    -- can receive different subsets (SPEC.md §6.5).
    scope           TEXT NOT NULL DEFAULT 'job',
    scope_target_id TEXT NOT NULL DEFAULT '',

    direction       TEXT NOT NULL,                    -- include | exclude
    source          TEXT NOT NULL,                    -- inline | listfile | jsonfile

    patterns_json   TEXT NOT NULL DEFAULT '[]',       -- source=inline
    file_path       TEXT NOT NULL DEFAULT '',         -- source=listfile|jsonfile
    json_key        TEXT NOT NULL DEFAULT '',         -- source=jsonfile, dot-path

    case_sensitive  INTEGER NOT NULL DEFAULT 0,       -- SMB is usually case-insensitive
    on_error        TEXT NOT NULL DEFAULT 'fail_run', -- fail_run | ignore_rule
    position        INTEGER NOT NULL DEFAULT 0,

    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE INDEX idx_filter_rules_job ON filter_rules(job_id, position);

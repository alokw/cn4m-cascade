-- Inbound trigger tokens and outbound status callbacks (SPEC.md §8, Phase 5b).

-- The token is stored as a SHA-256 hash, never in the clear, exactly as
-- session tokens are (0004's sessions.token_hash). It is shown to the user
-- once, when it is issued, and cannot be shown again — which is the whole
-- point: a database that leaks contains nothing that can start a run.
--
-- NULL means this job has no token and its hook endpoints refuse everything.
-- Revoking is setting it back to NULL; regenerating is overwriting it, which
-- makes the previous token stop working in the same statement.
ALTER TABLE jobs ADD COLUMN api_trigger_token_hash TEXT;

-- Looking a token up is the first thing an unauthenticated request does, so it
-- must not be a table scan: an endpoint that gets slower the more jobs exist
-- is an endpoint worth flooding. Partial, because most jobs have no token.
CREATE UNIQUE INDEX idx_jobs_trigger_token ON jobs(api_trigger_token_hash)
    WHERE api_trigger_token_hash IS NOT NULL;

-- Outbound callbacks (SPEC.md §8.2).
--
-- job_id is nullable: a NULL row subscribes to every job, which is what makes
-- "tell my dashboard about everything" configurable once rather than per job.
CREATE TABLE webhooks (
    id               TEXT PRIMARY KEY,
    job_id           TEXT REFERENCES jobs(id) ON DELETE CASCADE,
    url              TEXT NOT NULL,
    -- JSON array of event names: run_started, progress,
    -- target_unavailable_prompt, run_completed, run_failed.
    events_json      TEXT NOT NULL DEFAULT '[]',
    -- The HMAC-SHA256 key the receiver verifies X-Signature with. Encrypted
    -- at rest with the same box as target passwords: unlike a trigger token
    -- this one has to be recoverable, because signing needs the actual key.
    secret_encrypted TEXT NOT NULL DEFAULT '',
    enabled          INTEGER NOT NULL DEFAULT 1,
    -- Throttle for `progress`, which would otherwise fire once a second for
    -- the whole of a multi-hour run.
    min_interval_sec INTEGER NOT NULL DEFAULT 30,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- Delivery asks "which hooks care about this job?" on every event, so both
-- the per-job rows and the global ones need to be cheap to find.
CREATE INDEX idx_webhooks_job ON webhooks(job_id) WHERE enabled = 1;

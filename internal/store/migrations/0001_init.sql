-- Phase 1 schema: targets and settings only.
--
-- SPEC.md §7 lists the full v1 schema; the remaining tables (jobs, runs,
-- filter_rules, ...) are created by the migration for the phase that first
-- needs them, so that the schema and the code stay in step.
--
-- Deviations from SPEC.md §7 `targets`, approved 2026-08-31 (PROGRESS.md Q-1):
--   local_path      — §7 has no column for a type=local target's path
--   port            — non-default SMB ports
--   multichannel    — §5 requires a per-target multichannel toggle
--   negotiated_vers — §5 requires "recording which version worked"
--   updated_at      — §7 has created_at only

CREATE TABLE targets (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    type                TEXT NOT NULL CHECK (type IN ('smb', 'local')),
    host                TEXT NOT NULL DEFAULT '',
    share               TEXT NOT NULL DEFAULT '',
    subpath             TEXT NOT NULL DEFAULT '',
    local_path          TEXT NOT NULL DEFAULT '',
    port                INTEGER NOT NULL DEFAULT 0,
    username            TEXT NOT NULL DEFAULT '',
    password_encrypted  TEXT NOT NULL DEFAULT '',
    domain              TEXT NOT NULL DEFAULT '',
    mount_opts_override TEXT NOT NULL DEFAULT '',
    multichannel        INTEGER NOT NULL DEFAULT 0,
    negotiated_vers     TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

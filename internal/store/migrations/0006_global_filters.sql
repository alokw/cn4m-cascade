-- Global exclusion rules: applied to every job, in addition to that job's own
-- rules (SPEC.md §6.5).
--
-- A separate table rather than a nullable filter_rules.job_id. That column is
-- NOT NULL with a foreign key, and SQLite cannot relax either without a full
-- table rebuild — but the stronger reason is ownership: filter_rules is wholly
-- owned by its job, to the point that UpdateJob deletes every row for the job
-- and reinserts. Global rules have an independent lifecycle and must survive
-- that.
--
-- No scope or scope_target_id: a global rule is job-wide by definition, which
-- is also what lets it prune the shared source walk the way a job-scoped rule
-- does. No direction either: these are exclusions only. A global *include*
-- would widen every job rather than narrow it, because includes are OR'd — a
-- global "*.txt" plus a job's "*.jpg" would admit both, the opposite of what
-- anyone means by a global filter.
CREATE TABLE global_filter_rules (
    id             TEXT PRIMARY KEY,
    source         TEXT NOT NULL,                              -- inline | listfile | jsonfile
    patterns_json  TEXT NOT NULL DEFAULT '[]',
    file_path      TEXT NOT NULL DEFAULT '',
    json_key       TEXT NOT NULL DEFAULT '',
    case_sensitive INTEGER NOT NULL DEFAULT 0,
    on_error       TEXT NOT NULL DEFAULT 'fail_run',           -- fail_run | ignore_rule
    position       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE INDEX idx_global_filter_rules_position ON global_filter_rules(position, id);

-- Seeded defaults: the noise every job wants gone. Files are bare names, which
-- under gitignore semantics match at any depth. Folders carry a trailing slash,
-- which makes them directory rules that prune the whole subtree — much cheaper
-- than matching every path inside it.
--
-- This is a one-time seed. Editing the list later is a user action through
-- Settings; it does not re-seed, and changes here do not reach existing
-- installs.
INSERT INTO global_filter_rules
    (id, source, patterns_json, case_sensitive, on_error, position, created_at, updated_at)
VALUES (
    lower(hex(randomblob(16))),
    'inline',
    '[".symmetry-state.json","ROBOCOPY.RCJ","D3_bpc_16-9.d","desktop.ini",".DS_Store","assets.json",".sync.ffs_db","sync.ffs_db","sync.ffs_lock","Thumbs.db","videoin_1.mov","videoin_2.mov","videoin_3.mov","videoin_4.mov","videoin_5.mov","videoin_6.mov","videoin_7.mov","videoin_8.mov","videoin_9.mov","videoin_10.mov","videoin_11.mov","videoin_12.mov","videoin_13.mov","videoin_14.mov","videoin_15.mov","videoin_16.mov","ada.jpg","george.jpg","ACES2065-1_ColorChecker2014.exr","D3_color_16-9.png","D3_color_4-3.png","D3_test_16-9.png","D3_test_4-3.png","~private-asp"]',
    0, 'fail_run', 0,
    strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
), (
    lower(hex(randomblob(16))),
    'inline',
    '["_ARCHIVE/","~private-asp*/"]',
    0, 'fail_run', 1,
    strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
);

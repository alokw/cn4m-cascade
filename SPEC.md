# Project Spec: Web-Based SMB Sync Engine

## Purpose of this document
This is a design/architecture spec intended to be handed to an AI coding assistant (or a developer) for implementation. It describes goals, architecture, technology choices, component designs, and a phased build plan. Implement in phases, in order — each phase should produce something runnable and testable.

---

## 1. Goals

- A self-hosted file sync tool for SMB shares and local folders, running in a single Docker container.
- Web-based front-end for all configuration and monitoring (no desktop client).
- Sync **sources and destinations are primarily SMB/CIFS shares addressed by IP** (e.g. `//192.168.1.50/media`), added dynamically by the user through the UI. Local paths (bind-mounted into the container) must also work as sources/destinations.
- **Throughput is a first-class requirement.** The design should be able to saturate a gigabit link on large files and perform well on many-small-file workloads via parallelism.
- Sync modes:
  - **Mirror** — make destination exactly match source (deletes extraneous dest files).
  - **Update** — copy new/newer files to destination, never delete.
  - ~~**Two-way**~~ — bidirectional sync with a persisted state database. **Deferred indefinitely; see §14.**
- Jobs can be run manually, on a schedule (cron-style), or **triggered externally via a tokenized webhook URL**; sync status (state, progress, ETA) is likewise **queryable via a tokenized status URL** and can be **pushed to user-configured outbound callback URLs**.
- A job may have **one source and one or more destination targets** (fan-out). Progress/ETA is reported per file, per destination target, and for the job as a whole.
- **Flexible include/exclude filtering**: inline lists, wildcard patterns, external JSON files (with a user-specified key selecting the list inside the file), attachable at both the job level and per-target.
- If a destination target is unavailable at run time, the run **pauses and prompts the user to skip that target and continue** with the rest (with a configurable non-interactive fallback for scheduled/webhook runs).
- Robust behavior when a share goes offline mid-transfer: fail that target cleanly, never hang forever, never corrupt state.
- Persistent, browsable **task log and error log** (per run and global).

## 2. Non-goals (v1)

- No multi-user auth/RBAC — single admin user with a password is sufficient.
- No file versioning/recycle-bin (log deletions; add versioning later).
- No real-time/continuous watching (scheduled + manual only in v1).
- No protocols beyond SMB and local paths (no SFTP/S3/NFS in v1, but don't design them out — see storage abstraction below).

---

## 3. High-level architecture

```
┌─────────────────────────────────────────────────────────┐
│ Docker container (network_mode: host)                   │
│                                                         │
│  ┌──────────────┐   HTTP/WS    ┌─────────────────────┐  │
│  │  Web UI      │◄────────────►│  Backend (Go)       │  │
│  │  (React SPA, │              │  - REST API         │  │
│  │  served by   │              │  - WebSocket events │  │
│  │  backend)    │              │  - Job scheduler    │  │
│  └──────────────┘              │  - Sync engine      │  │
│                                │  - Mount manager    │  │
│                                └──────┬──────────────┘  │
│                                       │                 │
│                          ┌────────────┼────────────┐    │
│                          ▼            ▼            ▼    │
│                     SQLite DB    /mnt/smb/<id>  local   │
│                     (config,     kernel CIFS    bind    │
│                      job state,  mounts         mounts  │
│                      2-way sync                        │
│                      database)                          │
└─────────────────────────────────────────────────────────┘
```

**Key decisions (already made — do not revisit unless blocked):**

1. **Backend language: Go.** Goroutines map perfectly onto concurrent tree scanning and parallel copy workers; single static binary keeps the image small.
2. **SMB access via the platform's kernel SMB client, not a userspace SMB library.** The engine is handed ordinary filesystem paths and never learns the protocol behind them (§4). Two implementations of that, both satisfying `mountmgr.Mounter`:
   - **Linux / container:** mount shares on demand at `/mnt/smb/<target-id>` with `mount.cifs`, then treat them as ordinary paths. Kernel-level readahead, large `rsize`/`wsize`, SMB3 multichannel.
   - **Windows / native (added 2026-09-10):** there is nothing to mount. Windows opens `\\host\share\path` directly, so the connector authenticates the session with `WNetAddConnection2` and hands back the UNC root as the path. Credentials go in memory and never reach disk, which is strictly better than the Linux temp-file rule rather than a concession to it.

   `mount.cifs` is Linux-only, and a container on Docker Desktop measured **~5× slower** on sustained network transfer than the same host natively (PERFORMANCE.md). A platform-native SMB client is therefore a supported architecture, not a workaround.
3. **`network_mode: host`** to eliminate NAT overhead and any SMB networking weirdness — **on Linux, where it means what it says.** On Docker Desktop (Windows and macOS) containers run in a VM behind an internal gateway that `--network host` does not escape, so this decision cannot be honoured there. That is the measured reason the native Windows deployment exists.

   **Recommended deployment per platform:** Docker with `network_mode: host` on Linux; the **native binary** on Windows; Docker Desktop for development everywhere.
4. **Container capabilities:** `SYS_ADMIN` and `DAC_READ_SEARCH` (required for mount.cifs). Document clearly in the README that this is a privileged-ish container and why.
5. **SQLite** (via `mattn/go-sqlite3` or `modernc.org/sqlite`) for all persistence: targets, job definitions and run history. (The two-way state database is deferred — §14.)
6. **Frontend: React + Vite SPA**, embedded into the Go binary with `embed.FS` so the container serves everything from one process. WebSocket for live progress.

---

## 4. Storage abstraction

Even though v1 is SMB + local only, define a minimal interface so future backends (SFTP, S3) are possible:

```go
type Storage interface {
    // Resolve returns a local filesystem root path for this target,
    // ensuring it is ready (mounted, reachable). May block briefly.
    Resolve(ctx context.Context) (rootPath string, err error)
    // Health checks reachability cheaply (e.g., statfs on mountpoint).
    Health(ctx context.Context) error
    // Release is called when no jobs reference the target (unmount).
    Release(ctx context.Context) error
}
```

Implementations in v1: `LocalStorage` (trivial) and `SMBStorage` (mount manager below). The sync engine only ever sees root paths and `os`/`filepath` operations — it never knows about SMB.

---

## 5. Mount manager (SMBStorage)

Responsibilities:

- **Mount on demand.** When a job starts (or user clicks "test connection"), mount `//<ip>/<share>` at `/mnt/smb/<target-id>` with `mount.cifs`.
- **Mount options** (defaults, user-overridable per target in an "advanced" field):
  `vers=3.1.1,rsize=4194304,wsize=4194304,cache=loose,actimeo=30,soft,echo_interval=10,uid=<appuid>,gid=<appgid>,iocharset=utf8`
  - `soft` + `echo_interval` are critical: they make I/O return errors instead of hanging forever when the server disappears.
  - Try `multichannel` when the user enables it per-target; fall back gracefully if the server rejects it.
  - Fall back through `vers=3.1.1 → 3.0 → 2.1` on mount failure, recording which version worked.
- **Credentials:** stored in SQLite, encrypted at rest with a key derived from an `ENCRYPTION_KEY` env var (fail startup if unset). A **native deployment may supply it, and every other setting, from a `.env` file beside the executable** — the compose deployment has an orchestrator to interpolate one and a native install has nothing doing that. Environment variables take precedence over the file, matching `docker compose`; the file is discovered beside the executable only, never relative to the working directory, because a Windows service's working directory is `system32`. Pass credentials to mount.cifs via a temp credentials file with 0600 perms (never on the command line — visible in `ps`), deleted immediately after mount.
- **Refcounting:** multiple concurrent jobs may use the same target; unmount only when refcount hits zero, with a small idle grace period (e.g., 60s) to avoid mount churn on back-to-back jobs.
- **Stale mount detection:** before reporting a mount as ready, run a `statfs` with a timeout (do the syscall in a goroutine, select on ctx). If it doesn't respond in ~5s, force `umount -l` (lazy) and remount.
- **Startup hygiene:** on boot, lazily unmount anything under `/mnt/smb/*` left over from an unclean shutdown.
- **Health endpoint per target:** used by the UI's "test connection" button and shown as a status dot on each target.

Edge cases the implementation MUST handle:
- Server goes offline mid-job → copy operations return I/O errors (because `soft`); the job fails that file, retries N times with backoff, then aborts the job with a clear error. The mount manager marks the target unhealthy.
- Wrong credentials / share name → surface the mount.cifs stderr to the UI legibly.
- Two targets pointing at the same IP+share with different credentials → allowed; they're distinct mounts at distinct paths.

---

## 6. Sync engine

### 6.1 Pipeline

A job has **one source and one or more destinations**. A run proceeds in stages, all cancellable via context.

**Stages 1 and 3–4 are per *run*; stages 2 and 5–7 are per *destination*, and one destination never waits on another.** The source is resolved and scanned once and the listing is shared. Everything after that — availability gate, diff, preview hold, execute — belongs to a single destination, and a destination that is blocked (unreachable, or parked on a `prompt`) blocks only itself: the others plan and execute past it. The numbered order below describes the stages one destination passes through, not a set of barriers the whole run crosses together. The one exception is the preview gate (stage 6), which by its nature holds every destination, because a human is being asked to approve the whole run before any of it happens.

1. **Resolve** the source and every destination target (mount if needed).
2. **Availability gate:** if any destination fails to resolve, emit a `target_unavailable` event and behave per the job's `unavailable_policy`:
   - `prompt` (default for manual runs): pause **that destination**, push a prompt over WS/UI ("Destination 'NAS-B (192.168.1.51)' is unreachable — Skip and continue / Retry / Abort run"), wait up to `prompt_timeout_sec` (default 600, floor 5, ceiling 86400), then fall back to `prompt_fallback` (`skip` or `abort`). The run itself stays `running` and its other destinations carry on to completion — which is the point of fan-out when one server is down. A prompt must never stall a healthy destination.
   - `skip` (sensible default for scheduled/webhook runs): log it, mark that destination's result as `skipped_unavailable`, continue with the rest.
   - `abort`: fail the whole run immediately.
   A run that completes with ≥1 skipped/failed destination finishes with status `partial`.

   A destination whose **folder simply does not exist yet** is a different question from one that is unreachable, and gets its own answer rather than being folded into `unavailable_policy` — a job set to *skip* unreachable destinations is expressing caution, and silently creating folders is the opposite of it. The per-job `create_dest_dirs` setting decides: `ask` (the default) prompts with **Create the folder / Skip / Abort** and a countdown, `always` creates it and logs that it did, `never` treats it as unavailable. An unanswered prompt skips the destination, so an unattended run never invents a folder from a mistyped subpath. **A preview never creates anything**, whatever the setting says — planning must leave the destination exactly as it found it, or "nothing has been written yet" on the confirm screen is untrue. Only the *subpath* is ever created, never the share or the local root, and never the **source**: a missing source is a misconfiguration, and inventing one turns a typo into a run that copies nothing and calls itself a success.
3. **Scan** the source tree once and each available destination tree **concurrently** (bounded parallel directory walkers — e.g., 8–16 workers per tree, since SMB metadata ops are latency-bound, not bandwidth-bound). Produce trees (or sorted flat lists) of `{relpath, size, mtime, isDir, symlink info}`.
4. **Filter** the source listing through the resolved filter chain (see §6.5). Filtering happens after scan but before diff so the plan and byte totals reflect only in-scope files. Directory exclusions prune the walk itself where possible (skip descending) for speed.
5. **Diff** source vs each destination according to sync mode → one ordered action plan per destination: `[]Action{Copy, Delete, MkDir, RmDir, ConflictSkip...}` with byte totals for progress/ETA reporting.
6. **(Optional) Preview gate:** if the job is run manually with "preview" enabled, plan every destination, then send the plans to the UI and wait for confirmation before executing any of them. This is the one stage that holds the whole run. The run's status is `awaiting_confirmation` while it waits; it holds its mounts so that what is confirmed executes against what was planned, and an unconfirmed plan expires after `prompt_timeout_sec` and changes nothing.
7. **Execute** per destination with a worker pool (default 4 concurrent copy workers per destination, configurable 1–16; destinations run sequentially by default with a per-job `parallel_destinations` toggle — parallel fan-out multiplies read load on the source share). Directories created first (top-down, sequential-ish), file copies in parallel, deletions last (bottom-up).
8. **Finalize:** write per-destination and overall run summaries to DB, fire outbound status callbacks (§8.2), emit completion event.

### 6.1.1 Progress & ETA reporting

Maintained by a central per-run progress tracker, updated by copy workers and published over WS at ~2 Hz (and returned by the status API):

- **Per file in flight:** bytes done / total, current MB/s, ETA for that file.
- **Per destination:** files done / total, bytes done / total, rolling throughput (EWMA over ~15s so ETA doesn't whiplash on mixed file sizes), ETA for that destination.
- **Total run:** aggregate of all destinations, **including not-yet-started ones**, overall ETA, elapsed time. Destinations that have not been planned are **estimated from the mean of the ones that have**, and that estimate is folded into the reported totals rather than published beside them, so a fan-out run does not count upwards as each destination is planned. **The total is of work, never of scope:** it must describe what is going to be transferred, so a source tree that is already in sync contributes nothing to it. (An earlier implementation used the source scan — files × destinations, fixed when the scan ended — which was stable and wrong by three orders of magnitude on a synced tree; see PROGRESS.md D-130.) `estimated_pending_bytes` reports the estimate separately for callers that want it, and is already included in `bytes_total`, so an ETA must not add it again. A destination that never ran contributes neither work nor a pending share.
- ETAs are computed from **byte** progress, not file counts (file counts mislead badly with mixed sizes); display both anyway.
- During the scan phase (before totals are known), report files/dirs discovered per second and mark ETA as "estimating…".
- **The size of the source tree is reported separately, as `scanned_files` and `scanned_bytes`**, in both the live and the finished shape. It answers a different question from the progress total — how big the job is, rather than how much is moving — and an operator needs it *during* a run to anticipate, not only afterwards. It must never be used as the progress denominator: a tree already in sync is large and has nothing to copy.

### 6.2 Comparison rules

- Default: **size + mtime** (with a configurable tolerance, default 2 seconds, because SMB/FAT mtime granularity is coarse; also handle the classic DST/whole-hour offset with an optional "ignore ±1 hour" toggle).
- Optional per-job: **content compare** (streaming hash of both sides). Warn in the UI that this reads every byte over the network.
- Preserve mtimes on copied files (`os.Chtimes` after copy) — this is essential or every subsequent run re-copies everything.
- **A destination file another process holds open is skipped, never retried.** Windows fails the final rename with a sharing violation where Linux would replace the file happily; the owning application decides when it releases, so retrying only spends a run's time waiting. The file is reported as an **error** for accounting — it still blocks mirror deletions, because a destination missing a file it was meant to receive must not have removals run against it — but a destination whose **only** problem was locked files is classified `success` and *degraded*, which makes the run `partial`. It is not a failure: a media server holds its project files open for as long as the project is loaded, so a locked file can stay locked for days, and reporting `failed` every run for a condition nobody can clear teaches people to ignore the status until a real failure goes unnoticed too. One genuine error among the locks makes the destination `failed` as usual. The run tally names the count — `3 succeeded, 0 failed, 0 skipped, 3 file(s) locked` — **inside** the tally rather than after it, so the abbreviated forms that keep only the tally still carry it: "incomplete" beside "0 failed, 0 skipped" reports that something is wrong and nothing about what. The file is — named individually in the run log, and **summarised at completion** so an operator sees "3 files were locked by another process" without reading the whole log. The temp file is always removed.

### 6.3 Copy mechanics (performance-critical)

- Copy via a large userspace buffer (1–4 MiB) `io.CopyBuffer`; on Linux this will also let the kernel use readahead on the CIFS side. Do **not** use tiny default buffers.
- Copy to a temp name in the destination directory (`.partname.<rand>.tmp`), fsync, then rename over the final name. This makes interrupted copies non-destructive and restartable.
- Retry policy per file: 3 attempts with exponential backoff (1s/5s/15s); classify errors — permission errors don't retry, I/O timeouts do.
- Per-job bandwidth limit (optional, token-bucket wrapper around the copy reader).
- Skip-on-error vs abort-on-error is a per-job setting; always record per-file failures in the run log.

### 6.4 Two-way sync state — **deferred, see §13**

Two-way sync is not built and is not planned. The design work done for it, and the reasons it is
dangerous, are kept in §14 rather than here, so that this section describes only what the engine
actually does.

### 6.5 Filtering system

Filters decide which files/folders are in scope. Multiple filter **rules** combine into a per-job (and optionally per-target) filter chain, on top of a set of **global exclusions** that apply to every job.

**Global exclusions** are configured once, in Settings, and layered into every job's chain. They exist because the same noise — `.DS_Store`, `Thumbs.db`, `_ARCHIVE/` — has to be excluded from every job, and repeating it per job guarantees it will eventually be forgotten in one of them. Three deliberate constraints:

- **Exclusions only.** A global *include* would widen every job rather than narrow it, because includes are OR'd: a global `*.txt` plus a job's `*.jpg` would admit both. There is no global include.
- **Absolute.** A job's own include rule cannot re-admit a path a global rule excludes. This falls out of the evaluation order below, and is kept deliberately: the alternative is a layered evaluator inside the code that guarantees prune and admit agree — the guarantee that stops a mirror deleting a subtree one side cannot see.
- **Job-wide, therefore prunable.** A global rule may prune the shared source walk exactly as a job-scoped rule may, and for the same reason: it applies identically to every destination.

A fresh database is **seeded** with a default set of exclusions — editor and OS metadata (`.DS_Store`, `Thumbs.db`, `desktop.ini`), sync-tool state files, archive folders, and a handful of site-specific artefacts — so the common noise is excluded before anyone configures anything. The seed is applied once by the migration that creates the table. It is a starting point, not a fixed policy: the list is fully editable in Settings, and because global exclusions are absolute, a pattern that should not apply must be **removed there** rather than overridden by a job.

Two consequences worth stating, because neither is obvious from the list itself. A seeded pattern that matches a file someone actually syncs makes that file invisible on both sides — never copied again, and never deleted either, so the destination keeps a stale copy with nothing to signal it. And should the seed ever ship to a database that predates it, it would apply to already-configured jobs without their owner choosing it; that is acceptable only while no such deployment exists.

A global rule that cannot be loaded degrades **every** job's chain, disabling deletions for those runs — the same treatment a dropped per-job rule gets, because the consequence is the same: a filter narrower than configured turns protected files into extraneous ones.

**Rule sources (all produce a list of patterns):**

1. **Inline list** — patterns entered directly in the UI (one per line) or supplied as a JSON array in the job definition.
2. **List file** — a plain text file (one pattern per line, `#` comments allowed) at a path reachable by the container (local bind mount or a path on a configured target, referenced as `target://<target-id>/path/to/list.txt`).
3. **JSON file + key** — a JSON file (same reachability rules) plus a user-specified key that selects the pattern list inside it. The key is a dot-path supporting array traversal, e.g.:
   - file `{"backup": {"exclude": ["*.tmp", "cache/"]}}` with key `backup.exclude`
   - file `[{"name":"media","skip":["Thumbs.db"]}]` with key `0.skip`
   The value at the key MUST be an array of strings (or a single string); anything else is a validation error surfaced at save time and re-validated at run time (the file may have changed).

   **A `*` segment** matches every value of an object or every element of an array, so a catalogue keyed by something the user cannot predict is addressable: file `{"assets": {"<hash>": {"name": "a.mov"}, …}}` with key `assets.*.name` collects every asset's name. Object fan-out is emitted in sorted key order, so the resulting pattern list — and therefore the run log — is identical between two runs over the same document.

   **The key field may hold several keys, one per line.** Blank lines are ignored, results are unioned in the order written, and duplicates are dropped. This is what lets one rule draw on several sections of the same catalogue (`tracked_repo_assets.*.name` and `untracked_repo_assets.*.name`) without configuring the same file twice. Adding one rule per key remains equivalent, because includes are OR'd; the single rule exists so the file path, `on_error` and the run-log counter are configured once.

   **Strictness differs between the two forms, deliberately.** A key with no `*` is an *assertion* that a path exists: a missing key, a non-numeric array index or a scalar where a container was expected is an error, because the alternative is a typo silently selecting nothing. A key containing `*` is a *query*: children lacking the rest of the path are skipped, non-string values are skipped, and matching nothing at all is permitted — a catalogue whose sections vary, or whose section is empty, is a normal input rather than a broken one. A wildcard makes the whole of that key lenient; the two readings are not mixed within one key. A rule that resolves to no patterns is already reported as a run event (warn, or error for a global rule), so "nothing matched" stays visible without being fatal.

   *Limitation:* a JSON object with a key literally named `*` cannot be selected, because `*` is the wildcard. No escape is provided; if such a document ever appears, add one rather than changing the wildcard. Malformed/missing file or key at run time → treated per the rule's `on_error` setting: `fail_run` (default for excludes — silently syncing files the user meant to exclude is the dangerous direction) or `ignore_rule`.

**Rule semantics:**

- Each rule is `{source: inline|listfile|jsonfile, direction: include|exclude, patterns/path/key, scope: job|target:<id>, on_error}`.
- Pattern syntax: gitignore-style globs — `*` (within segment), `**` (across segments), `?`, trailing `/` marks a directory rule (prunes the whole subtree), leading `/` anchors to the sync root; otherwise patterns match anywhere in the path. Case-insensitive by default (SMB targets are usually case-insensitive), per-rule toggle. Implement with a well-tested library (e.g., `go-gitignore`-style matcher), not hand-rolled regex.
- **Evaluation order:** (0) global exclusions are part of every chain, and being excludes they win unconditionally — which is what makes them absolute; (1) if any include rules exist, a file must match at least one include (includes define the universe; a pure-exclude chain implicitly includes everything); (2) then excludes are applied and win over includes; (3) per-target rules are evaluated on top of job-level rules for that destination only — meaning different destinations of the same job can receive different subsets.
- Per-target wildcard include/exclude (the simple case from the feature list) is just an inline rule with `scope: target:<id>`.
- The run log records, per rule, how many paths it excluded/admitted (counts, not full path dumps — with an optional debug toggle to log every filtered path).
- **Filter dry-run endpoint/UI:** given a job, show a sample of what would be included/excluded (drives a "test filters" button in the job editor).
- List/JSON files are **re-read at the start of every run** (they are live configuration, not snapshotted at save time).

---

## 7. Data model (SQLite)

```
targets(id, name, type[smb|local], host, share, subpath, username,
        password_encrypted, domain, mount_opts_override, created_at)
jobs(id, name, source_target_id, source_subpath,
     mode[mirror|update], compare[fast|content], workers, bwlimit_kbps,
     delete_policy, schedule_cron, enabled, next_run_at,      -- scheduling: Phase 5a
     unavailable_policy[prompt|skip|abort], prompt_timeout_sec, prompt_fallback[skip|abort],
     parallel_destinations, create_dest_dirs[ask|always|never],
     api_trigger_token_hash)                            -- SHA-256; NULL = webhooks disabled (§8.1)
job_destinations(id, job_id, dest_target_id, dest_subpath, position)
global_filter_rules(id, source[inline|listfile|jsonfile], patterns_json,
     file_path, json_key, case_sensitive, on_error[fail_run|ignore_rule],
     position)                                          -- applied to every job; no scope or direction by design (§6.5)
filter_rules(id, job_id, scope[job|target], scope_target_id,
     direction[include|exclude], source[inline|listfile|jsonfile],
     patterns_json, file_path, json_key, case_sensitive, on_error[fail_run|ignore_rule])
runs(id, job_id, trigger[manual|schedule|webhook], started_at, finished_at,
     status[running|waiting_prompt|success|partial|failed|cancelled],
     files_scanned, bytes_total, error_summary)
run_destinations(run_id, dest_target_id,
     status[pending|running|success|failed|skipped_unavailable|cancelled],
     files_total, files_done, files_deleted, bytes_total, bytes_done,
     started_at, finished_at, error_summary)             -- per-destination results & progress
run_events(id, run_id, ts, level[info|warn|error], dest_target_id, relpath, message)
     -- the task/error log; indexed on (run_id, level) and (ts); pruned by retention setting
webhooks(id, job_id_nullable, url, events[start|progress|prompt|complete|error],
     secret, enabled, min_interval_sec)                  -- outbound status callbacks
settings(key, value)                                     -- incl. log retention days
```

---

## 8. API surface (REST + WS)

```
POST   /api/targets              create SMB/local target
GET    /api/targets              list with live health status
POST   /api/targets/{id}/test    mount + statfs + list root, return result
DELETE /api/targets/{id}

POST   /api/jobs                 create/update job
GET    /api/jobs
POST   /api/jobs/{id}/run        body: {preview: bool}
POST   /api/jobs/{id}/confirm    confirm a previewed plan
POST   /api/jobs/{id}/filter-test  body: {sample_limit} → included/excluded sample + per-rule counts
POST   /api/runs/{id}/cancel
POST   /api/runs/{id}/prompt     body: {action: skip|retry|abort, dest_target_id}
                                 answer a target-unavailable prompt
GET    /api/runs?job_id=&status= history
GET    /api/runs/{id}            full run detail incl. per-destination status/progress/ETA
GET    /api/runs/{id}/events?level=&dest=&page=   paged task/error log
GET    /api/logs?level=&since=&job_id=&dest=     global log view across runs
GET    /api/browse?target_id=&path=   directory listing for path pickers

# Webhook endpoints (token auth, NOT session auth — see below)
POST   /api/hooks/jobs/{id}/run?token=<api_trigger_token>
                                 externally trigger a run; body optional:
                                 {preview:false, unavailable_policy_override}.
                                 Returns 202 + {run_id, status_url}. 409 if already running
                                 (or ?queue=1 to queue one pending run).
GET    /api/hooks/runs/{run_id}/status?token=...   JSON status for pollers:
                                 {status, trigger, started_at, elapsed_sec,
                                  eta_sec, bytes_done, bytes_total, throughput_bps,
                                  destinations:[{target, status, files_done, files_total,
                                                 bytes_done, bytes_total, eta_sec}],
                                  current_files:[{relpath, dest, pct, eta_sec}],
                                  errors_count, last_error}
GET    /api/hooks/jobs/{id}/status?token=...       same shape, for the job's latest run

WS     /api/ws                   events: run progress (bytes/sec, files done/total,
                                 per-file/per-destination/total ETA, current files
                                 in flight), target health changes, target-unavailable
                                 prompts, run completion
```

Auth: single admin password (env var or first-run setup), session cookie, all `/api/*` behind it **except** `/api/hooks/*`, which use per-job/per-run bearer tokens (long random strings, shown once at creation, regenerable, revocable). Rate-limit hook endpoints.

`unavailable_policy_override` accepts `skip` or `abort` only. `prompt` is refused with a 400 rather than accepted and then ignored: an unattended trigger is precisely the case where there is nobody to answer, and a run that silently parks for its whole prompt timeout is worse than a request that says why it was rejected. The override applies to that run alone and never edits the stored job — a caller that can trigger a job must not be able to reconfigure it.

**A `preview:true` webhook run parks the job until it is confirmed or times out.** Nobody answers a webhook preview, so it burns its `prompt_timeout_sec` holding the job — during which scheduled runs are skipped and manual ones are refused. Support is kept because this section offers it and "plan without executing" is a legitimate thing to ask for, but a token's power is therefore "can start this job *and* can keep it from running", which is worth knowing before handing one out.

The token may be sent either as the `?token=` query parameter shown above or as `Authorization: Bearer <token>`, and **the header is the documented form**. Both work, because integrations expect the query parameter — but a credential in a URL ends up in proxy logs, browser history and `Referer` headers, none of which are places a trigger credential should be. Tokens are stored **hashed** (SHA-256), exactly as session tokens are, so a leaked database contains nothing that can start a run.

### 8.2 Outbound status callbacks

Optionally, per job (or globally), the user configures webhook URLs that the backend POSTs to on events: `run_started`, `progress` (throttled to `min_interval_sec`, default 30s), `target_unavailable_prompt`, `run_completed`, `run_failed`. Payload = the same status JSON as the polling endpoint plus an `event` field; sign with HMAC-SHA256 of the body using the webhook's `secret` in an `X-Signature` header. Delivery: 3 attempts with backoff, failures logged to run_events (level=warn), never block or fail the sync itself. This lets the tool notify Home Assistant / n8n / a custom dashboard without polling.

**Wire formats.** A callback declares how its body is shaped, because not every receiver speaks the generic one:

- **`json`** (default) — the §8.1 status payload plus `event`, HMAC-SHA256 signed. For n8n, Home Assistant, anything custom.
- **`cn4m`** — form-encoded `app`/`message`/`level`, which is what cn4m's `/suite/status` accepts. Unsigned, because that endpoint does not check one.
- **`discord`** — a JSON `{"content": …}` body carrying one line with an emoji for the outcome, for a Discord webhook URL.

`cn4m` and `discord` are **best-effort**: a delivery failure is not written to `run_events` and never fails a sync, because a suite dashboard or a chat service being unreachable is somebody else's outage rather than a problem with the backup. `json` keeps the warn-level logging, since a custom integration that silently stops arriving is a fault worth seeing.

A `discord` callback should subscribe to `run_completed` and `run_failed` only: `progress` posts a line every `min_interval_sec` for the length of every run, which is noise in a channel people read. `CN4M_CASCADE_DISCORD_WEBHOOK` seeds one on a **fresh** database and never overwrites, exactly as `CN4M_CASCADE_STATUS_URL` does for cn4m — an environment variable sets what a new installation starts with, and after that the row belongs to whoever runs it. **The URL is a credential** and is never logged.

---

## 9. Frontend (React SPA)

Pages:
1. **Dashboard** — job cards with last-run status, next scheduled run *(Phase 5)*, live progress bars for running jobs (via WS) showing per-destination and total progress + ETA, aggregate throughput graph for active runs *(Phase 6)*.
2. **Targets** — list with health dots; add/edit modal with a **type selector**: an *SMB share* (name, IP, share, subpath, credentials, advanced mount options) or a *local folder* (name, absolute path inside the container, subpath — no credentials, no mount). Either type may be a job's source or a destination. A "test connection" button shows real mount errors; it acts on a saved target, so it sits on the target list rather than inside the modal.
3. **Job editor** — pick source target + subpath and **one or more destinations** (each with its own subpath), mode, compare method, workers, bandwidth limit *(Phase 6)*, schedule *(Phase 5)*, preview-before-run toggle, unavailable-destination policy, and a **Filters tab**: add/edit filter rules of all three source types (inline patterns, list file, JSON file + key), scoped to the job or to a specific destination, with a "Test filters" button (calls `/api/jobs/{id}/filter-test`) showing a live included/excluded sample. Also a **Webhooks/API tab** *(Phase 5)*: issue, **regenerate** and revoke the trigger token (with copy-paste `curl` examples), and configure outbound callback URLs and event subscriptions. The token is displayed **once, at the moment it is issued**, and never again — §8.1 stores only its hash, so there is nothing left to show. This section previously said "view/regenerate", which the storage rule made impossible; settled 2026-09-06 in favour of §8.1, because a token that can be redisplayed is one a leaked database also hands over.
4. **Run detail** — live or historical: per-destination panels (status, progress bar, ETA, throughput), currently-copying files with per-file progress, action plan, paged task log with level/destination filters and an errors-only toggle. When a `target_unavailable` prompt is active, a blocking modal with Skip / Retry / Abort and a visible countdown to the fallback action.
5. **Logs** — global view across all runs (`/api/logs`), filter by level/job/**destination**/date, and a persistent error log view; retention configurable in settings *(Phase 6)*. The destination filter is what makes a target's history readable at all: its trouble is spread one event at a time across every job that writes to it, so no per-run view can gather it.

Two further screens exist that this list does not name, both reached from the nav: a **Jobs** index
(the dashboard's cards show a job's *latest* run, but creating, duplicating and deleting jobs needs
a list of its own) and a **Runs** list (every run regardless of job — the only way to reach a run
that is not its job's most recent, short of knowing its id). See PROGRESS.md D-74 and D-86.

Keep the UI dependency-light; polling fallback if WS drops.

> **This section describes the finished UI, which spans three phases.** Items marked with a phase in
> italics need a backend that does not exist in Phase 4 — the scheduler and webhooks arrive in
> Phase 5, bandwidth limiting and the throughput graph in Phase 6. Phase 4 builds everything else.
> The parenthetical markers exist so that the difference is visible here rather than rediscovered
> against §11.

---

## 10. Docker packaging

- Multi-stage Dockerfile: Node stage builds the SPA → Go stage embeds it and builds a static binary → final stage on **alpine** with `cifs-utils` **and `ca-certificates`** installed. Final image well under 100 MB — **25 MB** as built.
- **Alpine, decided by measurement.** This section originally specified `debian:bookworm-slim` on the grounds of CIFS parity with the development container. That target was unreachable: Debian measured 158 MB, with the slim base alone at **97.2 MB on arm64**, so "well under 100 MB" could not be met before installing a single package. The parity argument was also weaker than it read — the binary is static Go with `CGO_ENABLED=0`, so musl versus glibc never reaches it, and the CIFS behaviour this project depends on (the dialect ladder, multichannel fallback, the uninterruptible-syscall hangs of §5) is the *kernel's*, which is the host's either way. `mount.cifs` is the same upstream `cifs-utils` on both.
- That is still an argument rather than evidence, which is why §11's 6a exit criteria require the shipping image to **mount a real share** rather than merely to start: `make verify-image` mounts, lists, writes and unmounts against a live Samba server, and is a repeatable target rather than a one-off check.
- **`ca-certificates` is load-bearing**, not hygiene: outbound callbacks (§8.2) POST to arbitrary `https://` URLs and fail every one of them without a CA bundle.
- **No fixed `GOARCH`.** The image builds for whatever the host is, so Windows and macOS both build natively. `CGO_ENABLED=0` and a pure-Go SQLite driver make that free. Multi-arch via buildx is available for *publishing* a prebuilt image and is not needed to run one.

```yaml
# docker-compose.yml
services:
  cn4m-cascade:
    image: cn4m-cascade:latest
    # Bridge networking with a published port, NOT network_mode: host.
    #
    # host mode is Linux-only. On Docker Desktop — Windows and macOS, which is
    # where this actually runs — the container lives in a LinuxKit or WSL2 VM,
    # so "host" means *the VM* and the UI would not be reachable from the
    # user's browser at all. On a Linux host, `network_mode: host` remains a
    # legitimate swap for the performance §3 wanted; nothing else changes.
    ports: ["2649:2649"]
    # Makes `host.docker.internal` resolve on Linux too, so one status URL
    # works on every platform.
    extra_hosts: ["host.docker.internal:host-gateway"]
    cap_add: [SYS_ADMIN, DAC_READ_SEARCH]
    security_opt: [apparmor:unconfined]   # needed for mount on some hosts
    environment:
      - ENCRYPTION_KEY=${ENCRYPTION_KEY}
      - CN4M_CASCADE_ADMIN_PASSWORD=${CN4M_CASCADE_ADMIN_PASSWORD}
      - LISTEN_ADDR=:2649
      - TZ=${TZ:-UTC}                     # cron schedules are read in this zone (§11, Phase 5a)
      # localhost inside a bridge-networked container is the container itself,
      # so the code's bare-metal default would post to itself.
      - CN4M_CASCADE_STATUS_URL=${CN4M_CASCADE_STATUS_URL:-http://host.docker.internal:2640/suite/status}
    volumes:
      - ./data:/data                      # sqlite + logs
      - ${CN4M_CASCADE_LOCAL_DIR:-./local}:/mnt/local:rw   # optional local targets
    # Docker SIGKILLs 10s after SIGTERM by default; §10's own graceful shutdown
    # budget is 30s, because lazy-unmounting a dead share is the slow case.
    # Without this the container is killed two-thirds through the cleanup this
    # section requires.
    stop_grace_period: 45s
    healthcheck:
      test: ["CMD", "/usr/local/bin/cn4m-cascade", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    restart: unless-stopped
```

- Graceful shutdown: on SIGTERM, cancel running jobs (marking them `cancelled`), flush DB, lazy-unmount all shares. See `stop_grace_period` above — the promise is only kept if the orchestrator waits.

> **Four corrections to this section, made 2026-09-09 when it was finally built.** It previously
> passed `ADMIN_PASSWORD`, which the code has never read (it reads `CN4M_CASCADE_ADMIN_PASSWORD`), so
> the compose file silently ignored the operator's password. It omitted `stop_grace_period`, which
> made its own graceful-shutdown requirement unachievable. It mandated `network_mode: host`, which
> does not work on Docker Desktop and would have made the UI unreachable on the target platform. And
> it specified a base image whose size target it could not meet — see the alpine note above. The
> first three were invisible until the file was actually run; the fourth was invisible until it was
> measured.
>
> The base-image correction was itself found late, by review: the amendment above was written in the
> same change that shipped the *opposite* base image, and both sat in the tree together for a while.
> CLAUDE.md's first rule is that this document is the source of truth and deviations are raised, not
> made silently — deviating from a paragraph written minutes earlier is the same failure, and is
> easier to miss precisely because the reasoning still feels fresh.

---

## 11. Build phases (implement in this order)

**Phase 1 — Skeleton + mount manager.** Go server, SQLite schema, targets CRUD, mount/unmount/test with all the failure handling in §5. CLI-testable before any UI exists. *Exit criteria: can add an SMB target by IP via curl, test it, see legible errors for bad creds/offline hosts, mounts clean up properly.*

**Phase 2 — Sync engine, mirror + update modes, single destination.** Concurrent scanner, differ, parallel executor with temp-file+rename copies, retries, cancellation, run logging (run_events), progress tracker with per-file/total ETA. *Exit criteria: mirror a 100k-file tree between two SMB shares correctly and rerun as a no-op; pull the network cable mid-run and get a clean failure within ~30s, no hung process; ETA visibly converges on a mixed-size tree.*

**Phase 3 — Filtering + multi-destination.** Filter chain (§6.5) with all three rule sources and per-target scoping, `filter-test` endpoint, job_destinations fan-out, availability gate with `skip`/`abort` policies (the interactive `prompt` policy lands with the UI in Phase 4), per-destination progress/ETA. *Exit criteria: a job with two destinations where one is offline completes as `partial` under `skip`; a JSON-file exclude rule with a dot-path key demonstrably prunes a subtree, and a malformed key fails the run with a clear error.*

**Phase 4 — API + UI.** Session auth, dashboard, targets page, job editor (incl. the Filters tab), run detail with live WS progress/ETA panels, target-unavailable prompt modal (completing the `prompt` policy), **the preview gate (§6.1 step 6) and its confirm endpoint**, logs page. The job editor's **Webhooks/API tab moves to Phase 5** with the trigger-token and callback endpoints it drives: there is nothing for it to talk to before then.

Phase 4 is split in two: **4a is the API**, verifiable in the Samba harness with no UI; **4b is the SPA (§9)**, built against a then-frozen API.

*Exit criteria (4a): `/api/*` is closed to unauthenticated callers, login works, logout invalidates the token server-side and a forged cookie is refused; a preview parks with a visible plan and copies nothing, and confirming executes it; an unconfirmed preview cancels and still copies nothing; a `prompt` on an unavailable destination parks that destination while a healthy one completes, with `skip` → `partial` and `abort` → `failed`; an unanswered prompt falls back and says so in the log; a WS client sees progress and completion, and a stalled client is dropped without delaying the run; `/api/browse` lists a share and refuses path escapes; `/api/logs` reads across runs.*

*Exit criteria (4b-1): the SPA shell loads without a session while `/api/*` still refuses one; a deep link returns the shell on a cold load and a missing asset returns 404; first-run setup, login, reload and logout all work in a browser; the targets screen creates, edits, deletes and tests a target, showing a real mount error for a failing one.*

*Exit criteria (4b-2): a job is created, edited and run entirely from the UI and mirrors correctly; a preview parks and its confirm view names the files it will delete before executing; a `prompt` on an unavailable destination shows a counting-down modal while a healthy destination completes; killing the WebSocket mid-run leaves progress advancing via the polling fallback; the logs page filters by level and job across more than one run.*

> **Preview mode was moved here from Phase 6.** §11 originally listed it under Phase 6 while §6.1 step 6, §8 (`POST /api/jobs/{id}/confirm`) and §9 (the "preview-before-run toggle") all specified it as part of the run pipeline and the job editor. That was a contradiction in this document, not a choice available to the implementer. It is resolved in favour of Phase 4: the preview gate is a *run-pipeline* feature whose only interface is the run-detail screen, so building it apart from that screen would mean building it twice. Phase 6 keeps the polish items that genuinely are polish.

**Phase 5 — Scheduler and webhooks.** Cron scheduling (robfig/cron), inbound trigger tokens + status endpoints, outbound signed callbacks.

Split in two: **5a is scheduling**, **5b is webhooks** (inbound tokens and outbound callbacks). 5a adds no new attack surface and no new deletion path; 5b adds an endpoint reachable without a session, so it carries its own security review. **Two-way sync was originally 5c and has been deferred indefinitely — §14.**

*Exit criteria (5a): a job with a cron expression fires at its scheduled time and records `trigger=schedule`; a disabled job never fires; a fire while that job's previous run is still going is skipped and logged rather than queued or overlapped; an invalid cron expression is refused at save with a legible message; a schedule missed while the server was down does not fire on startup and says so in the log; a scheduled run against an unavailable destination reaches a terminal state with no human input.*

*Exit criteria (5b): a valid trigger token starts a run and an invalid, revoked or absent one is refused; a second trigger while running is a 409 unless `?queue=1`; the status endpoints return the documented shape both during a run and after it; an outbound callback carries a verifiable HMAC-SHA256 signature; a callback endpoint that is down or slow is retried, logged at warn, and **does not delay or fail the sync**; `/api/hooks/*` is reachable without a session while the rest of `/api/*` still is not.*


> **Phases 5 and 6 previously had no exit criteria**, while phases 1–4 all did. CLAUDE.md makes a phase complete only when its exit criteria pass in the harness, so the two largest phases were the two that could not be closed. The criteria above were written 2026-09-05, before any Phase 5 code, so they describe what the phase must prove rather than what it happened to do.

**Phase 6 — Packaging and polish.** **The packaging of §10 first**, then log retention/pruning, bandwidth limiting, throughput graph, multichannel toggle, docs. (Preview mode moved to Phase 4 — see the note there. Portable configuration is §8's open item, tracked in PROGRESS.md.)

**Phase 6b — Native Windows deployment.** A single `.exe` that runs without Docker, because Docker Desktop costs ~5× on sustained transfer (PERFORMANCE.md) and `mount.cifs` is Linux-only. Scope: a Windows `mountmgr.Mounter` using `WNetAddConnection2`/`WNetCancelConnection2` and `GetDiskFreeSpaceEx`; platform-appropriate configuration defaults (no `MOUNT_ROOT`, `%ProgramData%` for the database); locked-destination handling per §6.2; optional Windows Service subcommands (`-service install|uninstall|start|stop`) with the foreground process remaining the default; and the packaging and docs to install it. The engine, API, SPA, store, scheduler and filter chain are **unchanged** — §4's boundary is what makes that true, and a change to any of them is a sign the port is leaking.

*Exit criteria: the `.exe` mounts a real SMB share by UNC path and completes a job to it with no `mount.cifs` present; credentials never touch disk and never appear in a log or a process argument; a destination file held open by another process is skipped, reported individually, summarised at completion, and leaves the run `partial` with no temp file behind; `-service install` produces a service that survives a reboot and logs where Windows can see it; a job definition created on the container build still runs on the native build against the same targets; and sustained throughput to a 10GbE destination measurably exceeds the containerised figure on the same host.*

Ordered deliberately: **6a is packaging**, because until it exists there is no way to deploy this at all and everything else is polish on something only a developer can run — and because running the server currently means `exec`ing into the *test* container, which has collided with the suite badly enough to need a Docker daemon restart. **6b is log retention**, the only remaining item with a real failure mode: `run_events` is the largest table in the schema, it grows without bound, and the scheduler now produces runs unattended forever. **6c is bandwidth limiting** (§6.3), which touches the copy hot path and earns its own review. **6d is the throughput graph, the multichannel toggle and docs.**

*Exit criteria (6a): the image builds on a machine with nothing installed but Docker and comes in under 100 MB; a container from that image serves the SPA and answers its healthcheck, and **mounts a real CIFS share** — which is what proves `cifs-utils` and the capabilities are right rather than merely that the binary starts; SIGTERM stops it within its grace period with any running job marked `cancelled` and its mounts released; a missing or too-short `ENCRYPTION_KEY` refuses to start rather than coming up insecure; and running the server no longer shares a container with the test harness, so `make run` and `make test-integration` cannot interfere.*

*Exit criteria (6b): events older than the configured retention are pruned and newer ones are not; retention is configurable and a fresh install has a sane default; pruning a large backlog does not block a running sync; and a run's own events survive for the life of that run regardless of retention.*

*Exit criteria (6c): a job with a bandwidth limit transfers measurably slower than the same job without one and still completes correctly; the limit is per job; and an unlimited job is not slowed by the limiter existing.*

> **§10 was previously assigned to no phase.** Every phase above is a feature phase, so the
> production Dockerfile and `docker-compose.yml` that §10 specifies were named nowhere in this list
> — an omission, not a decision. They are Phase 6 work. Until they exist the only way to run the
> server is `make run`, which execs into the *dev* container the test harness also uses; the two
> collide, and that collision has killed a running server more than once (PROGRESS.md §7a).

> **Portable configuration — exporting a setup and importing it on another installation — is
> requested but not yet specified.** It is not in this document at all, and it cannot be added
> without first settling what happens to credentials: stored passwords are encrypted under a key
> derived from `ENCRYPTION_KEY`, so ciphertext does not travel and plaintext should not. See
> PROGRESS.md §8 T-1 for the design questions; nothing should be built until they are answered.

---

## 12. Testing notes

- Integration tests should spin up a Samba container (e.g., `dperson/samba` or plain `samba` on debian) as a target — this makes SMB behavior testable in CI without real NAS hardware.
- Specifically test: mtime preservation across SMB, mtime-tolerance comparison, unicode/emoji filenames, very deep paths, >260-char paths, 0-byte files, files that vanish between scan and copy, dest volume full, server death mid-copy (kill the Samba container), and unclean container restart with mounts left behind.
- Filtering tests: include+exclude precedence, directory-prune performance (excluded subtree is never walked), per-target scoping producing different subsets on two destinations of one job, list-file with comments/blank lines, JSON key dot-paths incl. array indices, JSON file changed between runs (re-read), missing file/key honoring `on_error`, case-insensitivity on SMB.
- Multi-destination tests: one dest offline at start under each `unavailable_policy`; prompt timeout falling back correctly; dest dying mid-run marks only that destination failed (`partial` overall); cancellation mid-fan-out.
- Webhook tests: trigger token auth (valid/invalid/revoked), 409 vs queue behavior on concurrent triggers, status endpoint shape while running/after completion, outbound callback HMAC signature verification and non-blocking failure.
- ETA sanity test: on a synthetic mixed tree, total ETA error < ~20% once 25% of bytes are done.
- Benchmark harness: generate synthetic trees (a) 10 × 10 GB files, (b) 200k × 50 KB files; record MB/s and files/s at worker counts 1/4/8/16 to pick sane defaults.

## 13. Known risks / open questions for the implementer

- `cache=loose` + `actimeo` trades metadata freshness for scan speed; if a share is concurrently modified by other clients during a sync, the scan may be slightly stale. Acceptable for v1; document it.
- Lazy unmount (`umount -l`) can leave zombie mounts consuming resources; monitor and log them.
- If the host itself already mounts the same share, kernel CIFS may share the superblock and mix options; document "let the container own its mounts."
- Symlinks over SMB are messy — v1 policy: skip symlinks, log them.
- Decide at implementation time whether `preserve permissions` is meaningful (SMB + uid/gid mapping usually makes this moot; default to not trying).

---

## 14. Deferred — someday, maybe

Work that was specified, thought through, and then deliberately not built. Kept here rather than
deleted so that a later decision to build it starts from the reasoning rather than from scratch.

### 14.1 Two-way sync

**Deferred indefinitely, 2026-09-09.** The intended deployments only ever push one direction, so
this would have been the most dangerous feature in the product built for nobody. Removed from
Phase 5, and from the UI: the job editor no longer offers the mode, and `mode: "twoway"` is refused
by validation with a message saying it is not supported rather than not yet implemented.

**Why it is the dangerous one.** Every other deletion in this system is decided by comparing two
live listings: if a file is at the destination and not at the source *right now*, it is extraneous.
Two-way deletion is decided from a **stored record of the past** — the file was here last time and
is not here now, therefore someone deleted it, therefore delete it on the other side too. That
inference is only as good as the stored state, and it fails towards data loss: a `sync_state` that
is empty, stale, partially written, or from an interrupted run makes present files look deleted.

**What was already settled, and should not be re-litigated:**

- **One destination per two-way job.** §6.4's `sync_state` row carries a single `mtime_dst`, and one
  row cannot describe a file living in three places with three different mtimes. Fan-out stays
  available for `mirror` and `update`, which are one-directional and have nothing to reconcile.
  (This resolved a real contradiction between §6.4 and the multi-destination model of §3.)
- **The first run must propagate no deletions at all.** An empty `sync_state` cannot distinguish
  "deleted since the last sync" from "never synced", and neither can an interrupted one. A first
  two-way run is a merge, not a reconciliation.
- **Conflicts are never guessed.** A file changed on both sides since the last state is marked,
  skipped, left untouched on *both* sides, and surfaced per-file with a "keep left / keep right"
  action. The rule is that a conflict is never resolved automatically: whichever side the tool
  picked would be the side the user did not want roughly half the time, and the loser is
  overwritten.

**The shape it would take:** `sync_state(job_id, relpath, size, mtime_src, mtime_dst,
last_synced_at)`, written on successful sync; deletion detection by presence in state and absence on
one side; conflict detection by both sides differing from the recorded state.

**Its exit criteria, if it is ever revived:** a file changed on one side propagates to the other and
a file deleted on one side is deleted on the other; a file changed on **both** sides is marked a
conflict, left untouched on both sides, and resolvable per-file in the UI; a `twoway` job with a
second destination is refused; and the first run of a two-way job propagates no deletions at all.

Note that CLAUDE.md's standing rule applies whatever happens here: **file identity must never be
inferred from metadata.** Size and mtime are not an identity, and acting on a false match relocates
data permanently and silently. Real identity needs this database — which is exactly why the database
is the risk rather than the remedy.

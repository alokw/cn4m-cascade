# Project Spec: Web-Based SMB Sync Engine ("FreeFileSync in a Container")

## Purpose of this document
This is a design/architecture spec intended to be handed to an AI coding assistant (or a developer) for implementation. It describes goals, architecture, technology choices, component designs, and a phased build plan. Implement in phases, in order — each phase should produce something runnable and testable.

---

## 1. Goals

- A self-hosted sync tool, functionally similar to FreeFileSync, that runs in a single Docker container.
- Web-based front-end for all configuration and monitoring (no desktop client).
- Sync **sources and destinations are primarily SMB/CIFS shares addressed by IP** (e.g. `//192.168.1.50/media`), added dynamically by the user through the UI. Local paths (bind-mounted into the container) must also work as sources/destinations.
- **Throughput is a first-class requirement.** The design should be able to saturate a gigabit link on large files and perform well on many-small-file workloads via parallelism.
- Sync modes (mirroring FreeFileSync's semantics):
  - **Mirror** — make destination exactly match source (deletes extraneous dest files).
  - **Update** — copy new/newer files to destination, never delete.
  - **Two-way** — bidirectional sync with a persisted state database to detect deletions/conflicts.
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
2. **SMB access via kernel CIFS mounts, not a userspace SMB library.** The backend mounts shares on demand at `/mnt/smb/<target-id>` using `mount.cifs`, then treats them as ordinary filesystem paths. This gets kernel-level performance (readahead, large rsize/wsize, SMB3 multichannel) and lets the entire sync engine be protocol-agnostic.
3. **`network_mode: host`** to eliminate NAT overhead and any SMB networking weirdness.
4. **Container capabilities:** `SYS_ADMIN` and `DAC_READ_SEARCH` (required for mount.cifs). Document clearly in the README that this is a privileged-ish container and why.
5. **SQLite** (via `mattn/go-sqlite3` or `modernc.org/sqlite`) for all persistence: targets, job definitions, run history, and the two-way-sync state database.
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
- **Credentials:** stored in SQLite, encrypted at rest with a key derived from an `ENCRYPTION_KEY` env var (fail startup if unset). Pass credentials to mount.cifs via a temp credentials file with 0600 perms (never on the command line — visible in `ps`), deleted immediately after mount.
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
3. **Scan** the source tree once and each available destination tree **concurrently** (bounded parallel directory walkers — e.g., 8–16 workers per tree, since SMB metadata ops are latency-bound, not bandwidth-bound). Produce trees (or sorted flat lists) of `{relpath, size, mtime, isDir, symlink info}`.
4. **Filter** the source listing through the resolved filter chain (see §6.5). Filtering happens after scan but before diff so the plan and byte totals reflect only in-scope files. Directory exclusions prune the walk itself where possible (skip descending) for speed.
5. **Diff** source vs each destination according to sync mode → one ordered action plan per destination: `[]Action{Copy, Delete, MkDir, RmDir, ConflictSkip...}` with byte totals for progress/ETA reporting.
6. **(Optional) Preview gate:** if the job is run manually with "preview" enabled, plan every destination, then send the plans to the UI and wait for confirmation before executing any of them. This is the one stage that holds the whole run. The run's status is `awaiting_confirmation` while it waits; it holds its mounts so that what is confirmed executes against what was planned, and an unconfirmed plan expires after `prompt_timeout_sec` and changes nothing.
7. **Execute** per destination with a worker pool (default 4 concurrent copy workers per destination, configurable 1–16; destinations run sequentially by default with a per-job `parallel_destinations` toggle — parallel fan-out multiplies read load on the source share). Directories created first (top-down, sequential-ish), file copies in parallel, deletions last (bottom-up).
8. **Finalize:** write per-destination and overall run summaries to DB, update two-way state DB if applicable, fire outbound status callbacks (§8.2), emit completion event.

### 6.1.1 Progress & ETA reporting

Maintained by a central per-run progress tracker, updated by copy workers and published over WS at ~2 Hz (and returned by the status API):

- **Per file in flight:** bytes done / total, current MB/s, ETA for that file.
- **Per destination:** files done / total, bytes done / total, rolling throughput (EWMA over ~15s so ETA doesn't whiplash on mixed file sizes), ETA for that destination.
- **Total run:** aggregate of all destinations (including not-yet-started ones, estimated using the current rolling throughput), overall ETA, elapsed time.
- ETAs are computed from **byte** progress, not file counts (file counts mislead badly with mixed sizes); display both anyway.
- During the scan phase (before totals are known), report files/dirs discovered per second and mark ETA as "estimating…".

### 6.2 Comparison rules

- Default: **size + mtime** (with a configurable tolerance, default 2 seconds, because SMB/FAT mtime granularity is coarse; also handle the classic DST/whole-hour offset with an optional "ignore ±1 hour" toggle like FreeFileSync has).
- Optional per-job: **content compare** (streaming hash of both sides). Warn in the UI that this reads every byte over the network.
- Preserve mtimes on copied files (`os.Chtimes` after copy) — this is essential or every subsequent run re-copies everything.

### 6.3 Copy mechanics (performance-critical)

- Copy via a large userspace buffer (1–4 MiB) `io.CopyBuffer`; on Linux this will also let the kernel use readahead on the CIFS side. Do **not** use tiny default buffers.
- Copy to a temp name in the destination directory (`.partname.<rand>.tmp`), fsync, then rename over the final name. This makes interrupted copies non-destructive and restartable.
- Retry policy per file: 3 attempts with exponential backoff (1s/5s/15s); classify errors — permission errors don't retry, I/O timeouts do.
- Per-job bandwidth limit (optional, token-bucket wrapper around the copy reader).
- Skip-on-error vs abort-on-error is a per-job setting; always record per-file failures in the run log.

### 6.4 Two-way sync state

- Per job, a SQLite table `sync_state(job_id, relpath, size, mtime_src, mtime_dst, last_synced_at)` capturing the state at last successful sync.
- Deletion detection: file present in state but missing on one side → propagate deletion to the other side.
- Conflict (changed on both sides since last state): do **not** guess. Mark as conflict, skip, surface prominently in UI with a per-file "keep left / keep right" resolution action. (FreeFileSync semantics.)

### 6.5 Filtering system

Filters decide which files/folders are in scope. Multiple filter **rules** combine into a per-job (and optionally per-target) filter chain.

**Rule sources (all produce a list of patterns):**

1. **Inline list** — patterns entered directly in the UI (one per line) or supplied as a JSON array in the job definition.
2. **List file** — a plain text file (one pattern per line, `#` comments allowed) at a path reachable by the container (local bind mount or a path on a configured target, referenced as `target://<target-id>/path/to/list.txt`).
3. **JSON file + key** — a JSON file (same reachability rules) plus a user-specified key that selects the pattern list inside it. The key is a dot-path supporting array traversal, e.g.:
   - file `{"backup": {"exclude": ["*.tmp", "cache/"]}}` with key `backup.exclude`
   - file `[{"name":"media","skip":["Thumbs.db"]}]` with key `0.skip`
   The value at the key MUST be an array of strings (or a single string); anything else is a validation error surfaced at save time and re-validated at run time (the file may have changed). Malformed/missing file or key at run time → treated per the rule's `on_error` setting: `fail_run` (default for excludes — silently syncing files the user meant to exclude is the dangerous direction) or `ignore_rule`.

**Rule semantics:**

- Each rule is `{source: inline|listfile|jsonfile, direction: include|exclude, patterns/path/key, scope: job|target:<id>, on_error}`.
- Pattern syntax: gitignore-style globs — `*` (within segment), `**` (across segments), `?`, trailing `/` marks a directory rule (prunes the whole subtree), leading `/` anchors to the sync root; otherwise patterns match anywhere in the path. Case-insensitive by default (SMB targets are usually case-insensitive), per-rule toggle. Implement with a well-tested library (e.g., `go-gitignore`-style matcher), not hand-rolled regex.
- **Evaluation order:** (1) if any include rules exist, a file must match at least one include (includes define the universe; a pure-exclude chain implicitly includes everything); (2) then excludes are applied and win over includes; (3) per-target rules are evaluated on top of job-level rules for that destination only — meaning different destinations of the same job can receive different subsets.
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
     mode[mirror|update|twoway], compare[fast|content], workers, bwlimit_kbps,
     delete_policy, schedule_cron, enabled,
     unavailable_policy[prompt|skip|abort], prompt_timeout_sec, prompt_fallback[skip|abort],
     parallel_destinations, api_trigger_token)          -- token nullable; set = webhook enabled
job_destinations(id, job_id, dest_target_id, dest_subpath, position)
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
sync_state(job_id, relpath, size, mtime_src, mtime_dst, last_synced_at)
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
GET    /api/logs?level=error&since=&job_id=       global log view across runs
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
                                 prompts, run completion, conflicts needing resolution
```

Auth: single admin password (env var or first-run setup), session cookie, all `/api/*` behind it **except** `/api/hooks/*`, which use per-job/per-run bearer tokens (long random strings, shown once at creation, regenerable, revocable). Rate-limit hook endpoints.

### 8.2 Outbound status callbacks

Optionally, per job (or globally), the user configures webhook URLs that the backend POSTs to on events: `run_started`, `progress` (throttled to `min_interval_sec`, default 30s), `target_unavailable_prompt`, `run_completed`, `run_failed`. Payload = the same status JSON as the polling endpoint plus an `event` field; sign with HMAC-SHA256 of the body using the webhook's `secret` in an `X-Signature` header. Delivery: 3 attempts with backoff, failures logged to run_events (level=warn), never block or fail the sync itself. This lets the tool notify Home Assistant / n8n / a custom dashboard without polling.

---

## 9. Frontend (React SPA)

Pages:
1. **Dashboard** — job cards with last-run status, next scheduled run, live progress bars for running jobs (via WS) showing per-destination and total progress + ETA, aggregate throughput graph for active runs.
2. **Targets** — list with health dots; add/edit modal (name, IP, share, subpath, credentials, advanced mount options, "test connection" button that shows real mount errors).
3. **Job editor** — pick source target + subpath and **one or more destinations** (each with its own subpath), mode, compare method, workers, bandwidth limit, schedule, preview-before-run toggle, unavailable-destination policy, and a **Filters tab**: add/edit filter rules of all three source types (inline patterns, list file, JSON file + key), scoped to the job or to a specific destination, with a "Test filters" button (calls `/api/jobs/{id}/filter-test`) showing a live included/excluded sample. Also a **Webhooks/API tab**: view/regenerate the trigger token (with copy-paste `curl` examples), configure outbound callback URLs and event subscriptions.
4. **Run detail** — live or historical: per-destination panels (status, progress bar, ETA, throughput), currently-copying files with per-file progress, action plan, paged task log with level/destination filters and an errors-only toggle, conflict resolution UI for two-way jobs. When a `target_unavailable` prompt is active, a blocking modal with Skip / Retry / Abort and a visible countdown to the fallback action.
5. **Logs** — global view across all runs (`/api/logs`), filter by level/job/date, and a persistent error log view; retention configurable in settings.

Keep the UI dependency-light; polling fallback if WS drops.

---

## 10. Docker packaging

- Multi-stage Dockerfile: Node stage builds the SPA → Go stage embeds it and builds a static binary → final stage on `debian:bookworm-slim` (or alpine) with `cifs-utils` installed. Final image should be well under 100 MB.
- `docker-compose.yml`:

```yaml
services:
  smbsync:
    image: smbsync:latest
    network_mode: host          # performance + SMB simplicity
    cap_add: [SYS_ADMIN, DAC_READ_SEARCH]
    security_opt: [apparmor:unconfined]   # needed for mount on some hosts
    environment:
      - ENCRYPTION_KEY=${ENCRYPTION_KEY}
      - ADMIN_PASSWORD=${ADMIN_PASSWORD}
      - LISTEN_ADDR=:8384
    volumes:
      - ./data:/data            # sqlite + logs
      - /srv/local-stuff:/mnt/local/stuff:rw   # optional local targets
    restart: unless-stopped
```

- Graceful shutdown: on SIGTERM, cancel running jobs (marking them `cancelled`), flush DB, lazy-unmount all shares.

---

## 11. Build phases (implement in this order)

**Phase 1 — Skeleton + mount manager.** Go server, SQLite schema, targets CRUD, mount/unmount/test with all the failure handling in §5. CLI-testable before any UI exists. *Exit criteria: can add an SMB target by IP via curl, test it, see legible errors for bad creds/offline hosts, mounts clean up properly.*

**Phase 2 — Sync engine, mirror + update modes, single destination.** Concurrent scanner, differ, parallel executor with temp-file+rename copies, retries, cancellation, run logging (run_events), progress tracker with per-file/total ETA. *Exit criteria: mirror a 100k-file tree between two SMB shares correctly and rerun as a no-op; pull the network cable mid-run and get a clean failure within ~30s, no hung process; ETA visibly converges on a mixed-size tree.*

**Phase 3 — Filtering + multi-destination.** Filter chain (§6.5) with all three rule sources and per-target scoping, `filter-test` endpoint, job_destinations fan-out, availability gate with `skip`/`abort` policies (the interactive `prompt` policy lands with the UI in Phase 4), per-destination progress/ETA. *Exit criteria: a job with two destinations where one is offline completes as `partial` under `skip`; a JSON-file exclude rule with a dot-path key demonstrably prunes a subtree, and a malformed key fails the run with a clear error.*

**Phase 4 — API + UI.** Session auth, dashboard, targets page, job editor (incl. Filters and Webhooks/API tabs), run detail with live WS progress/ETA panels, target-unavailable prompt modal (completing the `prompt` policy), **the preview gate (§6.1 step 6) and its confirm endpoint**, logs page.

Phase 4 is split in two: **4a is the API**, verifiable in the Samba harness with no UI; **4b is the SPA (§9)**, built against a then-frozen API.

*Exit criteria (4a): `/api/*` is closed to unauthenticated callers, login works, logout invalidates the token server-side and a forged cookie is refused; a preview parks with a visible plan and copies nothing, and confirming executes it; an unconfirmed preview cancels and still copies nothing; a `prompt` on an unavailable destination parks that destination while a healthy one completes, with `skip` → `partial` and `abort` → `failed`; an unanswered prompt falls back and says so in the log; a WS client sees progress and completion, and a stalled client is dropped without delaying the run; `/api/browse` lists a share and refuses path escapes; `/api/logs` reads across runs.*

*Exit criteria (4b): every screen in §9 is reachable and driven only through the documented API.*

> **Preview mode was moved here from Phase 6.** §11 originally listed it under Phase 6 while §6.1 step 6, §8 (`POST /api/jobs/{id}/confirm`) and §9 (the "preview-before-run toggle") all specified it as part of the run pipeline and the job editor. That was a contradiction in this document, not a choice available to the implementer. It is resolved in favour of Phase 4: the preview gate is a *run-pipeline* feature whose only interface is the run-detail screen, so building it apart from that screen would mean building it twice. Phase 6 keeps the polish items that genuinely are polish.

**Phase 5 — Scheduler, webhooks, two-way sync.** Cron scheduling (robfig/cron), inbound trigger tokens + status endpoints, outbound signed callbacks, sync_state DB, deletion propagation, conflict surfacing/resolution UI.

**Phase 6 — Polish.** Bandwidth limiting, throughput graph, multichannel toggle, log retention/pruning, docs. (Preview mode moved to Phase 4 — see the note there.)

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

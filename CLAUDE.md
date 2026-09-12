# Project: SMB Sync Engine (see SPEC.md)

## Hard rules — check before every action
- SPEC.md is the source of truth. If you disagree with it or find ambiguity, STOP and raise it before implementing. Do not silently deviate.
- Implement ONE phase at a time (SPEC.md §11). Never build ahead into a later phase's features, even "while we're here."
- Architecture decisions in SPEC.md §3 are settled: Go backend, SQLite, React SPA embedded via embed.FS, and — **for the containerised deployment** — kernel CIFS mounts via `mount.cifs` with host networking. Do not propose alternatives to those.
- **The exception, added 2026-09-10: a native (non-Docker) deployment may use its platform's own SMB client** instead of `mount.cifs`, because `mount.cifs` is Linux-only and Docker Desktop costs ~5x on throughput (PERFORMANCE.md). This is scoped, not open season: the substitute must implement `mountmgr.Mounter` and hand the engine an ordinary filesystem path, so SPEC.md §4's boundary is preserved and the engine still learns nothing about SMB. Anything wider than that is still a SPEC change to raise first.
- Never put credentials on a command line or in logs. mount.cifs credentials go in a 0600 temp file, deleted after mount.
- All blocking I/O against SMB paths must be cancellable and time-bounded. Nothing may hang forever when a share dies (SPEC.md §5). Concretely: a syscall wedged in the kernel cannot be interrupted from userspace, so every such call runs on its own goroutine and the caller selects on ctx — see `engine.bounded` and `mountmgr.ExecMounter.StatFS`. Never call `os.ReadDir`, `Stat`/`Info`, `Open`, `Sync`, `Rename`, `Remove` or `MkdirAll` directly on a path that may be a share.
- Bounding a *transfer* means bounding inactivity, not total duration: a legitimate multi-gigabyte copy may take many minutes, so use a stall timeout fed by progress (see `Copier.StallTimeout`), never a deadline on the whole operation.
- Abandoning a goroutine parked in a syscall is the accepted cost of not hanging. It holds a thread until the kernel returns. Use a buffered channel so the goroutine always completes, and say so in a comment where it happens.
- Copies are temp-file + fsync + rename. Never write directly to the final destination path.
- **A destination file another process holds open is skipped, not retried** (Windows fails the rename; Linux does not). It still counts as an *error* for accounting, so mirror deletions stay blocked; but a destination whose **only** failures are locks is `success`+degraded, making the run `partial` rather than `failed` — a media server can hold a file open for days, and crying `failed` every run for that trains people to ignore the status — and every locked file is named individually plus summarised at the end. Retrying a lock wastes a run's time on a file whose owner decides when it is free.
- Deletions are the one action a re-run cannot undo. Anything that removes data is logged individually, never summarised, and is withheld whenever the source listing might not reflect reality (unreadable directories, an empty source against a non-empty destination).
- Do not infer file identity from metadata. Size and mtime are not an identity: unrelated files collide constantly, and acting on a false match (a "rename") relocates data permanently and silently. Real identity needs the `sync_state` database (SPEC.md §6.4, Phase 5).
- Preserve mtimes on copied files (os.Chtimes) — required for idempotent re-runs.

## Workflow
- Start every phase in plan mode: read SPEC.md, propose a plan against that phase's exit criteria, wait for approval.
- A phase is done ONLY when its exit criteria (SPEC.md §11) pass in the test harness. Show the passing output.
- Before declaring a phase complete, spawn a fresh-context subagent to review the diff against SPEC.md and the exit criteria.
- Update PROGRESS.md (what's done / what's next / decisions made) at the end of every session before context is cleared.

## Build & test
- Go 1.25+ (`modernc.org/sqlite` requires it; `crypto/hkdf` already needed 1.24), `golangci-lint run` must pass before any commit.
- Test harness: `docker compose -f docker-compose.test.yml up` starts two Samba containers; `make test-integration` runs against them. Build this harness FIRST (before Phase 1 code) if it doesn't exist.
- Unit tests colocated (`_test.go`); integration tests in /test tagged `//go:build integration`.
- Table-driven tests preferred. Every bug fix gets a regression test.

## Code conventions
- Standard Go project layout: cmd/, internal/ (engine, mountmgr, api, store, scheduler), web/ (SPA source).
- Errors: wrap with %w and context (which target, which path). Error strings a user will see must be legible, not Go-internals.
- No panics in library code. Contexts passed explicitly everywhere.
- Log with slog; every run_event written to DB is also logged.
- Frontend: React + Vite, minimal dependencies, no UI framework beyond a light component lib if needed.

## Scaffolding for later phases
- Phase 3 fans out to many destinations and Phase 5 adds scheduling: anything that blocks, retries or holds a lock gets multiplied. Prefer a bounded, cancellable primitive now over a fix later.
- New blocking operations belong behind a helper in the package that owns them, not inline at the call site, so the bound is impossible to forget.
- The sync engine only ever sees local root paths (SPEC.md §4). It must never learn about SMB, targets, mounts or persistence — that boundary is what keeps SFTP/S3 backends possible.

## Definition of done (every task)
1. Code compiles, lint passes, unit tests pass.
2. Relevant integration tests pass against the Samba harness.
3. No TODOs left without a tracking note in PROGRESS.md.

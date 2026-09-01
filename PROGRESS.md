# PROGRESS

Living handoff doc. Update at the end of every session (CLAUDE.md → Workflow).

**Current phase:** Phase 4 — API + UI (SPEC.md §11). **Start in plan mode.**
**Phase 3:** ✅ Complete — Filtering + multi-destination. All three exit criteria pass, the
fresh-context review is done (it found five more paths to data loss, all filter-related, all fixed
— §5c), and vet, lint, unit and integration are all green under `-race` in a single clean run.
**Phase 2:** ✅ Complete — all exit criteria met, the fresh-context review is done (it found seven
data-loss bugs, all fixed with regression tests — §5b), and the bounded-I/O hard rule is satisfied.
**Phase 1:** ✅ Complete (record retained below).
**Last updated:** 2026-09-01 (Phase 3 complete)

---

## 0a. Phase 3 status

### Built
| Package | Contents |
|---|---|
| `internal/filter` (new) | gitignore-semantics pattern compiler over `doublestar/v4`, rule chain (includes define the universe, excludes win, per-target layered on job-level), dot-path JSON key extraction, per-rule counters, degraded-chain tracking |
| `internal/runner/filters.go` (new) | resolves every rule source once per run — inline, list file, JSON file, including `target://` files read off a share through `engine.ReadFileBounded` — and partitions one compiled set into the per-destination chains and the job-scope prune chain |
| `internal/runner/filtertest.go` (new) | `filter-test` dry run: scans the source, replays the chain, returns per-rule counts and a sample of admitted paths |
| `internal/runner/progress.go` (new) | `RunSnapshot` aggregating per-destination trackers into a run-level view, with `busiestPhase()` and byte estimates for destinations that have not been planned yet |
| `internal/store` (migration `0003`) | `filter_rules` table + CRUD, `job_destinations` lifted to many rows |
| `internal/engine` | `Matcher` seam on the differ (`Admits`/`PrunesDir`/`Degraded`), filter-aware scan pruning, deletion guard extended to degraded chains, `requiredDirs` deriving mkdirs from planned copies |
| `internal/runner/runner.go` | fan-out: resolve source once, scan once, loop destinations sequentially or in parallel; per-destination outcome and status; availability gate (`skip`/`abort`) |
| `internal/api` | filter rule CRUD on jobs, `POST /api/jobs/{id}/filter-test`, per-destination progress in run detail |

### Exit criteria (SPEC.md §11)
| Criterion | Status |
|---|---|
| Two destinations, one offline, completes `partial` under `skip` | ✅ `TestOneDestinationOfflineCompletesPartial` (36.4s) |
| A JSON-file exclude rule with a dot-path key prunes a subtree | ✅ `TestJSONFilterRulePrunesASubtree` |
| A malformed key fails the run with a clear error | ✅ `TestMalformedJSONKeyFailsTheRunClearly` |

### Verification (CLAUDE.md → Definition of done)
| Gate | Result |
|---|---|
| `go build` / `go vet -tags=integration` | ✅ clean |
| `golangci-lint run` | ✅ `0 issues.` |
| Unit tests under `-race` | ✅ config, engine, filter, mountmgr, secrets, store all `ok` |
| Integration under `-race` | ✅ **29/29, `ok ... 134.616s`** |
| Fresh-context subagent review | ✅ Done; all code findings fixed (§5c) |

```
--- PASS: TestOneDestinationOfflineCompletesPartial (36.33s)   ← exit criterion
--- PASS: TestOneDestinationOfflineUnderAbort (35.91s)
--- PASS: TestFanOutToTwoDestinations (0.32s)
--- PASS: TestJSONFilterRulePrunesASubtree (0.31s)             ← exit criterion
--- PASS: TestFilteredSubtreeAtTheDestinationIsNotDeleted (0.30s)
--- PASS: TestMalformedJSONKeyFailsTheRunClearly (0.10s)       ← exit criterion
--- PASS: TestMalformedKeyWithIgnoreRulePolicy (0.31s)
--- PASS: TestPerTargetFilterScoping (0.29s)
--- PASS: TestFilterTestEndpoint (0.08s)
--- PASS: TestDroppedFilterRuleDisablesDeletions (0.31s)
--- PASS: TestIncludeRuleCopiesIntoNewDirectories (0.30s)
--- PASS: TestParallelDestinations (0.30s)
... 17 further Phase 1 and Phase 2 cases, all PASS
--- PASS: TestDestinationDisappearsMidRun (42.44s)
ok  	github.com/alokw/cn4m-cascade/test	134.616s
```

The harness was verified clean *after* the run — 0 leftover CIFS mounts, no leftover iptables rules
— which is the end-to-end proof that the cleanup fix in §6 Phase 3 item 7 actually works.

Getting to this single clean run took three attempts; the two failed ones are written up in §6
Phase 3 item 7 because both failure modes look like broken product code and are not.

---

## 0b. Phase 3 — the blocking question, resolved

**SPEC.md §6.1 step 4 says "Filter the source listing." Taken literally that destroys data in
mirror mode.** Excluding `cache/` from the *source* removes those paths from the source tree; mirror
then sees them present at the destination and absent from the source — the definition of extraneous
— and deletes them. A user adding an exclude rule to skip copying a directory would silently delete
it from their backup on the next run.

**Resolved 2026-09-01 (D-37):** the chain is applied to **both** listings, so an excluded path is
invisible to the diff entirely — never copied, never deleted, never considered. This is a deliberate
deviation from §6.1's literal wording, raised rather than assumed, and documented in the README's
*Sync modes* section: *"Adding an exclude rule to save bandwidth must never destroy what is already
backed up."* The other Phase 3 questions were settled with the recommendations as proposed:
`doublestar/v4` plus our own gitignore rule layer, job-scoped rules only for pruning the shared
source walk, `target://` rule files honouring the rule's `on_error`, `unavailable_policy` defaulting
to `skip` with `prompt` rejected until Phase 4, and `parallel_destinations` defaulting to sequential.

## 0. Phase 2 status

### Built
| Package | Contents |
|---|---|
| `internal/store` (migration `0002`) | jobs, job_destinations, runs, run_destinations, run_events + CRUD, filters, event batching, interrupted-run reconciliation |
| `internal/engine` | concurrent scanner, differ (mirror/update), copier (temp+fsync+rename, mtime preservation, retry/backoff, error classification), executor (worker pool, cancellation, on_error, delete policy), progress tracker (EWMA throughput, byte-based ETA) |
| `internal/runner` | run orchestration: resolve → scan both trees concurrently → diff → execute → finalise; buffered event log; live progress; cancellation; shutdown |
| `internal/api` | jobs CRUD, `POST /api/jobs/{id}/run`, run list/detail/events/cancel |

### Exit criteria (SPEC.md §11)
| Criterion | Status |
|---|---|
| Mirror between two SMB shares correctly | ✅ `TestMirrorBetweenSMBSharesAndRerunIsANoOp` |
| Re-run as a no-op | ✅ same test — asserts the second run plans **0** copies, which is the real proof mtimes survive SMB |
| **100k-file tree** | ✅ `make test-scale` — 100k files mirrored in **2m4s** (~800 files/s), re-run planned **0** copies |
| Cable pull → clean failure, no hung process | ✅ `TestDestinationDisappearsMidRun` |
| Cable pull **within ~30s** | ⚠️ **relaxed to ~90s** with the user's agreement — see D-25 |
| ETA converges (<20% error at 25% of bytes) | ✅ `TestETAConverges`: 0.0% steady, 2.9% lumpy. Not yet measured on a real SMB tree |

### Scale run (`make test-scale`, final — after bounded I/O and the logging changes)
```
generating 100000 files...
generated in 1m15s
mirrored 100000 files in 2m2s      (~820 files/s over SMB)
re-ran in 1s                       (0 copies, 0 deletions; see the cache caveat in §6)
--- PASS: TestScaleMirror (222.00s)
```
Wrapping every share-touching syscall in a bounded goroutine cost nothing measurable — this run is
marginally faster than the one before the change.
Now also asserts: no deletions against an empty destination, no deletions on the re-run, and
content + mtime spot-checks across sampled directories (counts alone would not notice a truncated
or misdated file).

### Verified this session
`go build` / `go vet -tags=integration` clean · `golangci-lint run` → `0 issues.` ·
unit tests `-race` green (config, engine, mountmgr, secrets, store) ·
integration `-race` green (`ok ... 70.157s`, 20 tests incl. all Phase 1 cases).

---

## 1. Verification (CLAUDE.md → Definition of done)

| Gate | Result |
|---|---|
| Kernel CIFS support (blocker B-4) | ✅ `make verify-cifs` → `CIFS OK` |
| Code compiles | ✅ `go build ./...`, `go vet -tags=integration ./...` clean |
| `golangci-lint run` | ✅ `0 issues.` |
| Unit tests (**under `-race`**) | ✅ config, mountmgr, secrets, store all `ok` |
| Integration tests vs Samba harness (**under `-race`**) | ✅ 12 tests / 15 cases, `ok ... 10.136s` |
| Exit criteria walkthrough | ✅ `make demo` → **12 passed, 0 failed** |
| Fresh-context subagent review | ✅ Done; findings triaged in §5, all code findings fixed |

**Phase 1 exit criteria (SPEC.md §11), demonstrated by `make demo`:**

```
1. Add an SMB target by IP ............ created target 2a02b1ead5aff424c1d3a128c542e614
2. Test it (mount+statfs+list root) ... HTTP 200, negotiated version recorded, fixtures listed
3. Mount held for reuse ............... 1 mount(s) held for reuse
4. Wrong password ..................... HTTP 401
   target "bad-creds": authentication failed for //172.28.0.10/private
     — check the username, password and domain
5. Share does not exist ............... HTTP 502
   target "bad-share": share //172.28.0.10/no-such-share does not exist on the
     server, or the account cannot see it
6. Offline host ....................... failed in 6s (bound: 30s)
   target "offline": cannot reach //172.28.0.99/media — the host is down, or
     SMB (445/tcp) is blocked
7. Mounts clean up .................... no mounts left under the mount root
8. Credentials ........................ none in API responses, none left on disk
```

Reproduce: `make harness-up && make verify-cifs && make test-unit && make lint && make test-integration && make demo`

## 2. What's built

**Harness** — `test/samba/` (Debian-based Samba: guest share, credentialed share, two accounts,
unicode/0-byte fixtures), `docker-compose.test.yml` (two servers on static IPs + dev container with
`SYS_ADMIN`; `172.28.0.99` left unassigned as the offline target), `Dockerfile.dev` (Go 1.25,
`cifs-utils`, prebuilt golangci-lint v2), `Makefile`, `test/wait-for-samba.sh`, `test/exit-criteria.sh`.

**Code** — `internal/config`, `internal/secrets` (AES-256-GCM + HKDF), `internal/store` (SQLite +
embedded migrations + target CRUD/validation), `internal/health`, `internal/mountmgr` (Mounter seam,
dialect ladder, multichannel fallback, refcounting, idle reaper, stale-mount watchdog, startup
hygiene, bounded shutdown), `internal/storage` (SPEC §4 verbatim), `internal/api`, `cmd/smbsync`.

## 3. What's next

Phase 4 — API + UI (SPEC.md §11). **Start in plan mode**: re-read SPEC.md §8 and §9, propose
against Phase 4's exit criteria, wait for approval.

Phase 4 inherits three things worth knowing about:
- `Server.Handler()` is still the auth seam (D-4). Session auth lands here.
- `unavailable_policy: prompt` and `delete_policy: prompt` are both **rejected at validation today**
  with a "not until Phase 4" message. They are the interactive modal, and they are the reason the
  gate was built with a policy enum rather than a boolean.
- `runner.RunSnapshot` already aggregates per-destination progress; the UI consumes it rather than
  computing anything.

*(Phase 1 → 2 handoff notes, retained:)*

Phase 2 got three things from Phase 1 worth knowing about:
- `mountmgr.Manager.Acquire` returns `(root, release, err)` — jobs hold a reference for their whole
  run, which is what the idle grace and the in-use delete refusal were built for.
- The Samba containers are killable mid-transfer, which is how "pull the network cable mid-run"
  (a Phase 2 exit criterion) gets tested.
- `storage.Storage` is the only thing the engine should see. It must never learn about SMB.

## 4. Decisions log

D-1…D-13 were approved 2026-08-31 before implementation; D-14…D-17 came out of the review.

| # | Decision | Rationale |
|---|---|---|
| D-1 | `modernc.org/sqlite` | pure Go; keeps the cgo-free binary of SPEC.md §3.1 |
| D-2 | `targets` gains `local_path`, `port`, `multichannel`, `negotiated_vers`, `updated_at` | §5 needs a multichannel toggle and the negotiated dialect; §7 has nowhere to put either |
| D-3 | Empty `username` ⇒ mount with `guest` | §7 leaves `username` nullable without saying what it means |
| D-4 | No session auth in Phase 1; `Server.Handler()` is the seam | §8 assigns the API to Phase 4 |
| D-5 | AES-256-GCM, HKDF-SHA256, random nonce, base64 | §5 says only "derived from"; decrypt failure marks one target unhealthy, never crashes startup |
| D-6 | `GET /api/targets` returns cached health; only `/test` mounts | §8's "live health status" read literally mounts every share per page load |
| D-7 | `/test` releases its reference but leaves the mount up for the grace period | a run right after a test reuses the mount |
| D-8 | Migrations create only the current phase's tables | keeps schema and code in step |
| D-9 | `nosharesock` on by default | separate TCP session per mount, so two targets on one share cannot share credentials. **Verified** by `TestSameShareCredentialsAreNotSharedBetweenTargets` |
| D-10 | A pinned `vers=` in the override disables the ladder | the user asked for a specific dialect |
| D-11 | Only `KindDialect`/`KindUnknown` walk the ladder | auth, missing-share, unreachable, timeout and permission failures are identical at every dialect; retrying makes the commonest mistakes 3× slower to report *(wording corrected after review — the code is broader than the original phrasing)* |
| D-12 | Samba image from `debian:bookworm-slim`, not `dperson/samba` | arch-native on arm64; killable for Phase 2 |
| D-13 | Test compose uses a bridge network; production keeps `network_mode: host` | host networking does not work on Docker Desktop for macOS |
| D-14 | `mount_opts_override` rejects `password`/`pass`/`credentials`/`cred`/`hard`/`sharesock` | the field could otherwise put credentials in argv, defeat `soft`, or undo D-9 — all hard-rule violations reachable from a text box |
| D-15 | A mountpoint whose mounted source ≠ the target's current UNC path is detached and remounted | a target edited under a live mount would otherwise resolve to the old server |
| D-16 | `Shutdown` and the reaper use `TryLock` and never wait on a busy target | a mutex is not cancellable; waiting would blow the SIGTERM deadline |
| D-17 | Go 1.25 toolchain | `modernc.org/sqlite` v1.57 requires ≥1.25; `crypto/hkdf` (D-5) already required ≥1.24. **See §6 — CLAUDE.md still says "Go 1.22+"** |
| D-18 | `delete_policy` (§7, previously undefined) means: what to do about mirror deletions when the run had failures — `skip_deletes` (default) or `proceed` | Extra files at the destination are corrected by the next clean run; a wrong deletion is not. Phase 4 adds `prompt` as a third value, reusing the availability-gate modal |
| D-19 | **An incomplete source scan blocks deletions unconditionally**, whatever `delete_policy` says | An unreadable source directory makes everything beneath it look extraneous at the destination. This is not a policy choice; it is how a mirror destroys data |
| D-20 | Enum columns that later phases extend (`mode`, `compare`, run `status`) carry no SQL `CHECK`; validation lives in Go | SQLite cannot alter a CHECK without rebuilding the table |
| D-21 | `job_destinations` exists from Phase 2 with one row per job | Phase 3 fan-out would otherwise need a schema rewrite |
| D-22 | Extra `jobs` columns beyond §7: `compare_tolerance_sec`, `ignore_dst_hour`, `on_error`, `log_every_file` | §6.2 and §6.3 require each; §7 has nowhere to put them |
| D-23 | A transport-level failure that exhausts retries aborts the whole run, regardless of `on_error` | SPEC.md §5: the job aborts with a clear error once the server is gone. Otherwise every queued file burns its full retry budget against a dead server and the run never ends |
| D-24 | The cable-pull test blackholes a server with `iptables` (dev container gains `NET_ADMIN`) rather than killing the container | Connections are never closed and packets simply stop, which is what a real cable pull looks like — and the only thing that exercises `soft` + `echo_interval` |
| D-25 | Default `echo_interval` lowered from SPEC.md §5's **10** to **2**, and §11's "~30s" failure budget relaxed to "ends cleanly, no hang" (approved 2026-09-01) | See the measurements below. Detection time is entirely the kernel's, so the assertion is now the guarantee rather than the timing |
| D-26 | A failed health probe is confirmed by 3 probes over ~4s before the run is aborted | D-23's escalation makes a low `echo_interval` risky: one failed probe during a brief network hiccup would otherwise abort an hours-long sync |
| D-27 | Removals that clear a path for a create run in a dedicated **unblock** stage before any mkdir or copy | Ordering them with the trailing delete pass made type conflicts permanently unresolvable (P0-4) |
| D-28 | Mirror deletions are withheld when the source scans as **empty** while the destination is not | A dropped mount and a genuine "delete everything" are indistinguishable from the listing alone, and only one is recoverable (P0-5) |
| D-29 | Destination case-insensitivity is **assumed for SMB targets** | It only ever suppresses a deletion, so assuming it is the safe direction (P0-3) |
| D-30 | Symlinks are recorded as scan entries (`IsSymlink`) rather than omitted | A destination symlink must be visible to the differ, or a mkdir/rename follows it out of the destination root (P0-7) |
| D-31 | A file that vanishes between listing and stat is `Vanished`, not a scan error | Routine on a live tree (SPEC.md §12); must not block deletions (P0-6) |
| D-32 | Update mode copies only when the source is newer, and reports every skip as a conflict | SPEC.md §1: "copy new/newer files … never delete" (P0-1, P0-2) |
| D-33 | A destination nested inside the source on the same target is rejected at validation | Each run would otherwise copy the tree into itself one level deeper (P1-12) |
| D-34 | Every share-touching syscall goes through `engine.bounded`; transfers get a **stall** timeout (inactivity), not a deadline | A syscall wedged in the kernel cannot be interrupted from userspace, so what is bounded is how long the *caller* waits. A deadline on the whole transfer would break legitimate multi-gigabyte copies. Documented in CLAUDE.md |
| D-35 | Deletions, directory removals and overwrites are **always** logged individually; only brand-new files are gated behind `log_every_file` | A deletion cannot be undone by re-running, so it must never be summarised away. A 100k-file first run would otherwise write 100k rows |
| D-36 | **Rename/move detection is deferred to Phase 5**, not implemented here | Built and reverted: size + mtime is not identity. A probe showed two unrelated 9-byte files written in the same second being planned as a move, and after a false rename the metadata *matches*, so the differ treats the wrong path as correct forever — silent and permanent. Real identity needs `sync_state` (SPEC.md §6.4). Recorded as a rule in CLAUDE.md |
| D-37 | **The filter chain applies to both listings, not just the source** (§6.1 step 4 taken literally) | An excluded path is out of scope on *both* sides: never copied, never deleted, never compared. Filtering only the source makes every excluded destination file extraneous, so adding an exclude rule to save bandwidth would delete the backup it was meant to leave alone. Approved 2026-09-01 |
| D-38 | **Mirror removes destination files the source does not have; update never removes anything** | The user's call, and it matches SPEC.md §1. Mirror warns before the first destructive run; update's extraneous files are reported as informational only. Documented in the README's *Sync modes* section |
| D-39 | A rule dropped under `ignore_rule` marks the chain **degraded**, and a degraded chain may never delete | Dropping a rule *widens* scope. In mirror mode that instantly reclassifies everything the rule was protecting as extraneous. This is the same class of hazard as D-19's incomplete scan, so it gets the same unconditional block rather than a policy knob |
| D-40 | `on_error` defaults to `fail_run` for **includes as well as excludes** | SPEC.md §6.5 asks for it on excludes only. Includes need it at least as much: dropping the only include rule leaves an *empty* include set, and an empty include set admits everything. **This corrects a rationale I gave earlier and had backwards** — I had described a dropped include as failing safe by narrowing scope; it does the opposite |
| D-41 | A destination that completes but withheld work makes the **run** `partial`, even though its own row is `success` | A destination that skipped its deletions genuinely succeeded at what it attempted; the run is the only level that can say the whole thing did less than it was asked to. Otherwise a degraded filter is invisible to the user |
| D-42 | Directories to create are derived from the **planned copies**, not from admitted directory entries | A pattern like `*.jpg` matches `photos/a.jpg` but never the directory `photos`, so an include-only chain planned copies with no `mkdir` and every one of them failed with ENOENT. Deriving the set from the copies makes the two impossible to disagree |
| D-43 | Ancestor matching applies to **every** pattern, not only those written with a trailing slash | `PrunesDir` and `Admits` are two views of one decision. When only trailing-slash patterns matched ancestors, `cache` was pruned from the walk while `cache/x.bin` was still admitted by the diff — one side of a mirror seeing a subtree the other does not is how a mirror deletes |
| D-44 | Rule sources are resolved **once** per run and the compiled set partitioned | The chains and the prune chain each read the rule files, and a `target://` file that changed (or briefly failed) between the two reads gave the walk and the diff different scopes |
| D-45 | Each chain clones its rules so counters are per-destination | Job-scoped rules are shared by every destination; with parallel fan-out the shared counters were a real data race, and even sequentially the numbers in the run log were the sum across destinations rather than that destination's own |
| D-46 | Two destinations pointing at the same target are rejected at save time | `run_destinations` is keyed by `(run_id, dest_target_id)`, so the second insert violates the primary key. The job was creatable but could never run |
| D-47 | `unavailable_policy: abort` fires on **unavailability**, not on copy failures | `abort` is the availability gate (§6.6). Ordinary copy failures are `on_error`'s job; conflating them made one unreadable file kill every remaining destination |

## 5. Review findings and resolution

A fresh-context subagent reviewed the diff against SPEC.md §5 and the exit criteria. It confirmed
**no building ahead** (no jobs/runs/filters/scheduler/webhooks/UI code; migration creates only
`targets` and `settings`) and **no lock-ordering deadlock** (`m.mu` is always taken before `e.mu`,
never the reverse). Everything it found in the code is fixed:

| Finding | Fix | Regression test |
|---|---|---|
| Lazy unmount reused an already-expired context, so the dead-server fallback was killed instantly and the mount stayed wedged forever | fresh deadline via `context.WithoutCancel` | `TestLazyUnmountFallbackGetsAFreshDeadline` |
| A mountpoint was reused without checking it still held the target's share | `mountAt` compares the mounted source (D-15) | `TestMountpointHoldingADifferentShareIsReplaced` |
| §5's multichannel fallback was not implemented; it also poisoned the 2.1 rung | ladder is walked again with multichannel dropped | `TestMultichannelFallsBackWhenTheServerRejectsIt` |
| `mount_opts_override` was unvalidated — could put a password in argv, or set `hard` | forbidden-option validation (D-14) | `TestForbiddenMountOptionsAreRejected` |
| Unbounded `os.Stat` on a CIFS path in `SMBStorage.Resolve` | `statBounded` | covered by the subpath path |
| `Shutdown` waited on uncancellable mutexes; the reaper stalled on one busy target | `TryLock` throughout (D-16) | `TestShutdownDoesNotWaitForAnInFlightMount`, `TestShutdownWithoutStart` |
| `Forget` could orphan a mount permanently by dropping a referenced entry | refuses while referenced | `TestForgetRefusesWhileReferenced` |
| `Health()` never wrote to the health cache, so a dead share stayed green | both backends update it | — |
| Second `Shutdown` panicked on close-of-closed-channel | `sync.Once` + `started` flag | `TestShutdownIsIdempotent` |
| `make harness-up`'s readiness wait was a silent no-op (`smbclient` not installed) | `test/wait-for-samba.sh` polls 445 | — |
| No `-race`, no concurrency test | both `make test-unit` and `make test-integration` run `-race` | `TestConcurrentAcquireAndRelease` |

Also fixed: dead `entry.dir` field, unused `HostIsIP`, `err.Error() != "EOF"` → `errors.Is`,
subpath rejecting legitimate names like `a..b`, port range message, password-without-username now a
validation error, stale `Q-`/`R-` doc references renumbered to `D-`, stray `.golangci.bck.yml`.

## 5b. Phase 2 review — findings and resolution

A fresh-context subagent reviewed the Phase 2 diff against SPEC.md §6 and the exit criteria. It
confirmed **no building ahead**, and that the copy mechanics, `os.Remove`-not-`RemoveAll` deletes,
tracker mutex discipline, store layer and error wrapping are clean. It also found seven data-loss
bugs. All are fixed, each with a regression test in `internal/engine/regression_test.go`.

| # | Finding | Fix |
|---|---|---|
| P0-1 | **Update mode could delete.** The type-conflict branches sat outside the mirror guard, so a destination file where the source has a directory was removed — SPEC.md §1 says update never deletes | conflicts gated on mirror mode; update reports them instead |
| P0-2 | **Update copied older source files over newer destinations.** §1 says "new/newer" | update copies only when the source is newer than tolerance |
| P0-3 | **A case-only rename deleted the file just copied.** Source `Report.txt` / destination `report.txt` planned a copy *and* a delete; on a case-insensitive server the delete folded onto the new file. Source-side case collisions were also unguarded | case-folded destination index; the delete is suppressed and reported, colliding source paths are not both copied |
| P0-4 | **Type conflicts could never resolve.** `mkdir` ran before the delete clearing the blocking file, so every copy beneath failed — and those failures tripped the delete guard, skipping the corrective delete. Stuck identically forever | new *unblock* stage before any create; the test also asserts the re-run is a clean no-op |
| P0-5 | **A silently-empty source mirrored the destination to nothing.** D-19 fired only on scan *errors*; a dropped mount or stale `cache=loose` listing returns zero entries with no error | `deletionGuard` also blocks when the source is empty and the destination is not |
| P0-6 | **One vanished file blocked all deletions.** `de.Info()` returning `ErrNotExist` was recorded as a scan error, setting incomplete, which blocked every deletion and marked every run partial | the scanner separates `Vanished` from `Errors` |
| P0-7 | **Destination symlinks were invisible; writes escaped the root.** Symlinks were skipped on both trees, so `MkdirAll` followed a destination symlink and wrote outside it | the scanner records symlinks as entries so the differ can clear them |
| P1-9 | Bytes from failed copies stayed in the totals, so `bytes_done` could exceed `bytes_total` and drive the ETA to zero with work left | rewind on every non-success path |
| P1-10 | Data race on `ExecResult.Aborted` from the delete workers | `atomic.Bool`, matching `runCopies` |
| P1-11 | 4 retry attempts where §6.3 specifies 3 | `attemptsFor` caps at 3 |
| P1-12 | A destination nested inside the source on one target passed validation | `overlaps()` rejects containment either way |
| P2-13 | §6.1.1's per-file MB/s was missing; per-file ETA used the aggregate rate | per-file rate from the file's own elapsed time |
| P2-15 | `Shutdown` could return before a just-started run registered, leaving a row stuck `running` | `wg.Add` moved ahead of publication |
| P2-16 | One goroutine per directory, unbounded | goroutine budget; overflow walks inline |
| P2-17 | `context.Background()` in the event buffer | the run's finalisation context is passed in |
| P2-18 | Temp names added 22 chars and could exceed `NAME_MAX` | the basename component is truncated |
| P2-19 | A withheld-deletions reason was hidden when copies also failed | `classify` reports both, deletions first |
| P2-20 | The scale test compared counts only; the tree-match helper walked source→dest so it could not see extras | bidirectional comparison, content spot-checks, deletion assertions |

## 5c. Phase 3 review — findings and resolution

A fresh-context subagent reviewed the Phase 3 diff against SPEC.md §6.5/§6.6 and the exit criteria.
It confirmed **no building ahead** (no scheduler, no webhooks, no `sync_state`, no UI) and that the
availability gate, the JSON dot-path extractor and the per-destination progress aggregation are
clean. It found **five more paths to data loss**, all in the filter layer, plus a data race. All are
fixed, each with a regression test (`internal/filter/regression_test.go`,
`internal/engine/filter_test.go`, `internal/store/filters_test.go`, `test/phase3_integration_test.go`).

The pattern is the same one Phase 2 had: everything dangerous lives in the delete path, and the
mechanism is always **something that widens scope without anyone noticing**.

| # | Finding | Fix |
|---|---|---|
| P0-1 | **`ignore_rule` turned "excluded, therefore protected" into "extraneous, therefore deleted".** Dropping any rule widens the chain; in mirror mode everything that rule was protecting is immediately extraneous. Rule files may live on a share, so a moment's unavailability was enough to trigger it | a dropped rule marks the chain degraded; `deletionGuard` refuses to delete from a degraded chain, exactly as it does for an incomplete scan (D-39). The run reports `partial` with the reason (D-41) |
| P0-2 | **Include rules defaulted to `ignore_rule`, and my stated rationale for it was backwards.** I described a dropped include as failing safe by narrowing the set. It does the opposite: an empty include set admits *everything* | `fail_run` is now the default for both directions (D-40). Verified by probe: a chain with no rules admits `anything/at/all.bin` |
| P0-3 | **`PrunesDir` and `Admits` disagreed** for any directory pattern written without a trailing slash: the source walk pruned `cache/` while the diff still admitted `cache/x.bin`. One side of a mirror seeing a subtree the other does not is how a mirror deletes | ancestor matching applies to every pattern, not only trailing-slash ones (D-43) |
| P0-4 | **Rule files were read twice per run** — once for the diff chains, once for the prune chain — and the two reads could disagree, giving the walk and the diff different scopes | sources resolved once and the compiled set partitioned (D-44) |
| P0-5 | **Include rules could not create the directories their copies needed.** `*.jpg` never matches the directory `photos`, so no `mkdir` was planned while the copy of `photos/a.jpg` was; every such copy failed with ENOENT | directories derived from the planned copies (D-42) |
| P1-6 | **Data race on rule counters** in the parallel fan-out: job-scoped rules were shared by every destination. The parallel path had no test at all | per-chain rule clones (D-45); `TestParallelDestinations` runs it under `-race` |
| P1-7 | **Two destinations on one target were creatable but unrunnable** — `run_destinations` is keyed by `(run_id, dest_target_id)`, so the second insert violated the primary key. A test asserted the broken behaviour was fine | rejected at save (D-46); the test was wrong and was replaced |
| P1-8 | Per-rule counts were wrong three ways: pruning was not counted (a rule pruning 50k files reported "excluded 0"), `Explain` counted while `Admits` did not, and `filter-test` always returned zeroes | `decide()` is side-effect-free; both entry points count; `PrunesDir` counts |
| P1-9 | An empty pattern list hard-failed the run, so a legitimately empty list file killed the job | `CompileRule` accepts zero patterns; an empty include set is an empty universe, which is meaningful |
| P1-10 | `unavailable_policy: abort` fired on ordinary copy failures, so one unreadable file killed every remaining destination | `abort` is scoped to unavailability (D-47) |

**Tracked but not fixed** — see §6.

## 6. Open items for the next session

### Phase 3

1. **Rule files are not validated at save time.** SPEC.md §6.5 asks for it; today a typo in a
   `target://` path or a JSON key is only discovered when the run fails. The run-time error is
   clear (that is the exit criterion), but the feedback belongs in the editor. Phase 4's UI is the
   natural home.
2. **§6.5's optional debug toggle to log every filtered path is not implemented.** Counts are.
3. `handleFilterTest` returns 400 when the *source* is unavailable, which reads as "your request was
   malformed". Should be 502/503, matching the target-test endpoint.
4. `RunSnapshot` drops `FilesFound`/`DirsFound` from the per-destination trackers, so the run-level
   view cannot show scan totals. Harmless today because the UI does not exist yet.
5. **`ReadFileBounded` has no size cap.** A rule file pointed at a multi-gigabyte file on a share
   would be read into memory in full. Bounded in *time*, not in size.
6. Dead `p.anchored = false` assignment in `pattern.go` — no effect, but it invites the reader to
   believe there is a case where a pattern is un-anchored after the fact.
7. ~~A killed test run poisons the next one.~~ **Fixed** — `make harness-clean` (now a dependency of
   `make test-integration`) drops leftover blackholes and lazily unmounts anything under
   `/tmp/Test*`, and `blackhole()` drains stale rules before installing its own. A killed cable-pull
   test leaves both an `iptables` DROP *and* CIFS mounts that retry forever, and while the kernel is
   mid-reconnect to a server a **new** mount to it fails with error 115 — which reads exactly like a
   broken change. Worth knowing before trusting a Phase 3 integration failure.

   **The first version of that fix was itself a hang, and it is worth reading as a warning.** The
   drain loop shelled out to `iptables` with no timeout, from the cleanup of the one test whose
   premise is a wedged network. `iptables` waits on `/run/xtables.lock` indefinitely by default, and
   fork/exec can stall outright in a process whose threads are parked in uninterruptible CIFS
   syscalls — so the suite died on the 10-minute test timeout with the stack in `dropBlackholes`,
   the blackhole still installed, and the dev container unkillable in `D` state (`docker restart`
   returned *"tried to kill container, but did not receive an exit event"*, and every later `exec`
   failed in `setns`). Recovery was: flush the rule so the `soft` mounts could finally error, then
   recreate the container.

   Two lessons, both already in CLAUDE.md and both of which I broke in test code because it "isn't
   production code": **`exec.CommandContext` does not bound this** — its watchdog only arms after
   `Start` returns, and the stall was inside `forkExec`. The bound has to be on the *caller*, via
   the goroutine-plus-`select` idiom the engine uses. And a cleanup that cannot complete is worse
   than no cleanup: this one left the harness dirtier than the mess it existed to clear. The helper
   now passes `iptables -w 5`, bounds every invocation at 20s, and the cleanup fails the test loudly
   rather than silently leaving a blackhole installed. **Verified:** the clean suite in §0a ran
   `TestDestinationDisappearsMidRun` in its normal 42s, and the harness afterwards had 0 leftover
   CIFS mounts and no leftover rules.

### Phase 2

1. ~~Run the 100k-file criterion.~~ **Done** — see §0. One caveat worth a follow-up: the re-run
   scanned both 100k-entry trees in ~1s, which is only possible because `cache=loose` + `actimeo=30`
   served the metadata from the client cache moments after it was written (SPEC.md §13 acknowledges
   this trade). The *correctness* claim is unaffected — 0 copies planned is 0 copies planned — but
   that 1s is not a representative cold-cache scan time. A truer number needs an unmount/remount
   between runs, which belongs with the §12 benchmark harness in Phase 6.
2. ~~The ~30s cable-pull budget is not met.~~ **Resolved — see D-25.** Default `echo_interval`
   lowered from §5's 10 to 2, the ~30s figure relaxed, and the test now asserts the guarantee this
   layer can actually make (the run ends cleanly, no hang) rather than a kernel-governed timing.
3. **ETA convergence is unit-tested but not measured on a real SMB tree.** `TestETAConverges` drives
   the tracker directly with synthetic samples, so it validates the EWMA formula rather than the
   pipeline. Worth folding into the scale run.
6. ~~Blocking SMB I/O is not bounded.~~ **Done (D-34).** `engine.bounded` now wraps `ReadDir`,
   `Info`, `Open`, `Sync`, `Chtimes`, `Rename`, `Remove` and `MkdirAll`, and transfers get a stall
   timeout rather than a deadline. The old text is kept below for the reasoning.

   *Original:* **Blocking SMB I/O is not bounded — an unmet CLAUDE.md hard rule.** `os.ReadDir`, `de.Info`,
   `os.Open`, `File.Sync`, `os.Rename` and `os.Remove` in `internal/engine` are called directly.
   None uses the goroutine-plus-`select` wrapper Phase 1 built for `statfs`, and `copyBuffered`
   checks `ctx` only *between* chunks, so a single 4 MiB read blocked in the kernel is
   uninterruptible. Consequences: `POST /api/runs/{id}/cancel` has no bound, `Runner.Shutdown` can
   time out with goroutines parked in the kernel, and the "no hung process" criterion rests on the
   CIFS `soft` mount option rather than on our code. **Recommend fixing before Phase 3**, which
   multiplies the shares one run touches.
7. `SMBSYNC_TEST_DEST_OPTS` lets CI silently run the cable-pull criterion with non-default mount
   options. Harmless today, misleading later.
4. `job_destinations` allows exactly one row (validated in Go). Phase 3 lifts that.
5. `PATCH /api/jobs/{id}` does not exist — jobs are create/delete only so far.

### Carried from Phase 1

1. **CLAUDE.md says "Go 1.22+" but the toolchain is now 1.25** (D-17). Either update that line or
   pin older dependencies. Needs a decision — it is your file, so I left it alone.
2. ~~README still describes an unrelated project.~~ **Done** — rewritten with status, quickstart,
   command reference, teardown, configuration, a verified curl walkthrough, repo layout and
   troubleshooting, including the SPEC.md §3 privileged-container rationale. Note the old one-line
   description ("approval-aware file synchronization from a source repository") did not match
   SPEC.md; the README now follows the spec. Production `Dockerfile`/`docker-compose.yml` (SPEC.md
   §10) are still **not** Phase 1 and remain unbuilt.
3. **`GET` and `PATCH /api/targets/{id}` are not in SPEC.md §8's listed surface.** §11 Phase 1 says
   "targets CRUD", so they were implemented; flagging as an unrecorded extension of §8 rather than
   silently deviating.
4. The dialect ladder's *fallback* path has unit coverage only. The harness runs
   `server min protocol = SMB2`, so 3.1.1 always wins on the first rung and the integration suite
   never exercises a downgrade. A second Samba container pinned to SMB2.1 would close this.
5. `golangci-lint` is installed as `latest` (currently v2.13.2) rather than pinned — reproducibility
   gap worth closing before CI exists.

## 6b. Cable-pull measurements (D-25)

Detection time is dominated by the kernel deciding an SMB server is dead, which happens inside a
blocked `fsync` that userspace cannot interrupt. The engine's own contribution is already minimal:
the first transport-level error triggers a bounded `statfs` probe and aborts the run rather than
retrying every remaining file (D-23).

| `echo_interval` | measured detection |
|---|---|
| 10 (SPEC.md §5's default) | ~72s |
| 3 | ~31s isolated, ~50s under `-race` |
| 2 (**shipped default**) | ~43s, ~57s with the D-26 confirmation |
| 1 | ~22s |

Measurements are noisy — ±20s between identical runs, and 3 once measured *faster* than 2 — so the
integration test asserts the guarantee (ends cleanly within 90s) and logs the timing instead of
asserting it. Only `echo_interval=1` reliably lands under 30s, at the cost of the kernel being
quickest to declare a merely slow server dead; it stays available as a per-target
`mount_opts_override` for anyone who wants it.

## 7. Environment notes

- **No Go toolchain on the host** (macOS 14 / arm64) and `mount.cifs` is Linux-only, so every build,
  lint and test runs in the dev container. The `Makefile` targets are `docker compose` wrappers.
- **CIFS in the Docker Desktop kernel: confirmed working** (was blocker B-4).
- Docker Desktop's daemon hung for ~40 minutes during this session and needed a restart. If
  `docker` commands produce no output at all, that is the failure mode — restart Docker Desktop.
- `network_mode: host` does not work on Docker Desktop for macOS; the test compose uses a bridge.

## 8. TODOs without a home

None. Everything outstanding is in §6.

# PROGRESS

Living handoff doc. Update at the end of every session (CLAUDE.md → Workflow).

**Current phase:** Phase 4b-2 — dashboard, job editor, run detail, logs. **Start in plan mode.**
**Phase 4b-1:** ✅ **Complete.** All four exit criteria pass — the two HTTP-layer ones by test, the
two screen ones by a manual browser pass the user confirmed on 2026-09-02. The fresh-context review
is done (§5e; eight findings fixed, including a `.gitignore` regression I introduced). Vite/React/TS
project, embed + serving, the auth-boundary move, API client, WS hook with polling fallback, the
Targets screen with SMB **and local** targets, and no admin password policy. See §0-4b.
**Phase 4a:** ✅ **Complete.** All exit criteria pass — now recorded in SPEC.md §11 rather than only
here — the fresh-context review is done (§5d), and both decisions it escalated are resolved (D-59,
D-60). Session auth, preview/confirm, the interactive prompt policy, the WebSocket feed,
`/api/logs`, `/api/browse` and job update. See §0-4a.
**Phase 3:** ✅ Complete — Filtering + multi-destination. All three exit criteria pass, the
fresh-context review is done (it found five more paths to data loss, all filter-related, all fixed
— §5c), and vet, lint, unit and integration are all green under `-race` in a single clean run.
**Phase 2:** ✅ Complete — all exit criteria met, the fresh-context review is done (it found seven
data-loss bugs, all fixed with regression tests — §5b), and the bounded-I/O hard rule is satisfied.
**Phase 1:** ✅ Complete (record retained below).
**Last updated:** 2026-09-02 (Phase 4b-1 complete: review done, manual pass done)

---

## 0-4b. Phase 4b-1 status — SPA shell, serving and targets

Phase 4b is split: **4b-1** is the shell and the infrastructure that can go quietly wrong; **4b-2**
is the dashboard, job editor, run detail and logs. §9's screen list spans three phases, so the
scope was settled first — see D-61.

### Built
| Area | Contents |
|---|---|
| `web/` (new) | Vite + React 19 + TypeScript. Deps: react, react-dom, react-router-dom. No data-fetching or UI library |
| `web/embed.go` (new) | `//go:embed all:dist` + `Build()`. Sits beside the Vite output because the directive cannot reach outside its own package |
| `internal/api/spa.go` (new) | `spaHandler`/`spaFrom`: asset serving, deep-link fallback, cache headers, and a legible 503 when no build is embedded |
| `web/src/components/TargetModal.tsx` | SMB **and local** targets, with a type selector (D-64) |
| `internal/api/api.go` | **`Handler()` restructured** — the session guard is now mounted on `/api/` instead of wrapping everything |
| `web/src/api/` | hand-written types mirroring the frozen Go structs, and one `fetch` client with a typed `ApiError` carrying `status`/`code`/`kind` |
| `web/src/hooks/useEvents.ts` | one WS connection, capped backoff reconnect, `GET /api/runs` polling whenever the socket is down |
| `web/src/auth.tsx` | session bootstrap, first-run setup vs login, and a global 401 handler |
| `web/src/screens/Targets.tsx` | list with health dots, add/edit modal, delete, and a test button that renders `kind`-specific mount-error hints |
| `Dockerfile.dev`, `Makefile` | Node 22 as a build-time dependency; `web-install`/`web-build`/`web-lint`/`web-dev`; `build` and `test-integration` now depend on `web-build` |

### The one change worth reading carefully
`requireSession` used to wrap the whole mux, with `publicPath` listing the exceptions. That cannot
work once the server returns HTML: the login screen **is** the SPA, so a browser with no cookie has
to be served the shell. Rather than adding the static paths to `publicPath` — which makes "is this
public?" a question about a list someone must remember to update — the guard is now mounted on
`/api/` and the static handler sits outside it. Public by construction.

`TestStaticShellIsPublicButAPIIsNot` pins both halves: `/` returns the shell anonymously, and
`/api/targets`, `/api/jobs`, `/api/runs`, `/api/logs`, `/api/browse` and `/api/ws` all still return
401 `unauthenticated` without a cookie.

### Exit criteria (SPEC.md §11, added with this phase)
| Criterion | Status |
|---|---|
| Shell loads without a session; `/api/*` still refuses one | ✅ `TestStaticShellIsPublicButAPIIsNot` |
| Deep link returns the shell; a missing asset returns 404 | ✅ `TestSPADeepLinksAndMissingAssets`, `TestSPARouting` |
| The embedded build is the real SPA, not the placeholder | ✅ `TestEmbeddedBuildIsTheRealSPA` |
| Setup, login, reload, logout in a browser | ✅ Manual pass, confirmed by the user 2026-09-02: setup → login → targets, reload stays signed in, logout works |
| Targets: create, edit, delete, health dots, real mount error | ✅ Manual pass, confirmed by the user 2026-09-02: create, edit and delete all work, and a failing "test connection" reports the error |

### Verification (CLAUDE.md → Definition of done)
| Gate | Result |
|---|---|
| `go vet -tags=integration ./...` | ✅ clean |
| `golangci-lint run` | ✅ `0 issues.` |
| `npm run lint` (`tsc -b --noEmit`) | ✅ clean |
| `npm run build` | ✅ 48 modules, 242 kB / 78 kB gzipped |
| Unit tests under `-race` | ✅ all `ok`, including 4 new `internal/api` SPA cases |
| Integration under `-race` | ✅ **49 PASS / 0 FAIL, `ok ... 342.655s`** (43 before this phase, 6 new; 1 SKIP is `TestScaleMirror`). Re-run clean through `make` after the §5e fixes, the D-64 local-target work and the D-66 password change |
| Fresh-context subagent review | ✅ Done — see §5e. Six findings fixed, including one I introduced |
| Manual browser pass | ✅ Done by the user 2026-09-02 — both screens, all five flows |

### Open items
1. ~~The manual browser pass has not been done.~~ **Done 2026-09-02.** The user walked setup, login,
   reload, logout, and target create/edit/delete plus a failing connection test. All four 4b-1 exit
   criteria now pass. The automated tests cover the HTTP layer; this covered the screens.
2. **No CSP or security headers anywhere.** Irrelevant while the server only spoke JSON; now that it
   serves HTML, a `Content-Security-Policy`, `X-Content-Type-Options` and `X-Frame-Options` belong
   on the shell response. Not done in 4b-1 — flagged rather than skipped silently.
3. `useEvents` keeps every run it has ever seen in a Map with no eviction. Fine for a session or
   two; the dashboard in 4b-2 should bound it.
4. The dev container wedged mid-phase and the work was finished in an ad-hoc replacement — see §7a.
5. ~~`local` targets cannot be created or edited from the UI.~~ **Done 2026-09-02** at the user's
   request — see D-64. §9's "test connection" button still sits on the target *list* rather than
   inside the modal, because the frozen API only tests targets that have already been saved. That
   one is a deliberate §9 deviation, still open.

---

## 0-4a. Phase 4a status — API, auth and live events

Phase 4 was split (D-48): **4a is the API**, verifiable in the Samba harness; **4b is the SPA**,
built against a now-frozen API. §11 states no exit criteria for Phase 4 at all — it is the only
phase that does not — so a set was proposed and agreed before implementation.

### Built
| Package | Contents |
|---|---|
| `internal/store/auth.go` (new) | admin password (PBKDF2-HMAC-SHA256, stdlib), sessions keyed by token *hash*, settings accessors, expiry and purge |
| `internal/api/auth.go` (new) | `requireSession` middleware, first-run setup, login/logout/session, per-address login rate limiting |
| `internal/api/hub.go` (new) | `WS /api/ws`: run progress, prompts and completion, with slow clients dropped rather than allowed to block |
| `internal/api/browse.go` (new) | `GET /api/browse` path picker, bounded I/O, `..` rejected |
| `internal/api/logs.go` (new) | `GET /api/logs` across every run, filtered by level/job/run/time, newest first |
| `internal/runner/gate.go` (new) | the availability prompt and the preview park, both bounded |
| `internal/runner` | destination pipeline split into plan → (park) → execute **for previews only**; a normal run executes each destination inline as it is planned, so one destination's availability prompt never stalls another (D-60) |
| `internal/store` (migration `0004`) | `sessions`, `prompt_timeout_sec`, `prompt_fallback`, `(level, ts)` event index |
| `internal/api/jobs.go` | `PATCH /api/jobs/{id}`, `POST /api/jobs/{id}/confirm`, `preview` on run |
| `internal/api/runs.go` | `POST /api/runs/{id}/prompt` |

### Exit criteria (proposed and agreed — §11 states none)
| Criterion | Status |
|---|---|
| `/api/*` closed to strangers; login works; logout invalidates; forged cookies rejected | ⚠️ `TestSessionAuthGuardsTheAPI`, `TestForgedSessionCookieIsRejected` — closure, login and forgery are genuine; **logout is only proven at the store layer** (`store.TestSessions`). The API test's follow-up 401 is explained by the cookie jar dropping the cookie; it never replays the logged-out *token*. §6 Phase 4a item 7 |
| A preview parks with a plan and copies nothing; confirming executes it | ✅ `TestPreviewHoldsUntilConfirmed` |
| An unconfirmed preview cancels and still copies nothing | ✅ `TestUnconfirmedPreviewCancelsAndChangesNothing` |
| `prompt` parks the destination while the healthy one completes; skip → partial, abort → failed | ✅ `TestPromptPolicyParksAndAnswersSkip`, `TestPromptPolicyAnswersAbort`. The review found the "while the healthy one completes" clause neither asserted nor true; **both are now fixed** (D-60). The test asserts the healthy destination's files exist *while the other is still parked*, and was verified to fail against the old pipeline |
| No answer falls back and **says so** in the log | ✅ `TestUnansweredPromptFallsBackToSkip` |
| A WS client sees progress and completion; a stalled client is dropped without delaying the run | ✅ `TestWebSocketStreamsRunProgress`, `TestStalledWebSocketClientDoesNotDelayARun` |
| `/api/browse` lists a share and refuses escapes; `/api/logs` reads across runs | ✅ `TestBrowseListsAShareAndRefusesEscapes`, `TestLogsReadAcrossRuns` |

### Verification (CLAUDE.md → Definition of done)
| Gate | Result |
|---|---|
| `go vet -tags=integration ./...` | ✅ clean |
| `golangci-lint run` | ✅ `0 issues.` |
| Unit tests under `-race` | ✅ config, engine, filter, mountmgr, secrets, store all `ok` |
| Integration under `-race` | ✅ **43 PASS / 0 FAIL, `ok ... 326.789s`** (1 SKIP: `TestScaleMirror`, which only runs under `make test-scale`) |
| Fresh-context subagent review | ✅ Done — see §5d. Three findings fixed with regression tests; the rest tracked in §6 |

Re-verified after the §5d fixes **and** the D-59/D-60 changes: vet clean, lint `0 issues.`, unit
green under `-race` (now including `internal/api`, which had no test files before), integration
**43 PASS / 0 FAIL / 1 SKIP** in a single clean run, `ok ... 326.789s`.

One caveat on how that run was reached, because it cost four hours of wall time and will happen
again. **The preceding run wedged**, and it is the §6 Phase 3 item 7 failure mode exactly:
`TestDestinationDisappearsMidRun` hung with its iptables blackhole on 172.28.0.11 still installed,
so the CIFS mounts retried forever and threads parked in uninterruptible `D` state. Diagnosis:
`docker exec … echo` returns instantly while `ps` hangs, because `ps` walks `/proc` and blocks on
the D-state threads. `make harness-clean` cleared it (dropped the blackhole, lazily unmounted two
leftover mounts); `ps` still hangs afterwards because a lazy unmount cannot recall a thread already
in a syscall, but that is inert — those threads belong to the dead process, and the harness accepts
new work immediately (verified with a single-test probe before re-running). **No container rebuild
was needed**, contrary to the recovery recorded in §6 Phase 3 item 7 — clearing the rule was enough
this time. On the re-run `TestDestinationDisappearsMidRun` passed in 44.1s, so this is a flake in
that test, not a regression.

**`make test-integration` now passes `-timeout 15m`.** Go's default 10m timeout should have fired
on the hang and did not — the process was wedged below the point where its watchdog can act — so
the run parked indefinitely and produced no diagnostics at all. An explicit timeout at least yields
a goroutine dump next time. The flake itself is unfixed: §6 Phase 4a item 15.

```
--- PASS: TestSessionAuthGuardsTheAPI (3.83s)
--- PASS: TestForgedSessionCookieIsRejected (1.32s)
--- PASS: TestPreviewHoldsUntilConfirmed (1.69s)
--- PASS: TestUnconfirmedPreviewCancelsAndChangesNothing (6.57s)
--- PASS: TestConfirmWithoutAPreviewIsRejected (1.38s)
--- PASS: TestPromptPolicyParksAndAnswersSkip (37.42s)
--- PASS: TestPromptPolicyAnswersAbort (37.31s)
--- PASS: TestUnansweredPromptFallsBackToSkip (42.18s)
--- PASS: TestWebSocketStreamsRunProgress (7.88s)
--- PASS: TestStalledWebSocketClientDoesNotDelayARun (1.58s)
--- PASS: TestBrowseListsAShareAndRefusesEscapes (1.31s)
--- PASS: TestLogsReadAcrossRuns (1.83s)
--- PASS: TestJobUpdateReplacesDestinationsAndFilters (1.61s)
... all 29 Phase 1–3 cases still PASS, now signing in first
ok  	github.com/alokw/cn4m-cascade/test	320.890s
```

Adding auth touched every existing integration test: the harness now completes first-run setup and
holds a session cookie (`newHarness` → `signIn`), with `doAnon` for the cases that must be refused.
The diff was wide but shallow, as expected.

### D-59 / D-60 — the two decisions the §5d review escalated, and how they were resolved

Both were raised to the user rather than decided in-flight, per CLAUDE.md's first hard rule
(ambiguity in SPEC.md is raised, not silently resolved). The user delegated both back with
"update SPEC.md as you recommend… same with the README and how best to handle the prompt."

**D-59 — Preview mode belonged to Phase 6 in §11, and Phase 4a built it. Resolved: moved to
Phase 4, in SPEC.md.**
§11 listed *"Preview mode"* by name under Phase 6, while §6.1 step 6, §8
(`POST /api/jobs/{id}/confirm`) and §9 (the "preview-before-run toggle") all specified it as part of
the run pipeline and the job editor. That was a contradiction *in the spec*, not a choice available
to the implementer — and it should have been raised before implementation, not after. **SPEC.md §11
now resolves it in favour of Phase 4** and says why: the preview gate is a run-pipeline feature
whose only interface is the run-detail screen, so building it apart from that screen would mean
building it twice. Phase 6 keeps the items that genuinely are polish. The note is in the spec, so
the next reader sees the resolution rather than rediscovering the contradiction.

**Also fixed while there: §11 Phase 4 had no exit criteria** — the only phase without them, which is
why 4a's had to be invented and agreed ad hoc. The agreed set is now written into SPEC.md §11 as
Phase 4a's criteria, with a one-line set for 4b, so they are part of the source of truth.

**D-60 — A `prompt` on one destination stalled every destination. Resolved: fixed the code, not the
docs.**
The 4a restructure made the pipeline plan-all → park → execute-all, with the availability gate
inside the planning pass — so a `prompt` on a down destination blocked every healthy destination
behind it for up to `prompt_timeout_sec`. The alternative was to document the regression; that was
rejected. Fan-out exists precisely so that one dead server does not stop the others, D-53 and the
README already promised that behaviour, and Phase 3 had it.

**The fix inverts which case is special.** The plan→park→execute split exists *for preview*: a
preview must plan everything before a human can approve any of it. A normal run has no such need, so
`planDestinations` now takes an `execInline` callback and executes each destination the moment it is
planned — sequentially, before the next is planned; in parallel, inside that destination's own
goroutine. `execInline` is nil only for a preview, which still parks globally. Net effect: the gate
is per-destination again, and the preview hold is the one thing that stops the whole run.

**The exit-criterion test was vacuous and is now not.** `TestPromptPolicyParksAndAnswersSkip` only
checked the healthy destination *after the whole run ended*, so it passed whether that destination
copied concurrently or ten minutes later — which is how the regression got in. It now asserts A's
files are present **while B is still parked**, and re-reads B's status afterwards so the assertion
cannot pass by B having quietly resolved. **Verified against the old pipeline:** restoring
plan-all → execute-all fails it with "the healthy destination copied nothing while the unavailable
one was parked".

SPEC.md §6.1 was the root of the confusion — its numbered stages read as barriers the whole run
crosses together. It now states up front that stages 1 and 3–4 are per *run* while 2 and 5–7 are per
*destination*, that a blocked destination blocks only itself, and that the preview gate is the
single exception. §6.1 step 2's `prompt` bullet and step 6 say the same thing locally.

### Two bugs this phase found in existing code
- **The logging middleware silently broke WebSockets.** `statusRecorder` wrapped the
  `ResponseWriter` without forwarding `Hijack`, so the upgrade failed with `501` — a middleware
  breaking a protocol two layers away, with nothing in the logs to say so. It now forwards `Hijack`
  and `Flush`.
- **`Run.Terminal()` was `Status != RunRunning`.** With `awaiting_confirmation` added, a parked run
  would have been read as finished. It now enumerates the terminal statuses. The integration harness
  had the same bug in `awaitRun` and was fixed with it. **Correction (§5d):** `Active()` was added
  alongside, and this entry claimed it answers "the question Shutdown actually asks". It does not —
  `Runner.Shutdown` asks nothing of the sort, it just cancels everything in `r.active`. `Active()`
  has no callers and is dead code (§6 Phase 4a item 14).

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

### Scale run after Phase 3 (`make test-scale`)
```
generating 100000 files...
generated in 1m20s
mirrored 100000 files in 2m8s
re-ran in 2s                       (0 copies, 0 deletions)
--- PASS: TestScaleMirror (236.73s)
```
Against the Phase 2 baseline of 2m2s, and prior runs of 2m4s and 2m8s — **no measurable
regression**; the spread between identical runs is wider than the difference.

**Read that number narrowly.** The scale job has no filter rules, so `filter.NewChain(nil)` is in
play: the run pays the per-entry `Matcher` interface dispatch and the restructured fan-out loop,
which is what this measures, but it never evaluates a pattern. The cost of *actual* matching at
100k scale — particularly `**` patterns, which backtrack — is still unmeasured. Worth a scale
variant with a realistic rule set; noted in §6.

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

Phase 4b — the React SPA (SPEC.md §9). **Start in plan mode**: re-read §9, propose against the
five pages it lists, wait for approval.

The API it builds on is now complete and tested, and deliberately frozen: 4b should not need new
endpoints. Per D-49 the job editor ships **without** the Webhooks/API tab and run detail
**without** conflict resolution — both land in Phase 5 with the backends they drive.

Verification for 4b was agreed as **API-level integration tests plus a written click-through
checklist** rather than Playwright (see §11 note), so the checklist belongs in this file.

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
| D-48 | **Phase 4 is split into 4a (API) and 4b (UI)** | It is by far the largest phase and the only one §11 gives no exit criteria for. 4a is verifiable in the Samba harness; 4b then builds against a frozen, tested API rather than a moving one. Approved 2026-09-01 |
| D-49 | **§9's "Webhooks/API tab" and the two-way conflict UI are deferred to Phase 5** | §9 describes the finished product; §11 is the ordering authority, and it puts trigger tokens, outbound callbacks and two-way sync in Phase 5. Building those tabs now would mean UI in front of endpoints that do not exist |
| D-50 | **Phase 4a exit criteria were proposed and agreed**, since §11 states none | CLAUDE.md defines done as "exit criteria pass in the test harness". With none written, the phase had no definition of done at all. Listed in §0-4a |
| D-51 | An unanswered **prompt** falls back (default `skip`, per-job `prompt_fallback`); an unconfirmed **preview** cancels | Both choose the option that does less. A skipped destination is corrected by the next run; an unconfirmed plan must never execute. The asymmetry is deliberate |
| D-52 | `prompt_timeout_sec` defaults to 600, floor 5, ceiling 86400 — **SPEC.md §6.1 said 300 and now says 600** (synced 2026-09-01; the deviation was decided here but never reflected back into the spec, which is exactly the drift the "SPEC is the source of truth" rule exists to prevent) | A run may be unattended — from Phase 5 it may be started by cron with nobody watching — so "wait for a human" can never mean "wait forever" |
| D-53 | The **destination** parks on a prompt, not the run | The run stays `running` and its other destinations keep working, which is what makes fan-out useful when one server is down. A new `DestAwaitingPrompt` status carries it |
| D-54 | Password hashing is **stdlib `crypto/pbkdf2`** (SHA-256, 600k iterations), not bcrypt or argon2 | Either would add `golang.org/x/crypto` for one function; the cgo-free, dependency-light build is a stated goal (§3.1). The iteration count is stored with the hash so it can be raised later |
| D-55 | Sessions store a **SHA-256 of the token**, not the token | A stolen database must not yield live sessions — the same reasoning that encrypts target passwords (§5). A plain hash is right here where it would be wrong for a password: the token is 256 bits from `crypto/rand`, so there is no dictionary to attack |
| D-56 | The session cookie sets `Secure` **only when the request arrived over TLS** | The container is normally reached over plain HTTP on a LAN (§3 host networking, no TLS terminator). An unconditional `Secure` would make the browser discard the cookie and login would fail with nothing to see |
| D-57 | `Run.Terminal()` enumerates terminal statuses instead of `!= running`, and `Active()` was added | `awaiting_confirmation` is neither running nor finished. The old form would have called a parked run done |
| D-58 | A confirmed preview executes the **held** plan, not a fresh diff | The user agreed to a specific set of changes; re-diffing could execute something they never saw. The cost is that the plan is a snapshot, which the executor already tolerates (a file that vanished is a normal event). **Amended by the §5d review:** that reasoning covers copies but not deletes, which are not undoable — see §6 Phase 4a item 8. The trade-off stands; the exposure is now documented in the README and needs a shorter hold or a re-check on confirm |
| D-59 | **Preview mode moved from Phase 6 to Phase 4, in SPEC.md §11** | §11 assigned it to Phase 6 by name while §6.1/§8/§9 all specified it as part of the run pipeline and job editor — a contradiction in the spec. Resolved toward Phase 4 because the preview gate's only interface is the run-detail screen; building it apart from that screen means building it twice. Recorded in the spec, not just here, so the contradiction cannot be rediscovered. §11 Phase 4 also gained the exit criteria it never had |
| D-66 | **The admin password has no policy at all — any length, including empty** | Requested by the user 2026-09-02: the service usually runs on a closed network where a long password is friction rather than protection. The 8-character minimum in `store.SetAdminPassword` is gone. Three things were kept deliberately: a blank password is still a **credential** (the wrong one is still a 401, the rate limiter still applies — it is not an "auth off" switch), first-run setup still closes after first use, and the hashing is unchanged (fresh salt, 600k PBKDF2 iterations) so raising the bar later costs nothing. `TestAdminPasswordHasNoMinimumLength` and `TestBlankAndShortAdminPasswordsWork` cover it, the latter through the real setup → login → authenticated-request flow with a fresh cookie jar, because a blank password that could be set but not used would be worse than refusing it. **`weak_password` is gone from `/api/auth/setup`**: with no policy left, any error there is the database or the hash failing, so it is a 500 `setup_failed` rather than a 400 blaming the caller. The README explains when a blank password is and is not appropriate |
| D-67 | **An empty `SMBSYNC_ADMIN_PASSWORD` still means "not configured", not "no password"** | The asymmetry with D-66 is intentional. SPEC.md §10's compose file passes `ADMIN_PASSWORD=${ADMIN_PASSWORD}`, which expands to an empty string when the variable is unset on the host; treating that as a deliberate blank would turn a forgotten variable into a server anyone can sign into. Choosing no password has to be an explicit act, so it is only reachable through first-run setup |
| D-64 | **The target modal gained a type selector, so a local folder can be a source (or destination)** | Requested by the user 2026-09-02. The backend already supported it end to end — `store` validated `TargetLocal`, `storage.Provider.For` returned `LocalStorage`, and `Target.UNCPath`/`Describe` handled it — but **nothing exercised the path**: no integration test, and the UI hardcoded `type: "smb"`, so the combination had never run. Verified manually first (a local source mirrored to an SMB share, `success`, correct bytes on the share), then pinned by `TestLocalSourceMirrorsToSMB` and `TestTargetCanBeSwitchedBetweenSMBAndLocal`. **The modal sends the other type's fields as `""` rather than omitting them**, because the server validates the two as mutually exclusive and an omitted field keeps its stored value — without that, switching an existing SMB target to local is rejected with a validation error that reads as user error |
| D-65 | **`docker-compose.test.yml` publishes 8384 and 5173** | Neither was published, so the app was unreachable from a host browser and `make web-dev` could not have served anyone even once the container was healthy. The manual pass that 4b-1 depends on was impossible until this |
| D-61 | **§9's screen list scoped to what the frozen API supports; the deferred items marked in the spec** | §9 describes the finished UI across three phases — schedule picker, bandwidth limit, Webhooks/API tab, "next scheduled run", throughput graph, two-way conflict UI and retention settings all need a Phase 5 or 6 backend. §11's Phase 4 line even named the Webhooks tab, whose endpoints are Phase 5. Same shape as D-59, resolved the same way: each item now carries its owning phase in §9, and §11 moves the Webhooks tab to Phase 5. Building UI against endpoints that do not exist is building ahead |
| D-62 | **The session guard is mounted on `/api/` rather than wrapping everything** | Serving the SPA means serving HTML to a browser with no cookie — the login screen is part of the SPA. The alternative, adding the static paths to `publicPath`, makes "is this public?" depend on a list someone has to remember to update; a mount point makes it structural. `TestStaticShellIsPublicButAPIIsNot` pins both directions |
| D-63 | **Node is a build-time-only dependency, copied into `Dockerfile.dev` from `node:22-bookworm`** | The binary embeds compiled assets, so the production image ships no JavaScript toolchain (SPEC.md §10). Copying from the official image rather than apt/nodesource pins the version without adding a repository |
| D-60 | **A `prompt` parks one destination, never the run — fixed in code rather than documented away** | The 4a plan-all → park → execute-all restructure put the availability gate in the planning pass, so a prompt on a down destination stalled every healthy one for up to `prompt_timeout_sec`. Documenting that was rejected: fan-out exists so one dead server does not stop the others, and D-53, the README and the exit criterion all already promised it. `planDestinations` now executes each destination inline as it is planned; the plan/execute split is kept *only* for preview, which is the one gate that legitimately holds the whole run |
| D-59 | `PATCH /api/jobs/{id}` replaces destinations and filters wholesale, and is refused while the job runs | §8 lists only `POST /api/jobs` for "create/update"; a REST update of a job with nested children is a different operation, so this is recorded as an extension rather than a silent deviation. Children are positional and job-owned, so replace beats diff. Editing under a live diff is not something the engine is built to survive |
| D-60 | The WS hub reads the **most recent** runs each tick, not only the live ones | A short run can start and finish inside one tick. A feed watching only live runs would never mention it, so a dashboard would show a job that quietly never reported anything |
| D-61 | `github.com/coder/websocket` is the only new dependency | §3 decision 6 specifies WebSocket for live progress, so SSE was not an option despite being zero-dependency. coder/websocket is pure Go and cgo-free |

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

## 5d. Phase 4a review — findings and resolution

A fresh-context subagent reviewed the Phase 4a diff (`git diff a39a232..39d50ec`) against SPEC.md
§5/§6/§8/§9/§11, the CLAUDE.md hard rules and the agreed exit criteria.

**It broke the Phase 2/3 pattern: no finding deletes, overwrites or relocates data on its own.**
Explicitly clean, each verified rather than assumed: the deletion guards (`DeletionsBlocked`,
empty-source-vs-non-empty-dest, degraded chain, incomplete scan) are unchanged and still reach the
executor; **no new unbounded share I/O in production code** — `browse.go` uses
`ReadDirBounded`/`InfoBounded` and `gate.go` blocks only on channels and timers, never on a syscall
(the only `os.*` calls added anywhere in the diff are in `_test.go` against already-mounted
fixtures); no credential on a command line or in a log; temp-file + fsync + rename and `os.Chtimes`
untouched; no metadata-identity inference; no Phase 5 scheduler/webhook/`sync_state` code; and the
WS hub is free of deadlocks and goroutine leaks (`h.mu` is never held across a channel send or a DB
call, `client.close` is `sync.Once`-guarded, and `main.go` sequences shutdown so hijacked
connections close before the runs they watch). Auth crypto passed: PBKDF2-HMAC-SHA256 at 600k with
a stored iteration count, `subtle.ConstantTimeCompare`, 256-bit tokens stored as SHA-256, expiry
enforced at lookup, and CSRF covered by `SameSite=Lax` plus `websocket.Accept` pinning
`OriginPatterns` to `r.Host`.

The two most serious findings are **not code bugs but process/design ones**, and are recorded as
D-59 and D-60 in §0-4a and the decisions log rather than here.

### Fixed this session, each with a regression test

| # | Finding | Fix |
|---|---|---|
| P0-1 | **A job holding a parked preview could be edited, and the edit did not apply to the plan that then ran.** `handleUpdateJob`'s guard called `s.runner.Progress(id)` with a **job** ID, but `Progress` is keyed by **run** ID (`r.active[runID]`) — a job ID never appears in that map, so the branch was unreachable and the guard was a no-op. The DB backstop only matched `RunRunning`, and a parked preview is the distinct status `awaiting_confirmation`, so it did not catch it either. `UpdateJob` deletes and reinserts all `job_destinations` and `filter_rules`. **Scenario:** a preview parks a mirror plan showing "500 to delete"; the admin realises an exclude rule is missing, PATCHes the job to add `cache/`, then confirms believing the edit applies. It does not — the held plan runs and deletes the 500 files the new rule existed to protect. The `run_destinations` rows also then reference destination targets the job no longer has | new job-keyed `Runner.ActiveForJob` (`runner.go`), which covers running **and** parked runs because `byJob` is populated for a run's whole lifetime. `TestJobCannotBeEditedWhileAPreviewIsParked` — **verified to fail against the old code** (PATCH returned 200 and the edit landed), and it also asserts the guard *releases* once the park lapses |
| P1-2 | **`loginLimiter.attempts` grew without bound, pre-auth.** `blocked()` pruned an address's expired timestamps and wrote the (possibly empty) slice back, never deleting the key; `clear()` only fires on a *successful* login. An unauthenticated caller rotating source addresses leaked an entry per address forever | `delete` the key when nothing recent remains (`api/auth.go`). `internal/api/auth_test.go` (new — the package had **no test files** at all) covers the leak, the block-and-clear path, and that stale failures do not count |
| P1-3 | **The admin password write was not atomic**, so a crash mid-write locked the instance out permanently. `SetAdminPassword` issued three independent `SetSetting` calls — salt, iterations, hash. A crash after the salt write pairs the **new salt** with the **old hash**, so neither the old nor the new password verifies; `AdminPasswordSet` still returns true, so `POST /api/auth/setup` stays closed at 409, and with no password-change endpoint (§6 Phase 4a item 1) the only recovery is editing SQLite by hand | one transaction (`store/auth.go`). **No regression test:** the crash window is between two writes inside one function and is not reachable from a test without fault injection into the DB layer. Flagged rather than faked |

**Resolved after escalation** — the review's two most serious findings were process/design rather
than code, and were raised to the user before being acted on. Both are now closed: D-59 (preview's
phase placement, fixed in SPEC.md §11) and D-60 (a prompt stalling every destination, fixed in
`planDestinations` with a non-vacuous test). See §0-4a.

**Tracked but not fixed** — §6 Phase 4a items 7–14.

## 5e. Phase 4b-1 review — findings and resolution

A fresh-context subagent reviewed the (uncommitted) 4b-1 diff against SPEC.md §3/§8/§9/§10/§11, the
CLAUDE.md hard rules and the 4b-1 exit criteria.

**The auth boundary — the change this phase existed to get right — was verified clean.** The
reviewer built a standalone probe replicating the root/inner mux structure and ran 24 adversarial
paths against it under Go 1.25.14. Every path whose decoded form is under `/api/` reaches
`requireSession`, including `/api/`, `/api/bogus`, `/api/ws`, the percent-encoded `/%61pi/targets`,
and `CONNECT /api/targets`. `GET /api` 301s to `/api/` and then 401s rather than falling through to
the SPA. `//api/targets`, `/api//targets`, `/./api/targets` and `/api/auth/../targets` all redirect
to canonical and then 401. **No `/api`-prefixed request routes to the SPA handler, and no static
path hits the guard.** The `publicPath` bypass shape was chased specifically: `/api/auth/%2e%2e/targets`
does skip the guard because `publicPath` sees a decoded Path, but the inner mux matches per-segment
literals and no guarded pattern has `auth` as its second segment, so it 404s inside the mux. Not
exploitable, and unchanged from 4a. Path traversal in `spa.go` and the fresh-clone embed were also
verified clean, the latter by reproducing a clone with only `dist/.gitkeep` and compiling it.

### Fixed

| # | Finding | Fix |
|---|---|---|
| P0-1 | **I destroyed `.gitignore`.** I wrote the frontend rules with `cat >` instead of appending, dropping the whole original file — including `/data/` and `.env`. `DATA_DIR` defaults to `/data`, but a developer running `DATA_DIR=./data` produces `data/smbsync.db`, which holds every target's encrypted password, the PBKDF2 admin hash and live session token hashes; `.env` is where `ENCRYPTION_KEY` lives. `git add -A` would have staged both. **The key and the database together are the entire credential store** | original restored from `HEAD` and the frontend rules appended. Verified `.env` and `data/` are ignored again and `web/dist/.gitkeep` is still committable |
| P1-2 | **`useEvents` leaked a socket per remount under StrictMode.** `closedRef` was a component-lifetime ref, so the second mount reset it to false; the *first* socket's late `onclose` then saw itself as live, nulled `socketRef` — clobbering the reference to the still-open second socket — and opened a third that nothing could close. The backoff `setTimeout` was also never stored, so unmounting mid-backoff left a timer that would reconnect on any later mount | cancellation flag is now local to the effect run; the retry timer is stored and cleared; handlers are detached before `close()` |
| P1-3 | **The polling fallback did not advance `progress`.** It polled `GET /api/runs`, which returns bare `store.Run` — `progress` exists only on `GET /api/runs/{id}`. Phase, throughput, ETA, in-flight files and **a prompt's countdown deadline** would all sit frozen at the last WS frame while the socket was down. That is two 4b-2 exit criteria pre-broken | the poll now also fetches each non-terminal run individually |
| P1-4 | **`omitempty` does nothing for `time.Time`**, so `prompt_deadline`, `confirm_deadline` and `checked_at` are *always* serialised, as `"0001-01-01T00:00:00Z"`. The TS types marked them optional, which invites `if (d.prompt_deadline)` — true for every destination in every frame, giving a countdown from year 1. Verified directly: `{"prompt_deadline":"0001-01-01T00:00:00Z"}` | typed as required `string` with an `isZeroTime()` helper and comments at each site. The Go side is left alone: the API is frozen and `*time.Time` would be a breaking change |
| P1-5 | **`JobPayload.filters` was optional against a full-replace PATCH.** A 4b-2 editor that never opened the Filters tab, and so omitted `filters`, would silently delete every rule on the job — and a vanished exclude rule is a subtree that gets copied, or in mirror mode **deleted**, on the next run | `filters` is required in `JobPayload`, with the reasoning in the type's doc comment |
| P1-6 | `in_flight` and `TestResult.entries` are built by `append` from nil with no `omitempty`, so they arrive as `null`, not absent. Typed as optional, `in_flight!.map(...)` or an `in` check would throw | typed `T[] \| null` |
| P2-7 | **`TestSPARefusesTraversal` was near-vacuous** — it asserted only that the body lacked `root:`, never the status, so it would pass even if every missing asset fell through to the shell, which is the exact bug the asset branch exists to prevent | asserts status per case. Writing it surfaced that `%2f` is decoded into Path *before* cleaning, so that case lands outside the asset directory and correctly takes the shell fallback rather than 404ing — my first expectation was wrong and the test caught it |
| P2-8 | `make web-dev` had an **unterminated shell quote** and could never have run, while PROGRESS listed it as delivered | quote closed; `make -n` verified |

Also from the review: `make web-build` ran `npm ci` unconditionally on every integration run — now
conditional on `node_modules` being absent. And `useEvents` was **unreferenced** — 4b-1 scope per the
approved plan, but entirely unexercised — so the app shell now renders a live/polling feed indicator
that uses it.

### Tracked, not fixed
- No CSP / `X-Content-Type-Options` / `X-Frame-Options` on the shell — §0-4b open item 2.
- `TargetModal` hardcodes `type: 'smb'`, so `local` targets cannot be created from the UI, and the
  "test connection" button lives on the list rather than in the modal as §9 describes (the frozen
  API only tests *saved* targets). Recorded here as a deliberate §9 deviation rather than left
  silent — §0-4b open item 5.
- `FreeBytes` uses `omitempty` on a `uint64`, so a genuinely full share reports nothing rather than
  "0 free". Go-side, low stakes, not in a frozen-API-breaking position.
- `client.ts` does not `encodeURIComponent` ids; they are server-generated UUIDs.

## 6. Open items for the next session

### Phase 4a

0. ~~The fresh-context review has not been run.~~ **Done — see §5d.** Three findings fixed with
   regression tests; two escalated to the blocking decisions in §0-4a; the rest are items 7–13
   below. The review found **no self-contained data-loss bug**, breaking the Phase 2/3 pattern.
1. **Password change has no endpoint.** `store.SetAdminPassword` and `DeleteAllSessions` exist and
   are tested, but nothing calls them after first-run setup, so a password can only be changed by
   deleting the row. Needs a `POST /api/auth/password` that requires the current password and then
   invalidates every session. Small, and it belongs with the settings screen in 4b.
2. **A previewed run holds its mounts for up to `prompt_timeout_sec`.** That is by design — the
   confirmed plan must execute against what it planned against — but a job with a long timeout and
   nobody watching keeps a mount referenced for that whole window. Worth revisiting if it ever
   collides with the idle reaper.
3. **The WS feed is server→client only.** SPEC.md §8 lists target health changes among the events;
   only run progress, prompts and completion are broadcast. Health changes are still visible
   through `GET /api/targets`, so the dashboard can poll for those until 4b needs better.
4. **`recentRunWindow` is 50.** A burst of more than 50 runs between two one-second ticks would
   lose completion events for the oldest. Not reachable today (one run per job at a time), but it
   is an assumption worth naming before the scheduler arrives.
5. **No rate limit on `/api/auth/setup`.** It self-closes once a password exists, so the exposure
   is a single race on a brand-new instance, but it is the one auth endpoint without a limiter.
6. `sessionResponse` does not report *when* a session expires, so the UI cannot warn before it
   lapses. Cosmetic until 4b.

### Phase 4a — from the §5d review, tracked not fixed

7. **The logout exit criterion is proven at the wrong layer.** `handleLogout` sets `MaxAge:-1`, the
   harness cookie jar drops the cookie, and the follow-up 401 is fully explained by the *missing*
   cookie — the test never replays the logged-out **token**. Server-side invalidation is genuinely
   covered by `store.TestSessions`, so the behaviour is right; the claiming test is weak. The
   anonymous-access sweep also samples 6 read routes and omits `/api/ws`, `/run`, `/confirm` and
   `/prompt`. Cheap to close: keep the raw token and replay it with an explicit header.
8. **A confirmed preview executes a delete plan up to 24h stale, with no re-verification.** D-58
   chose to execute the *held* plan rather than re-diff, justified as "a file that vanished is a
   normal event". That reasoning covers copies; it does not cover deletes. `runDeletes` does a bare
   `boundedRemove(dstRoot/relpath)` — no re-stat, no mtime check, no comparison against current
   state — and every deletion guard was evaluated at plan time and is never re-checked. **Scenario:**
   09:00 a preview parks with `delete reports/2025.xlsx`; 09:30 a colleague writes a *new, wanted*
   file at that path; 10:00 the admin confirms and the held plan removes it. It was in no listing
   the admin reviewed, and a re-run cannot undo it. Fixes, cheapest first: cap the preview hold well
   below `prompt_timeout_sec`'s 86400 ceiling; re-scan the destination on confirm and re-prompt if
   the delete set changed. The exposure is now documented in the README under "Previewing a run".
   Preview stays in Phase 4 (D-59), so this item is live, not moot.
9. **`DestPlan` carries only counts, so a confirm gate over deletions shows no paths.**
   `progress.go:60-77` exposes `mkdirs / copies / deletes / rmdirs / copy_bytes` and conflict
   strings. A user confirming "412 to delete" cannot see *which* 412. SPEC.md §9 asks run detail to
   show "the action plan", and CLAUDE.md is explicit that removing data "is logged individually,
   never summarised". Needs the delete paths in the payload before 4b builds the modal.
10. **`/api/browse` has no entry cap and no aggregate bound.** Each `InfoBounded` call is
    individually bounded at 15s — the hard rule is satisfied *per call* — but the loop is not.
    **Scenario:** an admin opens the picker on a 200k-entry directory, `ReadDir` succeeds from
    `cache=loose`, then the server dies → 200k × 15s, each spawning a goroutine that holds an OS
    thread until the kernel releases it. `r.Context()` unwinds it if the browser gives up, nothing
    else does. Even healthy, the response is an uncapped 200k-element JSON array. Wants a `limit`
    (default a few thousand) plus one `context.WithTimeout` over the whole listing.
11. **An aborted preview is recorded as `cancelled`, not `failed`.** `resolveDestination`'s abort
    branch calls `h.cancel()` without setting `h.cancelled`, so `awaitConfirmation` takes
    `<-ctx.Done()`, `wasCancelled` is false, and `abandonPreview` writes `RunCancelled` with "the
    previewed plan was not confirmed". The user aborted; the record blames nobody confirming.
12. **Two gate races report success for an answer that had no effect.** (a) `waitForPrompt` returns
    on `timer.C` but `g.close(destID)` only runs after it returns; in that window `answer()` finds a
    live entry and replies `{"status":"accepted"}` — so a user answering `retry` is told it was
    accepted while the destination is being skipped. Should be the same 409 `no_such_prompt` the
    stale-modal case gets. (b) if `markConfirmed` closes the channel as the timer fires, Go picks a
    ready case at random and `abandonPreview` may run after the API already replied `200 confirmed`.
    (b) fails safe — nothing executes — but the reply is wrong.
13. **The progress flusher runs for the whole preview park.** `runner.go:287` starts it *before* the
    park at `:292`, so it ticks every second writing one `UPDATE run_destinations` per destination —
    up to 86,400 writes per destination at the maximum timeout, for counters that cannot change.
    The destinations also report `DestRunning` (set in `planOneDestination`) while the run is
    `awaiting_confirmation`, so 4b would draw a parked preview's destinations as running.
14. Nits from the review, none defects: `Run.Active()` is **dead code** — nothing calls it, and the
    §0-4a claim that it answers "the question Shutdown actually asks" is wrong (`Shutdown` just
    cancels everything in `r.active`); `startFlusher`'s doc comment says "and buffered events" but
    it only flushes progress (pre-existing); `hub.tick` issues 1+N queries per second while any
    client is connected, changed or not; `statusRecorder` forwards `Hijack`/`Flush` but has no
    `Unwrap()`, so other `http.ResponseController` users stay blocked; `handleBrowse` returns 502
    `mount_failed` when the *subpath* merely does not exist (404 would read better — same shape as
    the Phase 3 item 3 already tracked); and D-18 promised Phase 4 would add `delete_policy: prompt`
    reusing the availability modal, which was not added — conservative and fine, but the decision
    log now says something the code does not.

15. **`TestDestinationDisappearsMidRun` intermittently hangs instead of failing.** Seen once on
    2026-09-01: the test wedged with its blackhole still installed, the suite never completed, and
    nothing was logged. It passed in the three other full runs that day (44.1s, 42.4s, ~42s), so the
    rate is low, but the failure is expensive — it parks the harness until someone runs
    `make harness-clean` by hand, and until now it produced no diagnostics. `-timeout 15m` on the
    make target is a mitigation, not a fix; the next occurrence should dump goroutines and those
    stacks are the thing to read. Suspicion, unverified: the test's own cleanup races the blackhole
    it installed, so the drain loop never runs when the run wedges at the wrong moment.

### Phase 3

1. **Rule files are not validated at save time.** SPEC.md §6.5 asks for it; today a typo in a
   `target://` path or a JSON key is only discovered when the run fails. The run-time error is
   clear (that is the exit criterion), but the feedback belongs in the editor. Phase 4's UI is the
   natural home.
2. **§6.5's optional debug toggle to log every filtered path is not implemented.** Counts are.
3. `handleFilterTest` returns 400 when the *source* is unavailable, which reads as "your request was
   malformed". Should be 502/503, matching the target-test endpoint.
3b. **Pattern-matching cost at scale is unmeasured.** The post-Phase-3 scale run had no filter rules,
   so it exercised the `Matcher` dispatch but never a pattern. A `**`-heavy rule set over 100k
   entries is the case worth timing before anyone relies on filters at that size.
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

1. ~~**CLAUDE.md says "Go 1.22+" but the toolchain is now 1.25** (D-17).~~ **Done 2026-09-01** with
   the user's approval — the line now reads "Go 1.25+" and names why (`modernc.org/sqlite` requires
   it; `crypto/hkdf` already needed 1.24). Matches `go.mod`'s `go 1.25.0`.
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

## 7a. The dev container wedged mid-4b-1 (2026-09-01)

Recorded because it cost hours twice in one day and the recovery in §6 Phase 3 item 7 turned out to
be incomplete.

**Sequence.** A `TestDestinationDisappearsMidRun` hang left its iptables blackhole installed and CIFS
threads parked in uninterruptible `D` state. `make harness-clean` cleared the rule and the mounts,
and a *new* `go test` ran fine — the parked threads belong to the dead process and hold nothing a
new run needs. **But the container could no longer be replaced.** `docker compose up -d --build`
failed with *"tried to kill container, but did not receive an exit event"*, and every later `exec`
died in `setns`. `docker rm -f` could not kill it either. The D-state threads outlive the process
that made them, and only a Docker daemon restart clears them.

**Diagnosis in one line:** `docker exec <c> echo hi` returns instantly while `docker exec <c> ps`
hangs — `ps` walks `/proc` and blocks on the D-state entries.

**Recovery used.** A daemon restart was not available (unrelated containers were running), so the
phase was finished in an ad-hoc container from the same image:

```
docker run -d --name smbsync-dev-tmp \
  --cap-add SYS_ADMIN --cap-add DAC_READ_SEARCH --cap-add NET_ADMIN \
  --security-opt apparmor:unconfined \
  -e SMBSYNC_TEST_SAMBA_A=172.28.0.10 -e SMBSYNC_TEST_SAMBA_B=172.28.0.11 \
  -e SMBSYNC_TEST_OFFLINE=172.28.0.99 \
  -e ENCRYPTION_KEY=test-encryption-key-not-for-production \
  -v "$PWD":/src -v cn4m-cascade_gomodcache:/go/pkg/mod \
  -v cn4m-cascade_gobuildcache:/root/.cache/go-build \
  -w /src --network cn4m-cascade_smbnet cn4m-cascade-dev sleep infinity
```

The dev container holds no state — source is bind-mounted, caches are named volumes, test databases
are disposable — so a second one costs nothing. `make` still targets `smbsync-dev`, so the ad-hoc
container needs `docker exec` directly.

**Two corrections to §6 Phase 3 item 7:** clearing the blackhole is enough to unblock *new* work but
not to unwedge the container, and `docker restart` is not a recovery — the container cannot be
killed at all. **Restart the Docker daemon, or run a replacement container.**

## 7. Environment notes

- **No Go toolchain on the host** (macOS 14 / arm64) and `mount.cifs` is Linux-only, so every build,
  lint and test runs in the dev container. The `Makefile` targets are `docker compose` wrappers.
- **CIFS in the Docker Desktop kernel: confirmed working** (was blocker B-4).
- Docker Desktop's daemon hung for ~40 minutes during this session and needed a restart. If
  `docker` commands produce no output at all, that is the failure mode — restart Docker Desktop.
- `network_mode: host` does not work on Docker Desktop for macOS; the test compose uses a bridge.

## 8. TODOs without a home

None. Everything outstanding is in §6.

# PROGRESS

Living handoff doc. Update at the end of every session (CLAUDE.md → Workflow).

**Phase 5 is complete.** 5a scheduling (§0-5a) and 5b webhooks (§0-5b) are built, reviewed and
verified at **84 PASS / 0 FAIL / 1 SKIP**. Two-way sync — originally 5c — was **deferred
indefinitely** on 2026-09-09 (D-115, SPEC.md §14.1) and removed from the UI.
**Next:** Phase 6 — polish, plus the §10 packaging no phase owned until recently.
**Phase 4b-2b-ii:** ✅ Built, reviewed (§5j) and verified; **the manual browser pass (§5z U-1) is
the one gate outstanding**, and it closes Phase 4. See §0-4b2bii.
**Phase 4b-2b-i:** ✅ **Complete.** Jobs list, job editor with Settings and Filters tabs, path
picker, filter-test rendering. Review done (§5h, ten findings) and the browser pass confirmed by the
user 2026-09-04. Since then, on top of it: the mounter hang (D-80), global exclusions (D-82) and the
advisory rule-file check (D-85), reviewed in §5i. See §0-4b2bi.
**Phase 4b-2a:** ✅ **Complete.** The plan endpoint, withheld deletion counts, the browse cap, the
filter-test status split, and run detail with the prompt modal and confirm screen. The fresh-context
review is done (§5f, nine findings, seven fixed) and the manual browser pass is done (§5g, which
found three more). Four of five exit criteria confirmed; the WebSocket-fallback one is honestly
marked unverifiable by hand. See §0-4b2a.
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
**Last updated:** 2026-09-02 (Phase 4b-2b-i code complete; review done, manual pass pending)

---

## 0-5b. Phase 5b status — webhooks

The phase that lets other software drive this one, and **the first to expose an endpoint reachable
without a session**. Everything else under `/api/*` has sat behind `requireSession` since Phase 4a.
It is also the first outbound HTTP client: the server now POSTs to URLs a user supplies.

### Built
| Area | Contents |
|---|---|
| `internal/store/triggers.go` | issue / revoke / verify, 32 random bytes stored only as SHA-256 |
| `internal/api/hooks.go` | `/api/hooks/*` mounted **outside** the guard, per-job bearer auth, failure-only rate limiter, the §8.1 status shape |
| `internal/notify/` | HMAC-SHA256 signed callbacks, bounded client, retry with backoff, drop-not-block queue |
| `internal/runner/status.go` | `StatusPayload`, shared so §8.1 polling and §8.2 callbacks cannot drift |
| `internal/runner/runner.go` | `StartWebhook`, the `?queue=1` single slot, lifecycle notifications |
| `internal/store/webhooks.go` + `internal/api/webhooks.go` | callback CRUD; secrets encrypted, never returned |
| `web/src/components/WebhooksTab.tsx` | token issued once with curl examples, callback list |
| `migrations/0009_webhooks.sql` | `api_trigger_token_hash` + partial unique index, `webhooks` table |

### Two SPEC contradictions settled first
1. **§8.1 said tokens are "shown once at creation"; §9 said the editor can "view" them.** Impossible
   against a hash. Settled with the user in favour of §8.1: hashed, shown once, §9 amended to
   *regenerate*, and the column named `api_trigger_token_hash` so it cannot imply otherwise. The
   codebase already did exactly this for session tokens, so `hashToken` is reused rather than
   reinvented.
2. **`?token=` puts a credential in a URL**, where it reaches proxy logs, browser history and
   `Referer` headers. §8.1 specifies it and integrations expect it, so it still works — but
   `Authorization: Bearer` is now the documented form and what the curl examples show.

### Why the payload builder is shared
§8.2 says a callback carries "the same status JSON as the polling endpoint plus an `event` field".
Two builders satisfy that on the day they are written and drift the first time a field is added to
one of them — and this is the **only** shape consumed by software outside this project, where drift
is a broken integration rather than a cosmetic bug. Hence `runner.StatusPayload`, called by both.

### Verification
| Gate | Result |
|---|---|
| `golangci-lint run` | ✅ 0 issues |
| `go vet ./...` | ✅ clean |
| `make test-unit` (`-race`) | ✅ all packages, including the new `internal/notify` |
| `tsc -b` / `make web-build` | ✅ clean |
| Integration | ✅ **84 PASS / 0 FAIL / 1 SKIP**, `REAL_EXIT=0` (78 before this phase). Re-run unchanged after the §5n review fixes |
| Fixture shares + `/tmp/cn4m-mnt-*` afterwards | ✅ intact / none |
| All six §11 exit criteria | ✅ one test each |
| Fresh-context review | ✅ §5n — ten findings, all fixed; security core confirmed sound |

The two that carry the most weight:
- `TestHooksAreReachableWithoutASessionButNothingElseIs` — asserts **both** directions, because the
  failure modes are opposite and both are serious: hooks behind the guard breaks every integration
  silently, anything else outside it makes the whole API public. The mount works by ServeMux
  longest-pattern precedence, which is correct and completely invisible when reading the routing.
- `TestAHangingCallbackDoesNotDelayOrFailTheSync` — a receiver that accepts the connection and then
  never answers, which is worse than a refused one because nothing ever errors. The run finished in
  1.53s and succeeded.

### The bug the tests caught
`store.Run.FinishedAt` is a `*time.Time`, nil while a run is in flight, and `StatusPayload` called
`.IsZero()` on it — a nil dereference on the **common** path, since describing unfinished runs is
the entire purpose of that payload. It panicked the first time a test triggered a real run through
the queue. Worth noting that neither the compiler nor the linter could see it: `(*time.Time).IsZero`
is a perfectly legal method call on a nil pointer right up until it dereferences.

---

## 0-5a. Phase 5a status — cron scheduling

The feature that turns this from a copy utility into a backup tool, and the point at which runs
start happening with nobody watching. Every decision below follows from that: no run may park
waiting for an answer that will not come, and no firing may produce two runs.

### Built
| Area | Contents |
|---|---|
| `internal/scheduler/` | the package CLAUDE.md's layout has always named. A 30s tick reconciles from SQLite; `robfig/cron` parses only |
| `internal/store/schedule.go` | `ParseSchedule` (user-facing errors), `NextRun`, `RefreshNextRun`, `DueJobs`, `SetNextRun` |
| `internal/store/migrations/0008_scheduling.sql` | `schedule_cron`, `enabled`, `next_run_at`, plus a partial index matching the due query term for term |
| `internal/runner/runner.go` | `StartScheduled`, so the run log distinguishes unattended runs |
| `internal/api/schedule.go` | `POST /api/schedule/preview` — resolves an expression to its next three firings |
| `web/src/screens/JobEditor.tsx` | schedule field, enabled toggle, live `NextRunPreview` |
| `SPEC.md` §6.4, §7, §11 | twoway is single-destination; **exit criteria for 5a/5b/5c, which did not exist** |

### The two SPEC gaps this phase had to close first
1. **Phases 5 and 6 had no exit criteria** while 1–4 all did, so the two largest phases were the two
   that could not be closed under CLAUDE.md. Written before any code, so they describe what the
   phase must prove rather than what it happened to do.
2. **Two-way against fan-out was incoherent** — §6.4's single `mtime_dst` against the many
   destinations §3 has allowed since Phase 3. Settled with the user: a `twoway` job has exactly one
   destination.

### Why the database is the source of truth
No in-memory cron registry. Every tick asks SQLite which jobs are due and recomputes their next
firing. That costs one indexed query per thirty seconds and removes the whole class of bug where the
schedule someone sees is not the schedule that runs — no entry table to keep in step with the jobs
table, no `EntryID` bookkeeping on every edit, and a job changed by any route is picked up next tick.
It is the same shape as the rest of the codebase, where memory is a cache over SQLite.

### Verification
| Gate | Result |
|---|---|
| `golangci-lint run` | ✅ 0 issues |
| `go vet ./...` | ✅ clean |
| `make test-unit` (`-race`) | ✅ all packages, including the new `internal/scheduler` |
| `tsc -b` / `make web-build` | ✅ clean |
| Integration | ✅ **78 PASS / 0 FAIL / 1 SKIP** (76 before the review fixes, 70 before this phase) |
| `TestScheduledRunAppliesTheJobsOwnFilters` vs. the pre-fix code | ✅ **FAIL** — it deleted the protected file, which is how R-1 below was proved rather than argued |
| Fresh-context review | ✅ §5k — one critical, one high, seven others; all fixed |
| All six §11 exit criteria | ✅ one test each |

---

## 0-4b2bii. Phase 4b-2b-ii status — the dashboard and the global log

The last two screens of SPEC.md §9, and the end of Phase 4. Both are read-only views over endpoints
that already existed, so nothing here writes and nothing here can delete. The failure mode is the
opposite kind: **a dashboard that shows stale status is worse than one that shows none**, because it
is the screen someone opens *without* already knowing what to look for. A job that failed last night
reading "success" is how a backup goes unnoticed for a week.

### Built
| Area | Contents |
|---|---|
| `web/src/screens/Dashboard.tsx` | the new landing page: job cards with source → destinations, last-run status, live progress, blocked-run warning, Run / Preview |
| `web/src/screens/Logs.tsx` | the global log: level / job / destination / time filters kept in the URL, offset paging, auto-refresh while a run is in flight |
| `web/src/components/EventList.tsx` | log **rows**, shared — deliberately not the whole `EventLog` |
| `web/src/components/Bar.tsx` | the progress bar, extracted rather than retyped (the review's finding) |
| `internal/api/logs.go` | the `dest` parameter, named to match `/api/runs/{id}/events` |
| `internal/store/migrations/0007_run_events_dest_index.sql` | the index that parameter needs (D-87) |
| `web/src/App.tsx` | `/` → Dashboard, `/logs` → Logs; the `*` fallback moved off `/jobs` |

### Why `EventList` and not `EventLog`
The standing advice was to lift the whole component out of `RunDetail.tsx` so a second log renderer
could not drift from it. That is right about the **rows** — timestamp, level styling, path, message —
and wrong about the rest: the two views disagree on ordering (oldest-first against newest-first), on
which filters apply (this run's destinations against job, destination and time across all runs) and
on pagination. One component serving both would need a prop per difference, which is the tangle the
extraction exists to prevent. So the rows are shared and each screen owns its own fetching.

### What the 500-run page really costs
`store.RunFilter` has **no `Offset`** and `ListRuns` caps at 500 — ask for more and it silently
returns 100. So the shared page is not merely truncated for a busy install: a job whose runs have all
fallen off it is *unreachable*, and "never run" and "ran last week" render identically on a card
while meaning opposite things. Hence the per-job top-up query, and hence D-89, which stops that
top-up asking about never-run jobs forever.

### Verification
| Gate | Result |
|---|---|
| `golangci-lint run` | ✅ 0 issues |
| `make test-unit` (`-race`) | ✅ all packages ok |
| `tsc -b` / `make web-build` | ✅ clean |
| Integration | ✅ **70 PASS / 0 FAIL / 1 SKIP**, `ok ... 413.903s` (68 before this sub-phase; the SKIP is `TestScaleMirror`). Two runs in between were invalid, not failing — see §7b; the fixture share had been deleted by the harness itself |
| `TestLogsFilterByDestination` against the **pre-change** server | ✅ **FAIL**, as required — `dest=…` returned `map[:3 82f4…:6 b02b…:6]`, i.e. both destinations plus the three run-level events that belong to neither. A filter silently matching everything cannot pass it |
| Fresh-context review | ✅ §5j — six findings, all fixed |
| Fixture shares intact after the run | ✅ checked explicitly on both servers, which is how §7b was found in the first place |
| Manual browser pass | ⏳ **outstanding** — the one gate left, and it closes Phase 4 |

Two destinations in that test on purpose: with one, a `dest` parameter that was ignored entirely
would look exactly like a working filter.

### What the manual pass needs to cover
Nothing below can be checked by the suite, and a UI-only bug has turned up in every sub-phase this
pass has run against.
1. A running job advancing live on its card, and settling to the right status afterwards.
2. Run and Preview from a card, landing on the new run.
3. A job that has never run reading "never run" rather than borrowing another job's status.
4. The log filtering by level, job and destination across more than one run, and "Load more".
5. The per-job "Log" link from a card arriving pre-filtered.

---

## 0-4b2bi. Phase 4b-2b-i status — jobs, the editor and filters

Jobs can now be created, edited and run entirely by clicking. 4b-2b-ii is the dashboard and logs.

### Built
| Area | Contents |
|---|---|
| `web/src/screens/Jobs.tsx` | jobs list with run / preview / edit / delete |
| `web/src/screens/JobEditor.tsx` | Settings and Filters tabs, client-side guards, per-rule error attribution, filter-test report |
| `web/src/components/FilterRuleRow.tsx` | one rule: direction, scope, source and the fields each source requires |
| `web/src/components/PathPicker.tsx` | first consumer of `/api/browse`, surfacing truncation |
| `web/src/api/types.ts` | **`toJobPayload`** and payload-shaped nested types; filter-test types; `RunEvent.id` corrected to a number |
| `internal/store/jobs.go`, `internal/api/jobs.go` | job deletion actually works (D-73) |

### The bug this phase existed to catch (D-71)
`decodeJSON` sets `DisallowUnknownFields`, and a `Job` from `GET /api/jobs/{id}` carries
`id`/`created_at`/`updated_at` **plus** `id`/`job_id`/`position` on every destination and filter
rule. `JobPayload` stripped only the three top-level fields, so **an editor that loaded a job and
saved it unchanged would have returned 400**. `toJobPayload()` projects to exactly the accepted
shape. `TestJobFetchMustBeProjectedBeforePatching` pins **both** directions — verbatim must 400,
projected must 200 — because only asserting the happy path would let someone "fix" a future 400 by
removing `DisallowUnknownFields`, which is what stops a misspelled field being silently ignored.

### Exit criteria (SPEC.md §11)
| Criterion | Status |
|---|---|
| A job is created, edited and run entirely from the UI | ⏳ manual pass |
| A fetched job round-trips through save without a 400 | ✅ `TestJobFetchMustBeProjectedBeforePatching`, verified to fail against the old shape |
| A filter rule changes what `filter-test` reports, then what a run copies | ⏳ manual pass; the data path was verified by hand against a live server |
| A per-rule validation error highlights the offending row | ⏳ manual pass; the message format was verified (`filter rule 1: ...`) |
| Editing a job with a parked preview is refused legibly | ✅ `TestJobCannotBeEditedWhileAPreviewIsParked` (4b-1) |

### Verification (CLAUDE.md → Definition of done)
| Gate | Result |
|---|---|
| `go vet -tags=integration ./...` | ✅ clean |
| `golangci-lint run` | ✅ `0 issues.` |
| `npm run lint` / `npm run build` | ✅ clean; 283 kB / 89 kB gzipped |
| Unit tests under `-race` | ✅ all `ok` |
| Integration under `-race` | ✅ **68 PASS / 0 FAIL, `ok ... 422.975s`** — re-run clean after the §5i review fixes. *Before them:* `412.886s` — after the mounter bound (D-80), global exclusions (D-82) and the rule-file check (D-85). *Earlier this phase:* **63 PASS / 0 FAIL, `ok ... 429.702s`** (53 before this phase, 10 new; 1 SKIP is `TestScaleMirror`). Includes the folder-creation work of D-75 to D-77 and migration `0005` |
| Fresh-context subagent review | ✅ Done — §5h, ten findings, all fixed |
| Manual browser pass | ⛔ not done — open item 1 |

### Open items
1. ~~No manual browser pass yet.~~ **Done 2026-09-04.** The user created a job in the editor, typed
   two filter patterns on separate lines (the bug §5h P0-1 fixed), saved, tested and ran it.
   `test/manual-4b2a.sh` has been deleted now that the editor does what it was standing in for.
   The **Settings / global exclusions** screen was confirmed 2026-09-04 as well: the seeded list
   renders as its two rules with patterns intact, and removing a rule prompts with the warning that
   removal is the direction that can delete. The rule-file warning remains unexercised by a human.
2. `ruleErrors` is keyed by array index and cleared only on save, so removing a row moves the red
   border to a different rule until the next save.
3. Clearing a number input yields `Number("") === 0`, which the server silently rewrites to a
   default (workers → 4, prompt timeout → 600) rather than rejecting.
4. Invalid glob patterns are not compiled at save time, so a broken pattern surfaces only on "Test
   filters" or as a failed run.
5. Pressing Enter in a text input submits the form. Not destructive — `localProblem()` runs first —
   but surprising on the Filters tab.
6. ~~The seeded global exclusions reach existing installs and are absolute.~~ **Accepted knowingly
   2026-09-04**: nothing is deployed, so every installation is fresh and no already-configured job
   can be surprised by the seed. The list stays as requested, fully editable in Settings, and the
   consequence is documented in SPEC.md §6.5 — a seeded pattern matching a file someone syncs makes
   it invisible on both sides, so the destination keeps a stale copy with no signal. **This becomes
   a real question again the first time a database predates a seed migration**; a future seed should
   apply only to a genuinely new database rather than to any schema that has not yet run it.
7. ~~A filter rule pointing at a nonexistent list file saves without complaint.~~ **Done** — D-85.
   *Original:* **A filter rule pointing at a nonexistent list file saves without complaint** and only fails when
   "Test filters" is pressed or the run fails. SPEC.md §6.5 asks for save-time validation; the agreed
   resolution is to *warn* rather than block, so a job can still be configured before its rule file
   exists. Not built yet — see the global-filters work.

---

## 0-4b2a. Phase 4b-2a status — plan visibility and run detail

Phase 4b-2 is split: **4b-2a** is everything that touches deletions, **4b-2b** is the remaining
forms (dashboard, job editor, logs).

### Built
| Area | Contents |
|---|---|
| `internal/engine/differ.go` | `Plan.WithheldDeletes` / `WithheldRmDirs`, captured at the guard site *before* the actions are discarded |
| `internal/runner/progress.go` | `PlannedAction`, `DestActions`, `actionsOf`, `planPathLimit`; withheld counts on `DestPlan` |
| `internal/runner/runner.go` | `handle.planned` published per destination as it is planned, plus `Runner.PlanFor` |
| `internal/api/runs.go` | `GET /api/runs/{id}/plan` — the per-path detail |
| `internal/api/browse.go` | `browseLimit` (2000) + `truncated`/`total`, and `browseBudget` bounding the whole listing |
| `internal/api/jobs.go`, `internal/runner/filtertest.go` | `ErrSourceUnavailable` → 502 `source_unavailable`, distinct from a 400 for a bad rule |
| `web/src/components/ConfirmPlan.tsx` | the preview confirm screen: deletions listed individually, refusal shown with its count |
| `web/src/components/PromptModal.tsx` | blocking prompt with a countdown, treating `409 no_such_prompt` as "stale, close it" |
| `web/src/screens/RunDetail.tsx` | per-destination panels, in-flight files, cancel, filtered event log |
| `web/src/screens/Runs.tsx` | a minimal run list so run detail is reachable before the dashboard lands |

### Why the paths got their own endpoint (D-68)
`DestPlan` rides `RunSnapshot.plans`, and `setPlan` is called for **every** run, not just previews
(`runner.go`), with the hub broadcasting progress to every client once a second. Inlining path lists
would have put them on every tick of every run. `GET /api/runs/{id}/plan` is fetched once when the
confirm screen opens instead. It reads live runner state, so a finished run returns **409 `no_plan`**
rather than 404 — the UI closes a stale confirm tab on that, and it should not read as an error.

### Two things the trace turned up that were nearly got wrong (D-69)
- **`Unblock` actions are not ordinary deletes.** `plan.Deletes` counts only the trailing delete
  pass, but the type-conflict clears are *also* `ActionDelete`. Filtering `Actions` by kind alone
  would have listed more rows than the count printed beside them. They are reported separately as
  `replaces` — they destroy data, so they are shown, just not conflated.
- **A blocked deletion loses its actions *and* its count.** The differ does `deletes, rmdirs = nil,
  nil` after setting a bool, so the UI could only ever say "deletions blocked". The counts are now
  captured first, so the screen says "refusing to delete 2 files" — a materially different sentence,
  and the one CLAUDE.md's "never summarised" rule is about.

### Exit criteria (SPEC.md §11)
| Criterion | Status |
|---|---|
| A preview's confirm view names the files it will delete; confirming executes that plan | ✅ `TestRunPlanNamesTheFilesItWillDelete` — asserts the specific paths, that a source file is *not* among them, and that the count beside the list matches the list. **Confirmed in a browser 2026-09-02**: the screen listed the pre-existing file and warned it would be deleted; confirming removed it and copied the source tree |
| A blocked run says so with the count, never "nothing to delete" | ✅ `TestDiffBlocksDeletionsWhenTheSourceScanIsIncomplete` (2 deletes, 1 rmdir withheld) |
| `prompt` shows a counting-down modal while the healthy destination completes | ✅ **Confirmed in a browser 2026-09-02**: the healthy destination reached `success` (3/3 files) while the unreachable one sat at `awaiting_prompt`, and the run ended `partial`. That walkthrough also exposed three UI bugs — see §5g |
| Killing the WS mid-run leaves progress advancing via polling | ⚠️ **Not verified by hand.** Browsers offer no way to close a WebSocket directly, and DevTools' Offline toggle also blocks the polling it would test. The fallback is covered by inspection and by the reconnect logic in `useEvents`, not by a manual pass. Honest gap |
| `/api/browse` caps a large listing; `filter-test` returns 502 for an unreachable source | ✅ `TestBrowseCapsALargeListing`, `TestFilterTestReportsAnUnreachableSourceAsUnavailable` |

### Verification (CLAUDE.md → Definition of done)
| Gate | Result |
|---|---|
| `go vet -tags=integration ./...` | ✅ clean |
| `golangci-lint run` | ✅ `0 issues.` |
| `npm run lint` / `npm run build` | ✅ clean; 257 kB / 82 kB gzipped |
| Unit tests under `-race` | ✅ all `ok` |
| Integration under `-race` | ✅ **53 PASS / 0 FAIL, `ok ... 349.823s`** (49 before this phase, 4 new; 1 SKIP is `TestScaleMirror`). Unit tests now include `internal/runner`, which had no test files before. One run in between hung on the `TestDestinationDisappearsMidRun` flake (§6 item 15) — harness, not product: identical Go code passed before and after, and the intervening changes were frontend-only |
| Manual browser pass | ✅ Done by the user 2026-09-02 — found three UI bugs (§5g), all fixed |
| Fresh-context subagent review | ✅ Done — see §5f. Nine findings, seven fixed |

### Open items
1. ~~No manual browser pass yet.~~ **Done 2026-09-02** — see §5g. `test/manual-4b2a.sh` set both
   runs up, because there was no job editor at the time. **Deleted 2026-09-04**, once the editor was
   confirmed working in a browser: keeping it would have left a second, diverging way to create jobs
   that nobody maintains.
2. ~~`Runs.tsx` is a placeholder front door for run detail. 4b-2b's dashboard replaces it.~~
   **Superseded 2026-09-05.** The dashboard replaced it as the *front door*, but the screen stayed:
   cards show a job's latest run only, so this is the one route to a run that is not the latest.
   Promoted from placeholder to a permanent screen — D-86.
3. The confirm screen fetches the plan once when it opens. If a destination is still being planned
   at that moment the list is short by one destination; there is no refetch. Harmless for a preview
   (which parks only after every destination is planned) but wrong if it is ever reused elsewhere.

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

**SPA** (`web/src`) — all seven screens of SPEC.md §9 as built: Dashboard (`/`), Jobs, Job editor,
Targets, Runs, Run detail, Logs and Settings. One WebSocket for the whole app with a polling
fallback (`hooks/useEvents.tsx` — call `useEvents()`, never `useFeed`), and shared components for
the pieces two screens would otherwise let drift apart (`EventList`, `Bar`, `PathPicker`,
`FilterRuleRow`, `PromptModal`, `ConfirmPlan`, `TargetModal`, `RuleFileField`).

## 3. What's next

**Immediately: the manual browser pass on the dashboard and the global log** (§0-4b2bii, and §5z
U-1). It is the only gate left on Phase 4, and it has caught a UI-only bug in every sub-phase it has
run against — three in 4b-2a alone. Phase 5a was built ahead of it knowingly: both screens are
read-only, so a bug there misinforms but cannot write.

Two pieces of built work are **deliberately withheld until that pass happens**: the dashboard's
"next scheduled run" line, and the manual click-through of the new Schedule controls in the job
editor.

~~Then **5b — webhooks**~~ — **done**, see §0-5b.

~~Then **5c — two-way sync**~~ — **deferred indefinitely 2026-09-09 at the user's direction**
(D-115). Phase 5 is now scheduling and webhooks only, and **Phase 5 is complete**. The design work
is preserved in SPEC.md §14.1 rather than deleted, so reviving it would start from the reasoning
rather than from scratch.

That leaves **Phase 6 — polish**: bandwidth limiting, the throughput graph, the multichannel toggle,
log retention and pruning, **the packaging of §10** (which no phase owned until 2026-09-05), and
docs. Plus the portable-configuration work of §8 T-1, whose credential question is already settled
(D-91).

*(Superseded Phase 5 note, retained:)*

Then **Phase 5** (SPEC.md §11): the scheduler, webhooks and trigger tokens, the `sync_state`
database of SPEC.md §6.4, and two-way conflict resolution. **Start in plan mode.** Three things
Phase 4 leaves deliberately unbuilt for it, each marked in SPEC.md §9 rather than silently skipped:
the dashboard's *next scheduled run*, the job editor's Webhooks/API tab, and run detail's conflict
resolution UI. Phase 6 owns the throughput graph, bandwidth limiting and configurable log retention.

CLAUDE.md's scaffolding note is worth re-reading before starting: Phase 5 multiplies anything that
blocks, retries or holds a lock, which is what D-80 (a `mount.cifs` that hung for nine minutes
inside `forkExec`) cost a week to find with only manual runs to trigger it.

*(Phase 4b handoff notes, retained:)*

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
| D-82 | **Global exclusions live in their own table, are exclude-only, and are absolute** | Requested 2026-09-04. SPEC.md §6.5 had no notion of a global rule, so §6.5 and §7 are amended rather than silently extended. Three constraints, each narrowing scope deliberately: **exclude-only**, because includes are OR'd and a global include would *widen* every job (global `*.txt` + job `*.jpg` admits both); **absolute**, because making a job's include beat a global exclude means rewriting `filter.Chain.decide` — the code guaranteeing `PrunesDir` and `Admits` agree, which is what stops a mirror deleting a subtree one side cannot see; and **its own table**, because `filter_rules` is wholly owned by its job (`UpdateJob` deletes every row for it) and globals must survive that. I initially told the user a job include *could* override a global, and corrected it before building on the mistake |
| D-83 | **The `len(job.Filters) == 0` early return was the most dangerous line in the global-filters change** | A job with no rules of its own would have skipped the compile loop, and with it the degradation a broken global rule must cause — running a mirror with deletions enabled against a filter narrower than configured. Most jobs have no rules, so that was the common case, not an edge. `TestBrokenGlobalRuleDisablesDeletionsEverywhere` verified against the old code reported an actually-deleted file, not a technicality. Globals also had to reach the **prune chain** as well as each destination's diff chain: pruning the source without excluding at the destination makes a subtree look "missing at the source", which is how a mirror removes what the exclusion existed to protect |
| D-84 | **Global rules are numbered separately from a job's own in errors** | "global filter rule N" vs "filter rule N", independently numbered. The job editor highlights a row from that number; a shared sequence would point at the wrong rule, or at one the editor cannot display |
| D-90 | **The test harness mounted shares inside `t.TempDir()`, and its cleanup silently deleted a live share** | Found 2026-09-05 after a suite run reported the private fixture share empty. `t.TempDir()` registers a `RemoveAll` of everything beneath it, and `buildHarness` put the mount root there, so a mount still attached at cleanup time meant `RemoveAll` walked *into* the share and deleted the server's files — `.seeded` and all. A mount outliving its test is **normal**, not a bug: abandoning an unmount on its deadline is exactly what D-80 designed the mounter to do when a server dies. So the harness was destroying data whenever the safety mechanism worked as intended. Two properties made it invisible: the deletion succeeds silently against a *reachable* server, and the only log line came from the other mount, whose server was blackholed and therefore *refused* the same recursion — an error that reads as unrelated. Fixed structurally rather than with a check: the mount root moved out of `t.TempDir()` and nothing in its teardown may use `RemoveAll`. `os.Remove` on a directory succeeds only when it is empty, so against a live mount it returns EBUSY and cannot recurse by construction. A leftover `/tmp/cn4m-mnt-*` is the correct outcome; `make harness-clean` sweeps them. `TestMountRootCleanupNeverDeletesThroughAMountpoint` was verified to fail against the old code with `cleanup deleted a file underneath a mountpoint` |
| D-91 | **Config export omits passwords; the import re-enters them** | Confirmed by the user 2026-09-05 (§8 T-1). Stored passwords are encrypted under a key derived from `ENCRYPTION_KEY`, so exported ciphertext is undecryptable on any other installation, and exported plaintext is a file of working SMB passwords in a downloads folder. The export therefore carries no secret at all and names the targets that need one, so import shows a short list to fill in. A passphrase-encrypted export remains available later as a refinement without changing the rest of the format |
| D-101 | **`make test-integration` refuses to run when the harness environment is missing** | The suite skips every Samba test when `CN4M_TEST_SAMBA_A/B` are absent and then reports `ok` with exit 0. That is correct behaviour for a developer without a harness and a trap for everyone else: after the `SMBSYNC_* → CN4M_*` rename it produced a green run of **9 PASS and 70 SKIP** against a container whose environment predated the change (§7c). A skip-shaped pass is worse than a failure because it is indistinguishable from success unless someone compares the pass *count* to the previous run. The target now checks for the variables first and says how to recreate the container |
| D-98 | **`workers` defaults to 1, and is called "Files at a time" in the UI** | Requested 2026-09-06 after the field was read as controlling *destination* concurrency. It does not: it is files in flight within a single destination, while destination ordering is `parallel_destinations`, which has always been off — so destinations were already processed one after another in the order listed. The distinction is worth stating because the two settings pull in opposite directions: raising `workers` makes the **first** destination finish *sooner*. One is nonetheless a defensible default — gentlest on the source share, most predictable to watch — and raising it per job is one field |
| D-99 | **The project is renamed "cn4m cascade", except the HKDF info string** | Requested 2026-09-06 with explicit permission to break the existing build and config, nothing being deployed. Renamed: the page title and header brand, `cmd/cn4m-cascade`, the npm package, the SQLite filename (`cn4m-cascade.db`), the session cookie, the container names, and the `SMBSYNC_*` env prefix → `CN4M_*`. **`secrets.hkdfInfo` is deliberately left as `smbsync-target-creds-v1`**: it is never displayed, so a new name buys nothing, and changing it changes the derived key — every stored target password becomes undecryptable, with nothing failing at build, lint or test time. Leaving it is also what lets an existing database keep working after being renamed on disk. If it ever must change it needs a `-v2` and a re-encryption migration, which is what the `-v1` was always for |
| D-100 | **Leaving a parked preview cancels it** | A preview holds its job until confirmed or timed out, so Run answers "already in progress" for something making no progress — up to `prompt_timeout_sec` of a job that looks stuck for no visible reason. Navigating away having been shown the plan is treated as a decision. Guarded against React StrictMode's simulated unmount, which fires before the first fetch resolves and would otherwise cancel a preview the instant it opened. The 409 body now also distinguishes a parked preview from a live run |
| D-108 | **The hook rate limiter runs *after* token verification, not before** | Checking `blocked()` first meant one client with a stale token permanently 429'd every other caller at the same address — and behind a NAT, a reverse proxy or a Docker bridge, every integration shares one address. A bad token is still recorded and still blocked, so brute force is bounded exactly as before; what changes is that a working integration is never collateral damage. The map also has a hard ceiling with oldest-first eviction, because expiry alone is not a bound: a burst from many fresh addresses inside the window leaves every entry unexpired. Evicting a live entry forgets someone's failures early, which is the right trade — the ceiling exists to stop an anonymous caller exhausting memory, and an attacker still failing has one of the newest entries |
| D-109 | **`prompt` is refused as an `unavailable_policy_override`, not accepted and ignored** | The override exists because an unattended trigger is exactly when a job set to *ask* about an unreachable destination should not. Accepting `prompt` would produce a run that parks for its whole timeout with nobody to answer — silently doing the opposite of what the parameter is for. The override applies to a copy of the job, so triggering a job can never reconfigure it |
| D-110 | **A webhook `preview:true` can hold a job indefinitely, and SPEC says so** | A parked preview occupies the job until confirmed or timed out, and nobody confirms a webhook preview. A token holder looping it keeps the job unrunnable — scheduled runs skipped, manual ones refused. Support is kept because §8.1 offers it and "plan without executing" is legitimate, but the consequence is now written down: a trigger token's power is "can start this job **and** can keep it from running", which belongs in the decision to issue one |
| D-115 | **Two-way sync is deferred indefinitely and removed from the UI** | Requested 2026-09-09: the deployments this serves only ever push one direction, so it would have been the most dangerous feature in the product built for nobody. Every other deletion here is decided by comparing two *live* listings; two-way deletion is inferred from a **stored record of the past**, and that inference fails towards data loss — an empty, stale, partial or interrupted `sync_state` makes present files look deleted. Removing it eliminates that entire class of risk rather than deferring it behind a flag. The design work is preserved in SPEC.md §14.1, not deleted: the single-destination constraint, the first-run-no-deletions rule and the never-guess-a-conflict rule each cost real thought and should not be re-derived. Cheap to do because almost nothing depended on it — `engine.Conflict` is one-way sync's "declined to act, and why", not two-way conflict resolution, and `sync_state` was never built. The UI change was a single disabled dropdown option |
| D-114 | **`CN4M_STATUS_URL` seeds the suite callback at startup, not in the migration, and never overwrites** | Testing against a real cn4m on another host showed the hardcoded `localhost:2640` seed is wrong for any deployment where cascade and cn4m are not co-located — and a migration cannot read the environment, so the variable would have been useless on exactly those installs. The row is created at first start instead, from the configured URL, with `off` to disable. Not re-applied on later starts: a variable in a compose file must not revert a deliberate UI change, the same rule the admin password follows. Keyed on format rather than URL, so a callback someone has re-pointed still counts as present |
| D-111 | **Callbacks have a wire `format`, because cn4m does not speak the generic one** | `/suite/status` reads form fields; the generic webhook posts signed JSON. One delivery path with two encodings rather than two mechanisms, chosen so subscription, throttling, retry and the queue are shared and cannot drift. The cn4m format is unsigned (that endpoint checks nothing, so a required secret would be theatre) and **silent on failure**: cn4m is optional infrastructure whose absence is a normal state, and a seeded callback that warned on every run of a machine without cn4m would train people to ignore run warnings |
| D-112 | **Best-effort endpoints are circuit-broken, generic webhooks are not** | An absent cn4m was being retried three times per event, per run, for the life of the process. It now backs off from a minute, doubling to thirty — the schedule cn4m's own client uses — and one success clears it outright. Deliberately *not* applied to JSON webhooks: their failures are reported against the run so someone can see and fix them, and suppressing deliveries would hide something the user asked to be told about |
| D-113 | **A cancelled run now reports completion** | `execute` returned from the cancellation branch before the notify block, so a run cancelled after being announced was never announced as finished — leaving an integration waiting forever and a cn4m row stuck on "working". Found while deciding which level `cancelled` should map to, which is the kind of bug only a second consumer of the same data surfaces |
| D-102 | **Trigger tokens are hashed and shown once, not encrypted and viewable** | SPEC.md §8.1 and §9 contradicted each other; settled with the user in favour of §8.1. Sessions already work this way (`hashToken`), and the consequence is the point: a leaked database contains no credential that can start a run. The cost is real — wiring up an integration months later means regenerating and re-pasting — and was accepted knowingly. PBKDF2 is deliberately **not** used despite the admin password using it: 600k iterations exist to defend a low-entropy human-chosen secret, and a status endpoint built for polling must not pay that per request |
| D-103 | **`/api/hooks/*` sits outside the session guard by ServeMux precedence, and a test asserts it in both directions** | `root.Handle("/api/hooks/", …)` beats `root.Handle("/api/", requireSession(mux))` because Go 1.22 gives the longest pattern priority. That is correct and says nothing about authentication when you read it, so the routing is pinned by a test rather than by a comment: hooks reachable with a token and no cookie, and every other `/api/*` route still 401. The two failure modes are opposite and both are bad — hooks behind the guard breaks every integration silently, anything else outside it publishes the whole API |
| D-104 | **Issuing and revoking tokens is session-guarded, never token-guarded** | If a trigger token could mint tokens, a leaked one would issue itself replacements faster than anyone could revoke them. The integration test tries exactly that and expects 401 |
| D-105 | **Callback delivery drops rather than blocks, and never touches the sync** | The queue is bounded and a full queue discards the event with a warning. Blocking would push a stranger's outage into the run pipeline, which is the one thing §8.2 must not do — the same rule CLAUDE.md applies to blocking I/O against a share, pointed at a socket. Redirects are not followed either: a URL reviewed at configuration time is the only one that should ever receive a signed body. A hook whose secret cannot be decrypted is **not** delivered unsigned, because a receiver that does not verify would accept an unauthenticated POST believing it was authenticated |
| D-106 | **The §8.1 status shape is built in one place, `runner.StatusPayload`** | §8.2 requires callbacks to carry the same JSON as the polling endpoint. Two builders would drift the first time a field was added to one, and this is the only shape consumed by software outside the project — drift there is a broken integration, not a cosmetic bug |
| D-107 | **`?queue=1` holds exactly one run, in memory** | Five triggers during a long run mean one follow-on run, not five: the promise is "one run will follow". In memory because a queued run that survived a restart would fire for a request nobody remembers making, possibly hours later. The dequeue takes the slot under the same lock that releases the job, so a trigger arriving at that instant either queues behind a still-registered run or starts one outright — never both, never neither — and `closing` stops a cancelled run launching its successor into a shutting-down server |
| D-92 | **The scheduler treats SQLite as the source of truth and keeps no in-memory cron registry** | `robfig/cron` is used for parsing only; a 30s tick asks `DueJobs` what is due and recomputes each firing. The alternative — `cron.Cron`'s own scheduler with an `EntryID` per job — needs bookkeeping on every job create, update and delete, and any missed hook leaves the schedule someone sees different from the schedule that runs. Polling costs one partial-indexed query per thirty seconds and makes that class of bug unrepresentable. `next_run_at` is persisted rather than computed for the same reason it exists at all: after a restart, "was a firing missed while we were down?" is only answerable against a time that survived the restart |
| D-93 | **A firing missed while the server was down does not run late** | It is logged by name and rescheduled forward. A server off overnight would otherwise start every missed job at boot — several at once, at an hour nobody chose, possibly while someone is mid-restore. Missing one night is recoverable; a stampede of unattended syncs is not. Skipping *silently* would be worse than either, so `TestMissedScheduleDoesNotFireAtStartup` asserts on the log line, not merely on the absence of a run |
| D-94 | **An overlapping firing is skipped, never queued — and a job that cannot be rescheduled is not started at all** | Queueing turns "this job cannot finish inside its own interval" from a visible misconfiguration into a slowly worsening one. The subtler half is R-4: `reschedule` runs *before* the start and its failure now blocks the run, because a job left due after a failed `SetNextRun` would be picked up by the next tick and — if the first run finished inside thirty seconds, so the overlap guard missed it — produce a second unattended run from a single firing |
| D-95 | **An unparseable schedule stops the run, not just future scheduling** | The first implementation rescheduled and then started the job anyway, reasoning that the firing was genuinely due. A unit test caught it. Validation rejects bad expressions at save, so a bad one in the database arrived by some route that bypassed validation — and starting an unattended sync from configuration nobody can read is the wrong way to fail |
| D-96 | **Cron is evaluated in the server's local timezone, not UTC** | `cron.ParseStandard` builds a schedule whose location is `time.Local` and robfig evaluates fields in the location of the instant it is given, so passing UTC instants silently made `0 2 * * *` mean 2am UTC. The preview endpoint meanwhile used `time.Now()` and reported `time.Local`, and the job editor tells users to set `TZ` — so following the UI's own advice would have moved every schedule by the offset while the UI kept claiming otherwise. Local everywhere now, which is what a person typing "2am" means. Consequence accepted and documented: DST applies, so a job scheduled inside the hour that does not exist on a spring-forward day does not run that day |
| D-97 | **`jobPayload.Enabled` is a `*bool`, so an absent field means enabled** | A plain bool would make omitting the field mean *disabled*. For the one switch that governs whether backups happen, that is the wrong way round: every existing client and every hand-written `curl` would silently turn scheduling off. `nil` means "no opinion expressed", and the answer to that is yes |
| D-86 | **The Runs list is promoted from placeholder to a permanent screen, which SPEC.md §9 also does not list** | It was written in 4b-2a as a temporary front door, with a comment saying the dashboard would replace it. The dashboard replaced it as the *landing page* and did not replace it as a screen: cards show each job's **latest** run, and `store.RunFilter` has no `Offset`, so without this list a run that is neither the latest nor already known by id is unreachable from the UI. Recorded on the same grounds as D-74 rather than left as a comment contradicting itself, and §9 gained a paragraph naming both |
| D-87 | **The global log's destination filter needed an index, not just a parameter** | `EventFilter.DestTargetID` already existed and `handleLogs` merely never set it, so wiring it up looked free. It was not: `dest_target_id` had only ever been filtered *alongside* `run_id`, which `idx_run_events_run_level` covers, and the global view has no run to scope by. The query became `WHERE dest_target_id = ? ORDER BY ts DESC` against the largest table in the schema — walking `idx_run_events_ts` newest-first with a row lookup per candidate, reading the whole table whenever the destination is a quiet one. Which is the case people filter for, and the Logs page re-issues it every few seconds while a run is in flight, i.e. while `run_events` is being written hardest. Migration `0007` adds `(dest_target_id, ts)`, the `ts` half so the ordering is served by the index rather than sorted afterwards. Caught by the review, not by me: the feature worked and the test passed |
| D-88 | **The dashboard refetches on a run *transition*, not on the presence of finished runs** | First written as "reload whenever the set of terminal run ids changes", which misfires twice. The feed arrives already populated — with the socket down its first poll merges the newest fifty runs, nearly all long finished — so the set flips from empty to fifty ids immediately after the initial load, doubling every page load. And the refetch buys less than it appears to: the completed run's own row is already fresh, since `hub.tick` re-reads via `ListRuns` and the live copy outranks the fetched one. What it actually catches is the surroundings — the polling path, a job or target renamed in another tab. Now only a run this page watched while it was still running counts as a completion |
| D-89 | **A job that has never run is remembered as such, or the dashboard asks about it forever** | `ListRuns` caps at 500 with no offset, so a job whose runs fell off that page would render as "never run" — the top-up query per empty job exists for that. But a job that genuinely has no runs is *permanently* absent from the shared page, so the top-up asked again on every refresh and got the same empty answer: forty configured-but-unrun jobs meant forty extra requests each time. Caching the answer is safe in one direction only, which is the direction that matters — once a job runs, its run appears in the shared page, so never asking again cannot miss anything |
| D-85 | **A rule file's existence is checked advisorily, never blocking a save** | Rule files are read at the start of every run, not snapshotted (SPEC.md §6.5), so configuring a job before its file exists is legitimate. `POST /api/filters/check-file` stats rather than reads — a rule file can be large and this is an existence question — and reports `checked: false` for a `target://` path rather than mounting a share to answer a form field. "Not checked" and "checked and missing" must not look the same |
| D-80 | **`mount.cifs`/`umount` are bounded at the caller, not by `exec.CommandContext`** | The "flaky" `TestDestinationDisappearsMidRun` was never a test problem. The `-timeout 15m` added to `make test-integration` finally produced a goroutine dump: `Manager.Shutdown` parked **nine minutes** in `syscall.forkExec`, under `forceUnmount`, unmounting a share whose server had gone. `exec.CommandContext`'s watchdog only arms *after* `Start` returns, and fork/exec itself blocks in a process whose threads are parked in uninterruptible CIFS syscalls — **which §6 Phase 3 item 7 already documented, having hit and fixed exactly this in *test* code.** The production mounter never got the same treatment. It violated CLAUDE.md ("nothing may hang forever when a share dies"), SPEC.md §10's graceful shutdown, and `manager.go:573`'s own comment promising Shutdown "never blocks on a dead server". Both `Mount` and `Unmount` now run on their own goroutine with a `select` on ctx, the `engine.bounded` shape. The deadlines already existed in `forceUnmount`; they simply were not enforceable |
| D-81 | **A command abandoned on its deadline must not have its output buffers read** | Two data races written while fixing D-80, both caught by `-race` before they landed. First `cmd.Process.Kill()` on the timeout path, which races the `Start` writing that field — and was pointless, since `CommandContext` reaps the process once Start returns and a stalled fork has no process to kill. Then `Unmount` reading `stderr.String()` after abandoning the command, racing the command still writing into it. An `errTimedOut`/`abandoned(err)` pair now gates every buffer read. Worth recording because it is the *same class of error as the bug being fixed*: assuming something you stopped waiting for has stopped running |
| D-78 | **The web UI listens on 2649** | Requested by the user 2026-09-04. `LISTEN_ADDR` default, the compose port mapping, the Vite dev proxy, both test scripts and the docs all moved together; PROGRESS keeps its historical references to 8384 so older entries still read correctly |
| D-79 | **Targets and jobs can be duplicated, and a target can be tested without closing its modal** | Many targets differ only by address, and many jobs only by one destination — retyping a filter set to change an IP is the kind of friction that produces mistakes. A duplicated target does **not** copy the password (there is nothing to copy: the API never returns it), and a duplicated job is created and opened in the editor rather than saved silently, because the thing being changed is whatever makes it a different job. "Save and test" saves first by necessity — the API only tests a target it already knows about — which is safe for a flat target record in a way it would not be for a job, whose PATCH deletes and reinserts its children |
| D-75 | **Creating a missing destination folder is its own job setting, `create_dest_dirs` (ask/always/never), defaulting to `ask`** | First built as a corner of `unavailable_policy`, which was wrong: "the destination is unreachable" and "the folder does not exist yet" are different questions, and a job set to *skip* unreachable destinations is expressing caution — silently creating folders is the opposite of it. The user hit exactly that: a run auto-created a folder with no prompt because the job's policy was the default `skip`. Migration `0005` adds the column with `DEFAULT 'ask'`, so existing jobs get the confirmation too. An unanswered prompt skips the destination, so an unattended run never invents a folder from a typo |
| D-76 | **A preview creates nothing, whatever `create_dest_dirs` says** | Auto-creating at resolve time made `planOneDestination` — documented "It writes nothing" — write, so a preview would have created a directory before the plan was ever shown, breaking the "nothing has been written yet" promise on the confirm screen. `mayCreate` is gated on `!h.preview`, and the prompt does not offer Create during a preview either: an answer the run would refuse to honour is worse than not offering it. `TestPreviewDoesNotCreateTheDestinationFolder` was verified to fail against the broken version |
| D-77 | **`storage.ErrPathNotExist` distinguishes absent from unreachable** | `statBounded` collapsed a missing path into a plain `errors.New("does not exist")`, discarding the sentinel, so nothing downstream could tell "the folder is not there" from "the NAS is down" — which want opposite responses. Found while wiring folder creation; the local path was also reporting a missing *subpath* using the target's own root, so a job with a wrong subpath read as a broken target that had tested green seconds earlier |
| D-71 | **`toJobPayload()` projects a fetched job to exactly what the API accepts** | `DisallowUnknownFields` means a `Job` cannot be handed back verbatim: it carries server-owned fields on the job *and* on every nested destination and filter rule. The committed `JobPayload` stripped only the top-level three, so loading and saving a job unchanged would have 400ed. The nested payload types are separate from the response types rather than derived from them, because deriving is exactly how the nested fields were missed |
| D-72 | **The Filters tab's Test button is disabled while anything the test reads is unsaved** | `filter-test` loads rules from the database and never reads the request body, so unsaved edits cannot be tested. Auto-saving was rejected: PATCH is a full replace that re-mints every rule ID, and mid-edit is the wrong moment for it. The dirty check covers the source and destinations too, not just the rules — `runner.FilterTest` scans the *saved* source subpath, so a changed source would produce a report on a different tree presented as validation of these rules |
| D-73 | **Deleting a job deletes its run history, and is refused while it is running** | `runs.job_id` references `jobs(id)` **without** `ON DELETE CASCADE`, unlike every other child table, so deleting any job that had ever run failed with a raw `FOREIGN KEY constraint failed (787)` shown to the user verbatim — the feature was broken for exactly the jobs anyone would want to delete. Deleting the runs beats refusing: a job nobody can delete because it once ran is worse, and an orphaned run is a log entry pointing at a job that no longer exists. `run_destinations` and `run_events` already cascade from `runs`. Delete now also refuses while a run is active, matching PATCH — the runner holds the job in memory and the history being deleted is still being written. `TestDeletingAJobRemovesItsRunHistory` was verified to fail against the old code with the exact FK error |
| D-74 | **A jobs index screen exists, which SPEC.md §9 does not list** | §9 puts job cards on the Dashboard and names no separate jobs page. A list is needed as the editor's entry point before the dashboard exists (4b-2b-ii), and it remains the natural home for edit/delete, which do not belong on a dashboard card. Recorded rather than left as a silent deviation; §9 should gain it if it stays. **It did, 2026-09-05** — see D-86, which covers the Runs list on the same grounds |
| D-70 | **A host folder is shared into the container as `/mnt/local` by default** | Requested by the user 2026-09-02: local folders are a common source, and telling people to find a path that exists *inside* a container is a bad first experience. `~/cn4m` (`%USERPROFILE%\cn4m` on Windows) is created by `make harness-up` and mounted at `/mnt/local`, so the answer to "what do I type?" is always the same string regardless of platform. Overridable with `CN4M_LOCAL_DIR` in the shell or a `.env`. Cross-platform via Compose's nested defaults, `${CN4M_LOCAL_DIR:-${HOME:-${USERPROFILE:-/tmp}}/cn4m}` — verified that an unset `HOME` falls through to `USERPROFILE`, which is the Windows case. **Verified end to end**: a file written on the host in `~/cn4m` synced to an SMB share with `run: success`. The production compose of SPEC.md §10 should mirror this when it is built |
| D-68 | **The plan's per-path detail is its own endpoint, not a field on the progress payload** | `RunSnapshot.plans` is broadcast to every WebSocket client once a second, and `setPlan` runs for every destination of every run, not just previews. Inlining delete paths would have put a path list on every tick of every run. `GET /api/runs/{id}/plan` is fetched once, when the confirm screen opens. It reads live runner state — nothing persists a plan — so a finished run is **409 `no_plan`**, distinct from a 404, because a stale confirm tab hitting it is an ordinary race and must not read as a failure |
| D-69 | **`Unblock` removals are reported separately from deletions, and withheld deletions keep their count** | Two traps found while tracing the deletion path. (1) `plan.Deletes` counts only the trailing delete pass, but the type-conflict clears appended earlier are *also* `ActionDelete`; filtering `Actions` by kind alone would list more rows than the number printed beside them, so they are surfaced as `replaces` — shown, because they destroy data, but not conflated. (2) The guard does `deletes, rmdirs = nil, nil` after setting a bool, destroying the count along with the actions; `WithheldDeletes`/`WithheldRmDirs` are now captured first, so the UI can say "refusing to delete 2 files" instead of a bare "deletions blocked". The actions themselves stay discarded — a blocked deletion must not be one bug away from executing |
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

## 5f. Phase 4b-2a review — findings and resolution

A fresh-context subagent reviewed the 4b-2a diff against SPEC.md §6.1/§8/§9/§11, the CLAUDE.md hard
rules and the 4b-2a exit criteria.

**The core claim held.** The reviewer traced deletion-count consistency end to end and confirmed
`DestPlan.Deletes` and `DestActions.Deletes` **cannot disagree** — both derive from the same
post-guard slice, and when the guard fires both go to zero together. The withheld-count capture
point is right, the `PlanFor`/`publishPlan` locking is race-free (the lock pair supplies the
happens-before edge for the `p.plan` write, and `-race` is green), the new endpoint is behind the
session guard with the 404/409 split correct, and `st.Release` still works after the budget expires.

### Fixed

| # | Finding | Fix |
|---|---|---|
| P0-1 | **The browse cap truncated *before* the dirs-first sort.** `ReadDir` returns entries in name order, so slicing the first 2000 kept the alphabetically-earliest names and dropped every directory sorting after them. A share root with 200k `f*.txt` files and a `zzz-media/` directory returned 2000 files and **no directories** — a path picker that cannot navigate, which is the only thing a path picker is for | sort before truncating (`browse.go`). `TestBrowseCapsALargeListing` now seeds a directory that sorts after every file; **verified to fail** against the old ordering |
| P0-2 | **`browseBudget` bounded latency but not the syscall fan-out, and my comment claimed otherwise.** `engine.bounded` launches its goroutine and *then* selects — there is no `ctx.Err()` pre-check — so an expired budget did not stop the loop starting another `lstat`. A share dying mid-listing fired one goroutine per remaining entry in a burst: up to 2000 threads parked in the kernel per request, against Go's 10,000-thread hard limit | explicit `ctx.Err() == nil` check before each `InfoBounded`; sizes are dropped rather than rows, since a listing without sizes is still a usable picker. The comment now says what the code does |
| P1-3 | **`replaces` was the one list with no count anywhere** — `DestPlan` had no total for unblock removals, so a truncated list of >2000 of them showed 2000 rows with no number stating the real figure. The under-reporting D-69 exists to prevent, arriving through the list D-69 created | `engine.Plan.Unblocks` → `DestPlan.Replaces`, and the list checks itself against it |
| P1-4 | **One `truncated` flag across five lists cried wolf.** A first mirror of a large tree exceeds the cap on `mkdirs` and `copies` — lists the confirm screen never renders — so the user saw "some lists are truncated" while every list shown was complete. That trains people to dismiss the notice that matters | per-list reporting: `PathList` takes the authoritative count and says "N more not listed" only for the list actually short |
| P1-5 | **Two WebSockets per page.** `useEvents` was a plain hook with its own `useEffect`, mounted in the shell *and* in `Runs`/`RunDetail`, so each screen held two sockets with two independent states — doubling the chance of tripping the hub's slow-client drop, and doubling the poll load on fallback | lifted into `EventsProvider`, mounted once in `Shell`; `useEvents` now reads context and throws outside it |
| P1-6 | **A destination with counts but no path detail rendered as if complete**, with Confirm still enabled — counts without paths is exactly the state that looks finished | `safeToConfirm` blocks the button when any destination plans removals whose paths are missing, with an explicit message |
| P2-7 | **`actionsOf` checked `Unblock` *after* the mkdir and copy cases**, where `engine.partition` checks it first. They agree only because `Diff` never sets `Unblock` on a mkdir or copy today; if that changed, the executor would remove a path the screen filed under "will be created" | `case a.Unblock:` hoisted to the top, mirroring `partition`. `TestActionsOfTreatsAnyUnblockAsARemoval` pins it |
| P2-9 | **Overwrites were invisible.** `PlannedAction.overwrite` was populated and typed but never rendered; a mirror overwriting 500 destination files said only "500 to copy", though an overwrite destroys the destination's version as permanently as a delete | `engine.Plan.Overwrites` → `DestPlan.Overwrites`, surfaced on the confirm screen |

**Also closed the coverage gap the reviewer flagged**: `internal/runner` had no test files at all, so
`actionsOf` — the function this sub-phase exists for — had zero direct coverage. `progress_test.go`
now covers unblock separation, truncation, a nil plan, and that `DestPlan`'s counts match the lists.

### Tracked, not fixed
- **P2-8: the prompt modal has no dismiss control.** Partly addressed — both 409s now close it, and
  it no longer renders over a terminal run — but there is still no explicit close button. If a
  future state strands it, the user's only recourse is a reload. §6 Phase 4b-2 item 1.
- Exit criterion 2 (withheld counts) is proven in `internal/engine` and now in `internal/runner`,
  but nothing asserts the count survives to the wire. Narrow rather than vacuous. §6 item 2.

## 5g. What the 4b-2a manual pass caught

The browser walkthrough (2026-09-02) confirmed both screens work — and found three bugs that every
automated test had missed, because all three are about what the *screen* does over time rather than
what the API returns.

| # | Bug | Fix |
|---|---|---|
| 1 | **The event log never refreshed.** `EventLog` fetched once on mount and never again, so a run detail page opened as a run started showed "Nothing logged yet" for the entire run. The events existed the whole time — verified 5 of them in the database for a run whose UI showed none; nothing ever asked for them a second time | polls every 3s while the run is live, and refetches once when it goes terminal so the final events land without pressing Refresh |
| 2 | **A finished run showed a destination still `awaiting_prompt`.** `useEvents` deliberately retains the last progress snapshot after `run_finished` so a completed run keeps its final numbers — but that snapshot was taken *before* the fallback fired, so the panel contradicted the summary directly above it ("1 skipped (unavailable)") | once the run is terminal the panels are built from `run.destinations` (the database, authoritative) rather than the retained snapshot. Throughput, ETA and in-flight files are now live-only fields, since printing a rate for a stopped run is a lie |
| 3 | **Unformatted throughput** — `2.650562150193176 B/s`. `bytes()` rounded nothing below 1 KiB, and throughput is a float | rounded |

None of these was reachable from the API tests: the endpoints were correct in all three cases. They
are the argument for the manual pass being a real gate rather than a formality.

## 5h. Phase 4b-2b-i review — findings and resolution

A fresh-context subagent reviewed the job editor — the most destructive configuration surface in the
product, since PATCH deletes and reinserts every destination and filter rule.

**Clean, each verified rather than assumed:** the client-side overlap guard was differential-tested
against the real Go `overlaps()` over 17 cases and has **no false positives**, so it never blocks a
legal job; the filter-test report keys by `rule_id` and never zips arrays by index; `toJobPayload`
emits *precisely* the 17 + 2 + 9 fields the three payload structs declare, with `compare_tolerance_sec`
correct; no building ahead; no credentials; and the round-trip test is genuine, not vacuous.

### Fixed

| # | Finding | Fix |
|---|---|---|
| P0-1 | **Typing multi-line patterns silently concatenated them.** The textarea's value was the *parsed* array joined back, while `onChange` stripped blank lines — so pressing Enter at the end of a line produced text that parsed to the same array, React re-asserted the old DOM value, and the newline was erased as it was typed. `*.tmp` then Enter then `cache/` became the single pattern `*.tmpcache/`, which matches nothing: **both intended exclusions silently stop protecting anything, and in mirror mode their files become extraneous**. Pasting worked, so any paste-based test would have missed it | the textarea owns its raw text; patterns are derived on change. The reasoning is in a comment at the component, because the naive version looks correct |
| P0-2 | **A failed job load left an editable blank form bound to a real job id.** `loading` went false in `finally`, so a transient GET failure rendered `BLANK` — one empty destination, no filters — under the real id. Filling in a name and saving would PATCH every destination and filter rule out of existence | the form is not rendered at all when the load failed, with an explanation and a retry |
| P0-3 | **Deleting a job that had ever run failed with a raw FK error** — see D-73. Broken for every job worth deleting, and the message was `constraint failed: FOREIGN KEY constraint failed (787)` | history deleted with the job, in a transaction; delete refused while running; two regression tests, one verified to fail against the old code |
| P1-4 | **The delete-policy control was mislabelled "If deletions fail"** — it governs the opposite: what happens to deletions when *copies* fail. The one setting deciding whether a partially-failed mirror still deletes described a different condition | relabelled "If some files could not be copied", options reworded, with a note on why deleting after a failed copy is dangerous |
| P1-5 | **A target-scoped rule could display one destination and mean another.** With no placeholder option, a `scope_target_id` no longer among the destinations made the browser select the first entry without firing a change event. The server rejected it, but with a message shaped `filter rule N is scoped to...` — **no colon** — which the error parser did not match, so the row was not highlighted and the user saw a raw target id | placeholder option, an inline warning when the scope target is stale, and the parser accepts both message shapes |
| P1-6 | **The Test button ignored source and destination changes** — see D-72 |
| P1-7 | **Switching between job ids did not re-enter the loading state**, so `/jobs/A` → `/jobs/B` showed A's populated form under B's id, and a save in that window wrote A's settings onto B. Same for `/jobs/:id` → `/jobs/new`, which arrived pre-filled | the effect resets state per id and ignores a response that arrives after the id changed |
| P2-8 | A save overwrote the form with the server's response, discarding anything typed while it was in flight | the form is disabled while saving |
| P2-9 | A `targets.list()` failure was swallowed, leaving every dropdown reading "Choose…" with no explanation | surfaced |
| P2-10 | PROGRESS had no section or decisions for this sub-phase, and the jobs index screen is not in SPEC §9 | this section, D-71 to D-74 |

Three of these — P0-1, P0-2 and P1-5 — are the same shape: **a control that displays one thing and
means another**. In a form whose save is a full replace, that is the failure mode worth hunting.

## 5i. Review of the mounter bound, global filters and the rule-file check

A fresh-context subagent reviewed all three. **The four properties that could destroy data were each
traced and confirmed correct**, which is the result that mattered: globals reach both the prune chain
and every destination's diff chain (so the two sides agree and no subtree looks "missing at the
source"); the early return no longer skips globals; a broken global degrades every chain including
the prune chain; and the exclude-only, job-wide projection cannot be subverted — there is no field,
column or payload key by which a global could become an include or target-scoped.

Nine further findings, eight fixed.

| # | Finding | Fix |
|---|---|---|
| P0-1 | **My own fix was incomplete.** Every `mount`/`umount` call is bounded, but `Shutdown` loops targets serially and never checked its *own* deadline: three dead mounts × 15s exceeds the 30s shutdown budget, so the process is SIGKILLed mid-cleanup — the same SPEC.md §10 promise D-80 restored, one level up | `ctx.Err()` checked each iteration; the remainder is left to the kernel, which is what a lazy detach hands it anyway |
| P1-2 | **`create_dest_dirs` and the Create prompt shipped with no SPEC amendment**, while global filters in the same diff got one. Same rule, inconsistent treatment | §6.1 and §7 amended |
| P1-3 | **The rule-file check contradicted its own doc comment and D-85**, returning `checked: true, exists: false` for *any* error — a timeout or `EACCES` asserted the file was missing | non-`ErrNotExist` failures now report `checked: false` |
| P1-4 | **The Settings screen warned about the safe direction only.** Adding an exclusion cannot delete; **removing** one can — a path that stops being excluded becomes visible, and if it is at a destination but not the source, the next mirror removes it | an explicit warning plus a confirmation naming that consequence |
| P1-5 | **Store failures were reported as `400 invalid_filter` with a raw Go error**, so `SQLITE_BUSY` read as "your payload is wrong: database is locked" | validation moved ahead of the store; a 400 now means the payload, a 500 means us |
| P2-6 | An empty *global* rule file disarms every job at once, but logged at warn like a single job's | error level for globals |
| P2-8 | No unit coverage for the global-filter store | `internal/store/global_filters_test.go`, including that a rejected replace leaves the set untouched |
| P2-9 | Doubled error text: `unmounting X: unmounting X: ...` | one wrap |

Also from the nits: `runBounded` now supplies its own default deadline, so a future caller passing a
context without one does not silently get the unbounded behaviour the function exists to prevent.

**Not fixed, needs a decision:** the seeded defaults reach **existing** installs, and because global
exclusions are absolute no job can re-admit them. Most are noise, but `ada.jpg`, `george.jpg`,
`assets.json` and `ROBOCOPY.RCJ` are ordinary filenames — a job syncing a real one silently stops
updating it (no deletion; a permanently stale destination copy). Disclosed in SPEC §6.5 and raised
with the user. §6 Phase 4b item 6.

## 5j. Phase 4b-2b-ii review — findings and resolution

A fresh-context subagent reviewed the dashboard, the logs screen and the `dest` parameter. Six
findings, all fixed. It also confirmed the parts most likely to be wrong and were not: no second
WebSocket (both screens call `useEvents()`, the context reader, never `useFeed`); no render loop or
stale closure in the live-merge effects; `started_at` round-trips losslessly through
`formatTime`/`parseTime`, so live and fetched copies of one run compare equal and the live copy wins;
`percent()` guards `total <= 0`, so a scanning run shows `0%` and not `NaN%`; and no Phase 5 or
Phase 6 feature crept in.

| # | Finding | Resolution |
|---|---|---|
| M-1 | **The `dest` filter had no index and scanned the largest table.** Wiring up an existing `EventFilter` field looked free; it was not, because `dest_target_id` had only ever been filtered alongside `run_id` | Migration `0007` adds `(dest_target_id, ts)` — D-87 |
| M-2 | **The per-job top-up re-queried never-run jobs forever**, uncapped and on every refresh; and the refetch it hung off fired spuriously on the feed's first poll | The never-run answer is cached (D-89), and the refetch now triggers on a watched run's *transition* rather than on terminal runs being present (D-88) |
| M-2b | **The refetch comment overstated what it buys.** The completed run's own row is already fresh — `hub.tick` re-reads via `ListRuns` and the live copy outranks the fetched one | Comment rewritten to say what it actually catches: the polling path and list drift |
| L-3 | **The integration test's narrowing check was a trap at the page cap.** `ListEvents` caps at 1000, so a grown fixture would compare 1000 with 1000 and pass or fail for reasons unrelated to the filter | Explicit `total >= 1000` guard that fails loudly, saying the fixture outgrew the cap |
| L-4 | **Documentation debt**: no PROGRESS section for the sub-phase, and both SPEC §9 and `Runs.tsx` cited **D-74** for the Runs list, which D-74 does not mention | This section, §0-4b2bii, and D-86 — which is what those two now cite |
| L-5 | **The `PAGE` comment's conclusion did not follow from its premise.** `limit=1000` *is* honoured; only *above* 1000 is downgraded | Rewritten: 200 is chosen for the reader, not forced by the cap |
| L-6 | **`Bar` was duplicated** in `RunDetail` and `Dashboard` — the exact drift `EventList`'s own doc comment argues against | Extracted to `components/Bar.tsx`; the ARIA attributes are the half that rots silently |

M-1 is the one worth remembering. The feature worked, the test passed, and the defect was invisible
at every scale the harness runs at — an unindexed column is only a bug once the table is large, which
is precisely when nobody is watching. **Making an existing field filterable is a schema decision, not
a plumbing one.**

---

## 5z. The user's outstanding to-do list

**Requested 2026-09-05: surface this when Phase 5 starts wrapping up.** These are the items only the
user can action; everything else outstanding is in §6 and is mine.

| # | Item | Why it is waiting on them | Time pressure |
|---|---|---|---|
| ~~U-1~~ | ~~**The manual browser pass**~~ — **done 2026-09-06**, six findings in §5m, all fixed | Confirmed working: never-run cards, logs, duplicate-target, the scheduling toggle, and a scheduled job firing on time | — |
| ~~U-1b~~ | ~~**The Schedule controls**~~ — **done 2026-09-06.** The next-run preview was showing the browser's zone only, which made a UTC server look wrong (§5m M-5); fixed | — |
| U-5 | **Re-check the six §5m fixes in a browser**, and the renamed build comes up at all | Only a browser can | None |
| ~~U-6~~ | ~~**The Webhooks & API tab**~~ — **done 2026-09-09**, all of it worked. The format selector and the seeded cn4m row are new since that pass but are cosmetic additions to a screen already exercised | — |
| ~~U-6-old~~ | ~~**The new Webhooks & API tab**~~ — create a token, confirm it is shown once and the curl examples work, regenerate and confirm the old one stops, add a callback URL and watch a run report to it | Only a browser can, and the one-shot token display is exactly the kind of thing that looks right until someone reloads the page | None |
| ~~U-2~~ | ~~**Leftover scope directories**~~ — **cleared 2026-09-06** with the user's agreement. 182 on Samba B plus a handful on Samba A; both shares are back to their seeded fixtures | — |
| ~~U-3~~ | ~~**The identifier rename**~~ — **done 2026-09-06** (D-99). ⚠️ The database filename changed, so an existing dev database is ignored rather than migrated: `mv <data-dir>/smbsync.db <data-dir>/cn4m-cascade.db` keeps existing targets and jobs, and their passwords still decrypt because `hkdfInfo` was left alone | — |
| ~~U-3-old~~ | ~~When to do the identifier half of the rename~~ (§8 T-2) — `smbsync.db`, the session cookie, `cmd/smbsync` | It is a judgement call about churn, not a technical question | **Yes.** Renaming `smbsync.db` is free while every installation is fresh and orphans a real database the moment one is not. The window closes on first deployment |
| ~~U-4~~ | ~~**The cosmetic rename**~~ — **done 2026-09-06** (D-99) | — |

`hkdfInfo` is **not** on this list and must not be renamed with the rest: it is the HKDF info string
the credential key is derived from, and changing it makes every stored password undecryptable with
nothing failing at build or test time (§8 T-2).

---

## 5k. Phase 5a review — findings and resolution

A fresh-context subagent reviewed the scheduler. **One critical data-loss bug, one high-severity
inconsistency, seven smaller findings; all fixed.** It also confirmed the parts most likely to be
wrong and were not: the 22-column `jobColumns`/`jobInsertArgs`/`scanJob`/`UpdateJob` plumbing is in
step (a mismatch there is silent corruption); `next_run_at` cannot leak into a PATCH and 400 a save;
the zero-time "date that never occurs" case is handled end to end; `NextRun` is strictly-after, so a
job cannot double-fire within a tick; and shutdown ordering in `main.go` is correct.

| # | Finding | Resolution |
|---|---|---|
| R-1 | **CRITICAL: every scheduled run executed with the job's own filter rules missing.** `DueJobs` loaded destinations but not `filter_rules`, and my comment claiming the runner reloads them was simply wrong — `resolveChains` re-reads rule *files*, but the rule *rows* come from the struct. For a mirror this is a **deletion**: an excluded path at the destination has no source counterpart, so with the exclusion gone the differ calls it extraneous and removes it | `DueJobs` now loads filters exactly as `GetJob` does |
| R-2 | **HIGH: the scheduler evaluated cron in UTC while the preview and the startup log claimed `time.Local`.** Invisible today, because the container has no `TZ` — and the job editor actively tells users to set one, at which point `0 2 * * *` would fire at 02:00 UTC while the UI promised 2am local | Everything evaluates in local time; `ParseSchedule` documents it, including the DST consequence |
| R-3 | **One exit criterion had no test** — "a scheduled run against an unavailable destination reaches a terminal state with no human input" | `TestScheduledRunAgainstADeadDestinationEndsWithoutAnyone`: prompt policy, blackholed server, nobody answers |
| R-4 | **A failed `SetNextRun` write still started the run.** `reschedule` reported only whether the *expression* parsed, so a transient `SQLITE_BUSY` left the job due and a single firing could produce two runs | A failed advance is now a reason not to start |
| R-5 | **`next_run_at <= ?` is a TEXT comparison** over RFC3339Nano, whose length varies: a stored `02:00:00Z` sorts *after* a bind value of `02:00:00.123Z`, so a firing whose second the tick landed in was delayed a tick | The bind value is truncated to the second |
| R-6 | **`@every 10s` was accepted** despite the comment saying sub-minute expressions are refused — it fires once per tick, not once per interval | Rejected, with the shortest interval named in the message |
| R-7 | **Duplicating a job created a second live scheduled backup** the moment Create returned — before the editor opened, against the original's destination | A duplicate arrives paused |
| R-8 | **`Shutdown` panicked on a second call and blocked for the caller's whole timeout if `Start` was never called** — and the test harness holds a built-but-unstarted scheduler | `sync.Once` plus a started guard |
| R-9 | Smaller: `ErrAlreadyRunning` logged at error rather than warn; the migration comment claimed a dashboard use that does not exist yet; the PATCH asymmetry between `schedule_cron` and `enabled` was undocumented; `robfig/cron` was marked indirect | All corrected |

**R-1 is the one to remember.** Every test passed with it live, including all six of the new
scheduler tests, because the fixtures had no job-scoped filters and the *global* exclusions still
loaded. It was found by reading, not by running. Two things made it invisible: the manual path
(`GetJob`) and the scheduled path (`DueJobs`) disagreed silently, and `deletionGuard` — which exists
precisely to withhold deletions from an untrustworthy filter chain — does not fire, because a chain
whose rules were never loaded is not *degraded*. It is healthy and empty. **A guard that keys on
"degraded" cannot catch "never loaded".**

Proved rather than argued: `TestScheduledRunAppliesTheJobsOwnFilters` was run against the pre-fix
code and reported the protected file deleted.

---

## 5m. What the 5a / Phase 4 manual pass caught (2026-09-06)

The browser pass found six things, five of them bugs no test could have caught and one a real
misreading of the UI's own vocabulary. Confirmed working: never-run cards, the logs screen,
duplicate-target, the scheduling toggle actually preventing a run, and a scheduled job firing on
time (`33 20 * * *` fired at 20:33).

| # | Report | Cause and fix |
|---|---|---|
| M-1 | **The target modal vanished whenever the browser lost or regained focus** | `<div className="backdrop" onClick={onClose}>` — the click that refocused the window landed on the backdrop and dismissed a half-filled form, password and all. Click-to-dismiss removed entirely; Escape added, because a dialog with no keyboard exit is its own trap |
| M-2 | **Re-saving a target after a failed test was refused: "name already in use" — its own** | `persist()` chose create-vs-update from the `target` prop, which never changes. "Save and test" *saves first*, so a new target whose test fails is already saved — and the next save tried to create it again. A `createdID` now records the first successful create, so the modal switches to updating that record. Kept separate from the `editing` flag, which also drives the title and reads `target`, still null. The server was never wrong: `TestUpdatingATargetKeepsItsOwnName` pins that |
| M-3 | **Preview left a run parked, so Run then said "already in progress"** | A preview holds the job until confirmed or timed out. Leaving the run page having been shown the plan now cancels it — a clear enough "no" to act on. Guarded so React StrictMode's simulated unmount cannot trigger it. The 409 also distinguishes a parked preview from a live run, because "in progress" was actively misleading for something making no progress |
| M-4 | **Saving a new job landed in the editor for the job just created** | "Save" on a new job reads as "I am done here". It returns to the jobs list; editing an existing job still stays put, where saving is a checkpoint |
| M-5 | **`16 2 * * *` previewed as 7:16pm** | Not a scheduler bug, and worth stating precisely: the scheduler and the preview *agree* (R-2 fixed that). The container has no `TZ`, so its local zone is UTC, and the browser rendered 02:16 UTC in UTC-7. The preview now shows the server-clock time **and** the browser equivalent, so the two numbers are visible together instead of one appearing wrong; `TZ` is passed through in both compose files |
| M-6 | **"Workers" was read as destination concurrency** | It is files-at-a-time *within* one destination; destination ordering is `parallel_destinations`, already off. Relabelled "Files at a time" with the distinction spelled out. Default lowered 4 → 1 as requested (D-98) — noting that this makes the *first* destination finish later, not sooner |

M-2 is the instructive one. The bug was a piece of state the UI never updated after an action that
succeeded, and every layer below it was correct — which is why nothing in an 78-test suite could
see it. **"Save and test" makes the create/update distinction change underneath the form**, and
that is the kind of thing only a person clicking twice will find.

---

## 5o. Reporting to cn4m (2026-09-06)

The user asked whether the outbound callbacks actually work with **cn4m, the parent system this
tool is a component of**. They did not, and would not have.

**The mismatch.** cn4m's `/suite/status` takes form fields — its reference client posts
`-d app=… -d message=… -d level=…` — and `notify.post` sent `Content-Type: application/json` with a
JSON body. cn4m would have received a payload containing none of the three fields it reads. Nothing
would have errored; the suite view would simply never have shown cascade.

Two further mismatches with cn4m's documented contract, both from its own client's docstring —
*"a cn4m that is slow, down, or missing entirely changes nothing about watching"*:

- **Failure noise.** Every failed delivery wrote a `run_event` at **warn** against the run. A seeded
  cn4m callback on a machine without cn4m would have put warnings on every run forever, which
  teaches people to ignore run warnings — a worse outcome than never noticing cn4m is down.
- **Retry shape.** Three attempts per event, per run, indefinitely. cn4m's client backs off from a
  minute, doubling, to half an hour, precisely so an absent endpoint is left alone.

**Where cn4m lives is a deployment question, so it is an environment variable.** The seed originally
hardcoded `localhost:2640` in the migration, taken from cn4m's own client — and testing against a
real cn4m at `10.10.20.10` showed immediately that the default is wrong the moment cascade is not on
the cn4m host. A migration cannot read the environment, so the row is no longer seeded in SQL: it is
created at startup by `store.EnsureCN4MWebhook` from `CN4M_STATUS_URL`, which defaults to localhost
and accepts `off`.

It **never overwrites an existing row**, which is the same rule `CN4M_ADMIN_PASSWORD` follows (D-66)
and for the same reason: an environment variable sitting in a compose file must not silently undo a
change someone made in the UI. The check is on the callback's *format*, not its address, so a hook
already re-pointed at a different cn4m still counts as "there is one".

**Built:** a per-callback `format` (migration `0010`). `json` is unchanged — signed, retried,
failures reported. `cn4m` is form-encoded, unsigned (that endpoint checks no signature, so requiring
one would be theatre), silent on failure, and circuit-broken on the cn4m client's own schedule. A
global cn4m callback at `http://localhost:2640/suite/status` is seeded enabled, since every
installation of this tool is part of a cn4m suite.

**The level vocabulary** (`idle`/`working`/`ok`/`warning`/`blocked`/`error`) was added to cn4m in
response to this work; the app reports as `cascade`. The mapping lives in exactly one place,
`notify.cn4mLevel`, and two tests guard it: one pins each outcome, the other asserts that every
level the code can emit — across every event and status, including an unknown future event — is one
cn4m accepts. A typo there is invisible until the suite shows the wrong colour for a backup.

**Verified against a live cn4m at 10.10.20.10:2640 (2026-09-09).** All five levels cascade emits are
accepted, `app=cascade` is accepted, and the endpoint answers **201** rather than 200 — which the
delivery path already treats as success, but only because it tests `2xx` rather than equality.

The important finding is what happens when a level is *wrong*: **cn4m answers 201 and silently
coerces it to `idle`**. Probed directly with `done`, `bogus_level` and an empty string; all three
came back as `idle`. So the `done` originally guessed for a completed run would have rendered every
successful backup as grey "nothing happening", and a typo on a failure would look identical — with
no error, no warning, and nothing in any log to notice. There is no runtime signal at all, which
promotes `TestEveryEmittedLevelIsInCN4MsVocabulary` from tidiness to the only thing standing between
a typo and a suite view that misreports whether backups run.

`warning` and `blocked` both earn their slots. Without `warning`, "one destination skipped because a
NAS was off" reports as unqualified green; without `blocked`, a run awaiting a decision with a
timeout running is indistinguishable from history. **cascade never sends `idle`** — it speaks only
when something happens, so a healthy app rests on `ok` rather than returning to grey. Deliberate,
and worth revisiting only if the suite wants a heartbeat.

**A real bug fell out of it.** Deciding what a *cancelled* run should report revealed that it
reported nothing: `execute` returned from the cancellation branch before reaching the notify block,
so anything told "started" was never told it ended and a cn4m row would sit on orange until the next
run. Cancellation is an outcome, not the absence of one.

---

## 5n. Phase 5b review — findings and resolution

A fresh-context subagent reviewed the webhook work with a security brief. **The security core held**
— it verified, rather than assumed, that the guard boundary is sound *structurally* and not just
empirically: the hooks sub-mux registers only the three hook handlers, so even total confusion about
which mux receives a request can produce at most a 404 or a hook handler, never a guarded handler
without a session. It also confirmed Go's `ServeMux` returns a 301 to the client on a non-canonical
path rather than re-dispatching internally, so `/api/hooks/../jobs` and friends cannot be walked
past the guard; that the token hash never enters Go; that `?token=` never reaches a log line; that
all four denial reasons are byte-identical; and that the 23-column select plumbing is in step.

Ten findings nonetheless, all fixed.

| # | Finding | Resolution |
|---|---|---|
| R-1 | **A queued run could start after `Shutdown()` had returned.** `start()` never checked `closing` — only `finish()` did, and the dequeue happens on a *new goroutine*, so `Shutdown` could complete its `wg.Wait()` in the gap. The successor would then run against tearing-down mounts and a closing database, leaving a `runs` row stuck in "running" that nothing would ever cancel | `closing` is now checked inside `start()`, under the same lock that reserves the job. New `ErrShuttingDown`, answered as 503 |
| R-2 | **Shutdown-time callbacks were never delivered**, and `Stop` did not drain despite saying so. The notifier was started on the *signal* context, so SIGTERM killed every worker before the runs being cancelled produced their `run_completed` events — the exact events the shutdown comment claimed were worth waiting for | Own context, cancelled after `Stop`. Workers now check the queue first and drain it on stop, instead of a three-way select that abandons the backlog at random |
| R-3 | **The `progress` event was never emitted.** The UI offered the checkbox, `min_interval_sec` had its own field, the throttle was implemented — and nothing ever fired it | Emitted from the existing flusher, built from the snapshot alone so the per-flush path does no database work |
| R-4 | **The rate limiter's map grew without bound**, and the comment claimed the leak had been avoided. Pruning only on an address's *next* request never reclaims one that never returns — and this is the endpoint designed to be anonymous | Sweep on record, plus a hard ceiling. **The first fix was incomplete and a test caught it**: sweeping expired entries does nothing against a burst of fresh addresses inside the window, so the sweep now also evicts oldest-first down to the cap |
| R-5 | **One bad client permanently 429'd every good poller at the same address** — and behind a NAT, proxy or Docker bridge every integration shares one | The token is verified *first*; the limiter now only decides what happens to a request that already failed. Brute force is bounded exactly as before |
| R-6 | **`?queue=1` could return a false 409 and lose the trigger.** If the run finished between `start()` failing and `QueueNext`, the handler answered "already in progress" when nothing was | Retries `start()` when nothing is running and nothing was queued |
| R-7 | **The documented status shape was incomplete**: no `destinations[].eta_sec`, and `eta_sec`/`throughput_bps` vanished entirely from a finished run — against an exit criterion that says "both during a run and after it". Per-destination counters also came from the lagging DB rows | Live per-destination counters merged from the snapshot, `eta_sec` per destination, and both keys present after a run with `-1` for "not computable" |
| R-8 | **`unavailable_policy_override` was documented in §8.1 and unimplemented** — and `DisallowUnknownFields` made sending it a hard 400, so a client following the spec verbatim failed | Implemented on a *copy* of the job, so a trigger cannot reconfigure it. `prompt` is refused explicitly rather than accepted and ignored: there is nobody to answer |
| R-9 | **A token holder can keep a job permanently unrunnable** by looping `preview:true`, since a parked preview holds the job. The credential's advertised power is "can start this job"; its real power includes "can stop it running" | Support kept — §8.1 offers it — but SPEC now states the consequence plainly, so handing out a token is an informed act |
| R-10 | Smaller: a dead `ErrNoToken` sentinel; `WebhooksFor`'s comment claimed it decrypted secrets (it never has); `notify.deliver` could nil-deref its DB in library code; the `r.notify` comment claimed an optimisation that never fires | All corrected or removed |

**R-4 is the one worth remembering.** Not the leak — the *fix*. I wrote a sweep, believed it, and
the test I wrote alongside it failed immediately: expiry-based cleanup is not a bound when the
attack is a burst inside the window. The finding was right and my first answer to it was wrong, and
only writing the assertion revealed that.

---

## 6. Open items for the next session

### Phase 5b

1. ~~Nothing exercises the hook rate limiter.~~ **Done** — `internal/api/hooks_test.go` covers the
   threshold, the bound and the never-failed case. Still untested *end to end* through HTTP: that
   the 21st bad token in five minutes returns 429 rather than 401 is asserted only at the unit
   level.
2. ~~A queued run is lost on restart with nothing telling the caller.~~ **Done** — the 202 body now
   says so.
3. **`current_files[].dest` is still missing** from the status payload (SPEC.md §8.1 lists it). The
   information is genuinely lost upstream: `progress.go` flattens `InFlight` across destinations, so
   with fan-out two destinations copying the same relpath appear as two identical entries with no
   way to tell them apart. Fixing it means carrying the destination through the snapshot.
4. **The webhooks UI is job-scoped only.** Global callbacks (`job_id` NULL) are supported by the
   store and the API and are *displayed* in the job tab, but there is no screen that creates one;
   Settings is the natural home.
5. **No delivery observability.** A callback that fails all three attempts writes a `run_event` at
   warn, which is right, but there is no per-webhook "last delivery" state — so a URL that has been
   silently failing for a week looks identical to one that has never fired.

### Phase 5a

1. **`prompt_timeout_sec` can be up to 86400.** A nightly job with `unavailable_policy: prompt` and
   a 24-hour timeout parks for a day when its destination is down, and because `ActiveForJob` stays
   true throughout, *every* firing in that window is skipped. The default is 600s so this needs
   deliberate misconfiguration, but the schedule UI should say so — an interactive policy on an
   unattended job is a combination worth warning about rather than only documenting.
2. **Nothing measures the tick's cost at scale.** `DueJobs` is partial-indexed and runs every 30s,
   which is fine for tens of jobs and unmeasured beyond that.
3. **The scheduler has no visibility in the UI.** A missed firing is a log line; a skipped
   overlapping run is a log line. Neither reaches the dashboard, so the place someone looks to ask
   "did last night run?" cannot answer "it was skipped because the previous one was still going".
   Worth a `run_event`-like surface, but it needs a run to attach to and a skipped run has none.

### Phase 4b-2b-ii

1. **The manual browser pass has not been run.** The checklist is in §0-4b2bii; it is the last gate
   on Phase 4.
2. **The global log's auto-refresh stops once the list has been paged past**, deliberately —
   re-fetching page 0 under a list someone has extended would drop everything below it. The
   consequence is that a user who presses "Load more" stops receiving new events until they refresh.
   Prepending only genuinely new rows would fix it properly; a note on the button would do for now.
3. **Offset paging over a newest-first list is approximate by construction.** Events written between
   pages shift the window, so a page can repeat rows (deduped by id) or, less visibly, skip them. A
   keyset cursor on `(ts, id)` is the real fix and would replace `EventFilter.Offset`.
4. **`//172.28.0.11/private` has accumulated ~170 leftover `fan-*` and `run-*` scope directories**
   from historical runs whose cleanup could not reach a blackholed server (`_ = os.RemoveAll(...)`
   swallows the failure). Harmless but unbounded, and it slows every listing of that share. Safe to
   delete — they are all test scope directories — but nothing should do so automatically.
5. **The blackhole cleanup is a single 20s `iptables -D` attempt** and can be defeated by the same
   blocked-fork condition it exists to clean up (§7b step 1). When it loses, the rule stays installed
   and every later test in that run fails against *both* servers. Retrying with a total budget rather
   than one attempt is the fix; the block is transient.
6. **Nothing measures the `dest` filter's query cost.** D-87 added the index by reading the plan, not
   by timing it; there is no large-table benchmark anywhere in the repo, so a future filter added to
   `run_events` could regress the same way without anything noticing.

### Phase 4b-2

1. **The prompt modal has no explicit dismiss control.** Both 409s close it and it no longer renders
   over a terminal run (§5f P2-8), but if some future state strands it the only way out is a reload.
   A close button that leaves the prompt unanswered would be honest — the fallback still fires.
2. **Nothing asserts `withheld_deletes` survives to the wire.** It is unit-tested in the differ and
   in `destPlanOf`, but no test reads it out of a JSON response, so a dropped assignment or a bad
   tag would ship silently. An integration case with an unreadable source directory would close it.
3. `ConfirmPlan` fetches the plan once when it opens and never refetches. Correct for a preview,
   which parks only after every destination is planned, but wrong if the component is reused for a
   live run.
4. ~~`Runs.tsx` is a placeholder front door, replaced by the dashboard in 4b-2b.~~ **Resolved
   2026-09-05** — the dashboard took over as the landing page; the screen itself stayed and is now
   permanent. See D-86.
5. **Files the server writes into the shared folder are root-owned on Linux**, because the container
   runs as root. Harmless on Docker Desktop (macOS/Windows), which maps ownership, but a Linux user
   syncing *into* `~/cn4m` will need `sudo chown`. Noted in the README. A `user:` mapping on the
   compose service would fix it properly and is worth doing before anyone deploys on Linux.

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
9. ~~`DestPlan` carries only counts, so a confirm gate over deletions shows no paths.~~ **Fixed in
   4b-2a** — `GET /api/runs/{id}/plan` (D-68) lists them individually, and the confirm screen renders
   them. *Original:*
   `progress.go:60-77` exposes `mkdirs / copies / deletes / rmdirs / copy_bytes` and conflict
   strings. A user confirming "412 to delete" cannot see *which* 412. SPEC.md §9 asks run detail to
   show "the action plan", and CLAUDE.md is explicit that removing data "is logged individually,
   never summarised". Needs the delete paths in the payload before 4b builds the modal.
10. ~~`/api/browse` has no entry cap and no aggregate bound.~~ **Fixed in 4b-2a** — `browseLimit`
    (2000) with `truncated`/`total`, and `browseBudget` (30s) over the whole listing so a share that
    dies partway stops the loop instead of paying the per-call timeout 2000 times.
    `TestBrowseCapsALargeListing` covers it. *Original:* Each `InfoBounded` call is
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

15. ~~`TestDestinationDisappearsMidRun` intermittently hangs instead of failing.~~ **Diagnosed and
    fixed 2026-09-04 — it was a production bug, not a flake.** See D-80: `Manager.Shutdown` could
    block forever in `forkExec`. The suspicion recorded below (that the test's own cleanup raced the
    blackhole) was **wrong**; the goroutine dump the `-timeout` produced pointed straight at the
    mounter. The lesson stands about the timeout being worth adding: three hangs produced no
    diagnostics at all, and the fourth produced the stack that solved it. *Original:* Seen twice —
    2026-09-01 and 2026-09-02 — against roughly a dozen passing runs, so the rate is order 1-in-6.
    Each time it wedges with its blackhole still installed and leaves CIFS mounts behind, parking
    the harness until `make harness-clean` runs.

    **The 2026-09-02 occurrence proved `-timeout 15m` works**: the suite panicked with a goroutine
    dump instead of hanging forever. The dump was then **lost to a shell filter** —
    `grep -E "^(--- |ok |FAIL|PASS|panic)"` kept the `panic:` line and discarded every stack frame
    under it. Next time, capture the full output and filter only for display; the stacks are the
    whole point of the timeout.

    Confirmed each time by what `harness-clean` reports: mounts under
    `/tmp/TestDestinationDisappearsMidRun*` plus a blackhole on 172.28.0.11. Suspicion, still
    unverified: the test's own cleanup races the blackhole it installed, so the drain never runs
    when the run wedges at the wrong moment.

    It is a **test-harness** flake, not a product defect — the last several full runs passed with
    identical engine code, and the two hangs bracket frontend-only changes.

### Phase 3

1. **Rule files are not validated at save time.** SPEC.md §6.5 asks for it; today a typo in a
   `target://` path or a JSON key is only discovered when the run fails. The run-time error is
   clear (that is the exit criterion), but the feedback belongs in the editor. Phase 4's UI is the
   natural home.
2. **§6.5's optional debug toggle to log every filtered path is not implemented.** Counts are.
3. ~~`handleFilterTest` returns 400 when the *source* is unavailable.~~ **Fixed in 4b-2a** —
   `runner.ErrSourceUnavailable` is wrapped at the resolve failure and mapped to 502
   `source_unavailable`; a bad rule still returns 400. `TestFilterTestReportsAnUnreachableSourceAsUnavailable`
   covers it.
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

## 7d. Two integration suites at once corrupt each other (2026-09-09)

I started a second `make test-integration` **51 seconds before the first finished**, and got a
failure that looked like a real regression:

```
TestOneDestinationOfflineCompletesPartial: status = success, want partial
could not remove the blackhole on 172.28.0.11: iptables: Bad rule (does a matching rule exist?)
```

`make test-integration` begins with `harness-clean`, which loops `iptables -D` over both Samba
addresses. So the starting suite **deleted the blackhole the running suite had just installed** —
the "offline" destination was reachable, the run succeeded, and the original suite's own cleanup
then found no rule to remove.

Same collision class as `make run` versus `make test-integration` (§7a), which is already known and
documented; this was two test suites rather than a server and a suite. Diagnosed by comparing
timestamps in the two logs rather than by reading the failure, which on its own reads exactly like a
genuine fan-out bug.

**Rule: one integration suite at a time, full stop.** The harness is a shared, stateful, singleton
thing — one pair of Samba containers, one iptables table, one `/tmp`. Waiting for a run to finish
costs ten minutes; misreading a corrupted result costs a great deal more.

---

## 7c. The rename skipped 70 tests silently (2026-09-06)

**A green suite that tested almost nothing.** The run after the `SMBSYNC_* → CN4M_*` rename reported
`ok ... 110.430s` and exit code 0 — with **9 PASS and 70 SKIP**. It read as success at a glance and
was worth nothing.

**Cause.** The env vars moved in `docker-compose.test.yml`, but a container's environment is fixed
at creation: the *running* dev container still had `SMBSYNC_TEST_SAMBA_A`. `env(t, ...)` found
nothing and every test needing a Samba host skipped itself, exactly as designed.

**The lesson is about the check, not the rename.** Counting `--- PASS` and `--- FAIL` was never
enough — 0 failures is also what a suite that ran nothing reports. **The pass count has to be
compared against the previous run**, and it had been: 78 the run before. Reading the number rather
than the verdict is what caught it.

**Then the recovery made it worse.** `docker compose up -d --force-recreate` recreated both Samba
containers and then failed to stop the dev container — *"tried to kill container, but did not
receive an exit event"* — leaving the harness with no dev container **and** no running Samba, and
`docker rm -f` could not kill it either. Same D-state wedge as §7a, now confirmed to survive
`rm -f` as that section predicted.

**Recovery used (§7a's ad-hoc container, with the new variable names):**

```
docker compose -f docker-compose.test.yml up -d samba-a samba-b
docker run -d --name cn4m-cascade-dev-tmp \
  --cap-add SYS_ADMIN --cap-add DAC_READ_SEARCH --cap-add NET_ADMIN \
  --security-opt apparmor:unconfined \
  -e CN4M_TEST_SAMBA_A=172.28.0.10 -e CN4M_TEST_SAMBA_B=172.28.0.11 \
  -e CN4M_TEST_OFFLINE=172.28.0.99 \
  -e ENCRYPTION_KEY=test-encryption-key-not-for-production -e TZ=UTC \
  -v "$PWD":/src -v cn4m-cascade_gomodcache:/go/pkg/mod \
  -v cn4m-cascade_gobuildcache:/root/.cache/go-build \
  -w /src --network cn4m-cascade_smbnet cn4m-cascade-dev sleep infinity
docker exec cn4m-cascade-dev-tmp sh -c 'cd /src && CGO_ENABLED=1 go test -race -tags integration ./test/'
```

**Resolved 2026-09-06**: the user restarted Docker Desktop and ran `make harness-up`. That cleared
the wedged container and rebuilt all three under the new names with the new `CN4M_TEST_*` variables
— confirming both that a daemon restart is the *only* fix and that it costs nothing, since the
harness is entirely defined by the compose file and two named volumes.

`DEV` in the Makefile is nonetheless overridable now, so the whole toolchain can work against an
ad-hoc container without editing anything the next time this happens:

```
make test-integration DEV="docker exec -i cn4m-cascade-dev-tmp"
```

**Fixed properly** so the silent version cannot recur: `make test-integration` now fails when the
harness environment is missing, rather than letting the suite skip its way to a pass — see D-101.

---

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

## 7b. The harness deleted a Samba share (2026-09-05)

Recorded in full because the failure looked like three different things before it looked like the
right one, and the wrong diagnoses were all plausible.

**Symptom.** An integration run reported 14 failures; a re-run reported 3 different ones. The second
set were all "the share listed no fixture files" — `hello.txt`, `ünïcodé-📁.txt` and `nested` were
gone from `//172.28.0.10/private`, along with the `.seeded` marker the entrypoint writes.

**The sequence, and why each step misleads:**
1. `TestDestinationDisappearsMidRun` blackholes Samba B. Its cleanup ran `iptables -D` and **timed
   out at 20s**, leaving the DROP rule installed. Same mechanism as D-80: `iptablesBounded` has to
   fork, and fork blocks in a process whose threads are parked in uninterruptible CIFS syscalls.
   *The cleanup that keeps the harness usable is itself vulnerable to the condition it cleans up.*
2. Every later test then failed with `mounting //172.28.0.10/private timed out` — **Samba A**, which
   was never blackholed. Not a contradiction: the leftover mounts to the dead B kept fork blocked, so
   mounting anything timed out. Reading the IP in the error as the cause points at the wrong server.
3. The 20s clean timeouts rather than an indefinite hang are `runBounded` **working**. Easy to
   misread as the regression.
4. The actual deletion: `buildHarness` put the mount root under `t.TempDir()`, whose cleanup is a
   `RemoveAll`. Both unmounts had been abandoned on their deadlines, so both mounts were still
   attached. `RemoveAll` recursed into each. Against blackholed B it failed —
   `TempDir RemoveAll cleanup: readdirnames .../mnt/d5850...: host is down`, the one line in the log,
   and it names the mount that *survived*. Against reachable A it **succeeded**, emptying the share
   silently.

**Why it had never been seen** despite the harness being months old: it needs an unmount to be
abandoned, which needs a server to die mid-run, which only `TestDestinationDisappearsMidRun`
arranges — and its own destination is the blackholed one, so the visible half always fails. The
share that gets deleted is the *source*, on the healthy server, which nothing complains about.

**Fixed** by D-90, structurally: the mount root left `t.TempDir()`, and nothing in its teardown may
use `RemoveAll`. `os.Remove` on a directory succeeds only when it is empty, so a live mount returns
EBUSY and recursion is impossible rather than merely guarded against.

**Still open** (§6, Phase 4b-2b-ii): the `iptables -D` of step 1 is still a single 20s attempt. The
block is transient, so retrying with a budget would very likely hold — and until it does, one wedged
blackhole test can still poison every test after it in the same run.

**Recovery, if it happens again:** `make harness-clean`, then
`docker compose -f docker-compose.test.yml restart samba-a` (or `samba-b`) — the entrypoint's seed
is guarded by `.seeded`, so a restart re-creates the fixtures only if they are actually missing.

---

## 7. Environment notes

- **No Go toolchain on the host** (macOS 14 / arm64) and `mount.cifs` is Linux-only, so every build,
  lint and test runs in the dev container. The `Makefile` targets are `docker compose` wrappers.
- **CIFS in the Docker Desktop kernel: confirmed working** (was blocker B-4).
- Docker Desktop's daemon hung for ~40 minutes during this session and needed a restart. If
  `docker` commands produce no output at all, that is the failure mode — restart Docker Desktop.
- `network_mode: host` does not work on Docker Desktop for macOS; the test compose uses a bridge.

## 8. TODOs without a home

Three requested by the user 2026-09-05, recorded here because none belongs to Phase 4 and two need a
decision before they can be planned. **Do not start any of them without raising them first** — the
first two change SPEC.md, and CLAUDE.md is explicit that SPEC is the source of truth.

### T-1. Portable configuration: export a whole setup and import it elsewhere

Targets, jobs (with destinations and filter rules) and the global exclusions, moved to a second
installation without retyping them. Nothing in SPEC.md covers this today, so it needs a §-level
decision, not just an implementation.

**The crux is credentials, and it has no free answer.** A target's password is encrypted with a key
derived from `ENCRYPTION_KEY` via HKDF (`internal/secrets`), so:
- Exporting the stored ciphertext produces a file that is **useless on the other machine** — a
  different `ENCRYPTION_KEY` derives a different key and nothing decrypts.
- Exporting plaintext produces a file of working SMB passwords sitting in a downloads folder. That
  is against the spirit of CLAUDE.md's credential rule even though the rule names command lines and
  logs specifically.

**Settled 2026-09-05 (D-91): the export omits every password and names the targets that need one**,
so the import screen is a short list of "these four targets need their password re-entered".
A passphrase-encrypted export (the passphrase supplied by the user, not derived from
`ENCRYPTION_KEY`) is the natural refinement if re-entry proves annoying, and can be added later
without changing the file format's other half.

Other things the design has to settle:
- **Identity.** Jobs reference targets by id. Either the export references targets by *name* and the
  import re-links them, or it carries ids and the import preserves them. Names are legible in a file
  someone may hand-edit; ids are unambiguous. Names, with a collision check on import, is probably
  right — but it makes target names load-bearing, which they are not today.
- **What is excluded.** Runs, `run_events`, sessions and the admin password are install-local and
  must not travel.
- **Merge or replace.** Importing into a populated install is the case that can destroy work. A
  merge that silently overwrites a job of the same name is the failure mode; a preview of what will
  change, in the manner of the run preview, is the shape that fits this codebase.
- **Format.** YAML if it is meant to be hand-edited and version-controlled, JSON if it is only ever
  round-tripped through the UI. The request mentioned both a config folder *and* a save/export GUI;
  they are compatible, but only the first requires the file to be pleasant to read.

Suggested home: **Phase 6**, alongside the other operator-facing work. It is not a blocker for
Phase 5.

### T-2. Rename "SMB Sync" to "cn4m cascade"

The official name of the utility. The mentions split into two groups with very different costs.

**Cosmetic — safe to change at any time (4 places):** `web/index.html`'s `<title>`, the `brand` span
in `web/src/App.tsx`, and the headings in `README.md` and `SPEC.md`.

**Identifiers — each carries a real cost:**
| Identifier | Where | Cost of renaming |
|---|---|---|
| `hkdfInfo = "smbsync-target-creds-v1"` | `internal/secrets/secrets.go:21` | **Do not rename.** It is the HKDF info string the credential key is derived from. Changing it makes every stored target password undecryptable — and nothing would fail at build, lint or test time; targets would simply start failing to mount. If it ever must change it needs a re-encryption migration and a `-v2` suffix, which is what the existing `-v1` was for |
| `smbsync.db` | `internal/config/config.go:39` | Orphans an existing database. Free *today* — every install is fresh (see the seeded-exclusions decision) — and expensive the moment one is not |
| `smbsync_session` | `internal/api/auth.go:16` | Logs everyone out once. Harmless |
| `cmd/smbsync` | Makefile, Dockerfile, docs | Pure churn; touches the build in several places at once |
| `SMBSYNC_TEST_SAMBA_A/B` | harness, compose | Test-only |
| `smbsync-samba-a` etc. | `docker-compose.test.yml` | Container names; cosmetic but requires `harness-down` first |

Recommendation: do the cosmetic four now, and the identifiers as one deliberate commit **while
installs are still fresh** — with `hkdfInfo` explicitly left alone and a comment saying why.

### T-3. Run the whole thing under Docker rather than `make run`

**This is not a new feature — SPEC.md §10 already specifies it** (multi-stage Dockerfile, the
production `docker-compose.yml`, `network_mode: host`, `cap_add`, the `./data` volume, graceful
shutdown on SIGTERM). What is missing is that **§11 assigns §10 to no phase at all**: every phase
from 1 to 6 is about features, and packaging is named in none of them. That is a gap in the
document, not a decision anyone made.

`make run` today is `docker compose exec dev go run ./cmd/smbsync` — the *dev* container, which is
also what the test harness uses. That is the direct cause of a recurring problem in §7a: `make run`
and `make test-integration` share one container and collide, which is how the Error 137 incidents
happened. A real image would separate them.

Recommendation: make §10 an explicit **Phase 6** deliverable and say so in §11. Worth doing before
anyone deploys, and it pairs naturally with the Linux file-ownership item in §6 (Phase 4b-2 item 5),
which is a `user:` mapping on the same compose service.

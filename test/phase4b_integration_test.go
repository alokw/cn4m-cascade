//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// rawGet issues a GET and returns the status, content type and body without
// assuming JSON — the SPA routes return HTML, which h.do cannot parse.
func (h *harness) rawGet(path string, anon bool) (int, string, string) {
	h.t.Helper()

	client := h.client
	if anon {
		client = h.anonClient
	}

	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatalf("building the request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("reading the response: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// The security-relevant assertion of Phase 4b-1. Serving the SPA moved the
// session guard from "wraps everything" to "mounted on /api/", so that the
// shell can load for a browser with no cookie — the login screen is part of
// the SPA. This pins both halves of that: the shell became public, and the
// data did not.
func TestStaticShellIsPublicButAPIIsNot(t *testing.T) {
	h := newHarness(t, nil)

	// With no cookie at all.
	status, ctype, body := h.rawGet("/", true)
	if status != http.StatusOK {
		t.Fatalf("anonymous GET / = %d, want 200 — the SPA shell must load without a session: %s", status, body)
	}
	if !strings.Contains(ctype, "text/html") {
		t.Fatalf("anonymous GET / content-type = %q, want text/html", ctype)
	}
	if !strings.Contains(body, `id="root"`) {
		t.Fatalf("anonymous GET / did not return the SPA shell: %q", truncate(body))
	}

	// The data behind it is still closed.
	for _, path := range []string{
		"/api/targets",
		"/api/jobs",
		"/api/runs",
		"/api/logs",
		"/api/browse?target_id=x",
		"/api/ws",
	} {
		status, _, body := h.rawGet(path, true)
		if status != http.StatusUnauthorized {
			t.Fatalf("anonymous GET %s = %d, want 401 — the session guard must still cover it: %s",
				path, status, truncate(body))
		}
		if !strings.Contains(body, "unauthenticated") {
			t.Fatalf("anonymous GET %s did not return the unauthenticated envelope: %s", path, truncate(body))
		}
	}

	// And the auth endpoints stay reachable, or nobody could ever sign in.
	if status, _, _ := h.rawGet("/api/auth/session", true); status != http.StatusOK {
		t.Fatalf("anonymous GET /api/auth/session = %d, want 200", status)
	}
	if status, _, _ := h.rawGet("/healthz", true); status != http.StatusOK {
		t.Fatalf("anonymous GET /healthz = %d, want 200", status)
	}
}

// Deep links have to survive a cold load: a user pasting a run URL gets the
// shell and the client router takes over. A missing asset must not, because
// HTML served for a .js request surfaces as a syntax error somewhere
// unrelated to the actual fault.
func TestSPADeepLinksAndMissingAssets(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{"/targets", "/runs/does-not-exist", "/jobs/1/edit"} {
		status, ctype, body := h.rawGet(path, false)
		if status != http.StatusOK || !strings.Contains(ctype, "text/html") {
			t.Fatalf("GET %s = %d (%s), want 200 text/html", path, status, ctype)
		}
		if !strings.Contains(body, `id="root"`) {
			t.Fatalf("GET %s did not return the SPA shell", path)
		}
	}

	if status, _, _ := h.rawGet("/assets/definitely-not-built.js", false); status != http.StatusNotFound {
		t.Fatalf("GET a missing asset = %d, want 404 — it must not fall back to the shell", status)
	}
}

// The real Vite build is what ships, so assert the embedded one is real rather
// than the placeholder: index.html must reference a hashed bundle under the
// asset directory, and that bundle must actually be served.
func TestEmbeddedBuildIsTheRealSPA(t *testing.T) {
	h := newHarness(t, nil)

	_, _, shell := h.rawGet("/", true)
	if !strings.Contains(shell, "/assets/") {
		t.Fatalf("the embedded index.html references no bundle — is this the placeholder? %s", truncate(shell))
	}

	asset := betweenQuotes(shell, "/assets/")
	if asset == "" {
		t.Fatalf("could not find an asset URL in the shell: %s", truncate(shell))
	}
	status, ctype, _ := h.rawGet(asset, true)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 — the shell references an asset that is not served", asset, status)
	}
	if strings.Contains(ctype, "text/html") {
		t.Fatalf("GET %s returned HTML (%s) — the asset branch fell through to the shell", asset, ctype)
	}
}

// betweenQuotes pulls the first quoted string containing needle out of s.
func betweenQuotes(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	start := strings.LastIndexAny(s[:i], `"'`)
	end := strings.IndexAny(s[i:], `"'`)
	if start < 0 || end < 0 {
		return ""
	}
	return s[start+1 : i+end]
}

func truncate(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}

// localTarget is the payload the UI's target modal sends for a local folder.
// The server validates the two types as mutually exclusive, so the SMB fields
// are sent empty rather than omitted — that is what lets an existing SMB
// target be switched to local without being rejected.
func localTarget(name, path string) map[string]any {
	return map[string]any{
		"name": name, "type": "local", "local_path": path,
		"host": "", "share": "", "username": "", "password": "",
	}
}

// A local folder as the *source* of a job, mirrored to an SMB share. The
// storage provider has had a LocalStorage branch since Phase 1 and the store
// has validated the type for as long, but nothing exercised the path
// end-to-end — the UI could not create one until the target modal grew a type
// selector, so the whole combination was untested.
func TestLocalSourceMirrorsToSMB(t *testing.T) {
	h := newHarness(t, nil)

	srcRoot := t.TempDir()
	seedTree(t, srcRoot, map[string]int{"a.txt": 64, "nested/b.bin": 2048})

	scope := fmt.Sprintf("local-%d", time.Now().UnixNano())
	srcID := h.createTarget(localTarget(uniqueName("local-src"), srcRoot))
	dstID := h.createTarget(smbTarget(uniqueName("dst"), sambaA(t), shareGuest, "", ""))

	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		t.Fatalf("creating the destination subpath: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dstRoot) })

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("local-to-smb"),
		"source_target_id": srcID,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 60*time.Second)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("local-source run = %v, want success: %v", got, run)
	}
	assertSMBTreesMatch(t, srcRoot, dstRoot)

	// A local target needs no mount and no credentials, so testing it must
	// succeed without either — and report the folder as its root.
	status, body := h.do(http.MethodPost, "/api/targets/"+srcID+"/test", nil)
	if status != http.StatusOK {
		t.Fatalf("testing a local target = %d, want 200: %v", status, body)
	}
	if got, _ := body["root"].(string); got != srcRoot {
		t.Fatalf("local target root = %q, want %q", got, srcRoot)
	}
}

// Switching an existing SMB target to local has to clear host, share and
// username, which the server rejects if they survive. This is the payload the
// modal sends; if it regresses, editing a target's type fails with a
// validation error that reads as if the user did something wrong.
func TestTargetCanBeSwitchedBetweenSMBAndLocal(t *testing.T) {
	h := newHarness(t, nil)

	id := h.createTarget(smbTarget(uniqueName("switcher"), sambaA(t), shareCredentialed, userName, userPassword))

	local := t.TempDir()
	status, body := h.do(http.MethodPatch, "/api/targets/"+id, localTarget("switched", local))
	if status != http.StatusOK {
		t.Fatalf("switching SMB → local = %d, want 200: %v", status, body)
	}
	if got := body["type"]; got != "local" {
		t.Fatalf("type after the switch = %v, want local", got)
	}
	if got := body["local_path"]; got != local {
		t.Fatalf("local_path = %v, want %q", got, local)
	}
	// The credentials must be gone: a stored password with no username is a
	// state the server refuses if the target is ever switched back.
	if got, _ := body["has_password"].(bool); got {
		t.Fatal("the password survived the switch to a local target")
	}
	if got, _ := body["host"].(string); got != "" {
		t.Fatalf("host survived the switch: %q", got)
	}

	// And back again.
	back := smbTarget("switched-back", sambaA(t), shareGuest, "", "")
	back["local_path"] = ""
	if status, body := h.do(http.MethodPatch, "/api/targets/"+id, back); status != http.StatusOK {
		t.Fatalf("switching local → SMB = %d, want 200: %v", status, body)
	}
}

// The admin password has no policy: a blank one is a supported choice for a
// closed network (README, "Admin password"). This is the whole flow through
// the API — set it, then actually sign in with it — because a blank password
// that could be set but not used would be worse than refusing it.
func TestBlankAndShortAdminPasswordsWork(t *testing.T) {
	for _, password := range []string{"", "x", "ab"} {
		t.Run("password="+password, func(t *testing.T) {
			h := newHarnessNoSignIn(t)

			status, body := h.do(http.MethodPost, "/api/auth/setup", map[string]any{"password": password})
			if status != http.StatusOK {
				t.Fatalf("setup with %q = %d, want 200: %v", password, status, body)
			}

			// Setup must close behind us, or anyone could claim the instance.
			if status, body := h.do(http.MethodPost, "/api/auth/setup",
				map[string]any{"password": "someone-else"}); status != http.StatusConflict {
				t.Fatalf("second setup = %d, want 409: %v", status, body)
			}

			// A fresh client, so the session cookie from setup cannot mask a
			// broken login.
			fresh := h.anonJar(t)
			status, body = fresh.do(http.MethodPost, "/api/auth/login", map[string]any{"password": password})
			if status != http.StatusOK {
				t.Fatalf("login with %q = %d, want 200: %v", password, status, body)
			}
			if status, body := fresh.do(http.MethodGet, "/api/targets", nil); status != http.StatusOK {
				t.Fatalf("after login, GET /api/targets = %d, want 200: %v", status, body)
			}

			// Still a real credential: the wrong password is refused.
			other := h.anonJar(t)
			if status, _ := other.do(http.MethodPost, "/api/auth/login",
				map[string]any{"password": password + "-wrong"}); status != http.StatusUnauthorized {
				t.Fatalf("login with the wrong password = %d, want 401", status)
			}
		})
	}
}

// The confirm screen's whole purpose: a preview must be able to name the files
// it is about to delete, not just count them (CLAUDE.md forbids summarising a
// deletion; SPEC.md §11's 4b-2 criteria require the paths).
func TestRunPlanNamesTheFilesItWillDelete(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"keep.txt": 10})

	// Extraneous at the destination, so a mirror plans to remove them.
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	seedTree(t, dstRoot, map[string]int{
		"keep.txt":      10,
		"gone.txt":      20,
		"stale/old.bin": 30,
	})

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("plan-paths"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"prompt_timeout_sec": 60,
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	_, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", map[string]any{"preview": true})
	runID, _ := body["id"].(string)
	h.awaitRunStatus(t, runID, 30*time.Second, string(store.RunAwaitingConfirmation))

	status, plan := h.do(http.MethodGet, "/api/runs/"+runID+"/plan", nil)
	if status != http.StatusOK {
		t.Fatalf("GET plan = %d, want 200: %v", status, plan)
	}

	dests, _ := plan["destinations"].([]any)
	if len(dests) != 1 {
		t.Fatalf("expected one destination in the plan, got %v", plan["destinations"])
	}
	d, _ := dests[0].(map[string]any)

	deletes := pathsOf(t, d["deletes"])
	// Both extraneous files, and nothing that belongs to the source.
	for _, want := range []string{"gone.txt", "stale/old.bin"} {
		if !slices.Contains(deletes, want) {
			t.Fatalf("the plan does not name %q among its deletions: %v", want, deletes)
		}
	}
	if slices.Contains(deletes, "keep.txt") {
		t.Fatalf("the plan would delete a file present at the source: %v", deletes)
	}

	// The count beside the list has to match the list, or the screen lies.
	_, detail := h.do(http.MethodGet, "/api/runs/"+runID, nil)
	progress, _ := detail["progress"].(map[string]any)
	plans, _ := progress["plans"].([]any)
	first, _ := plans[0].(map[string]any)
	if got, _ := first["deletes"].(float64); int(got) != len(deletes) {
		t.Fatalf("DestPlan reports %v deletes but the plan lists %d: %v", first["deletes"], len(deletes), deletes)
	}

	if status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm = %d: %v", status, body)
	}
	if run := h.awaitRun(t, runID, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("confirmed run = %v", run["status"])
	}
	assertSMBTreesMatch(t, srcRoot, dstRoot)
}

// Once a run finishes its plan is gone — nothing persists one. That has to
// read as "this run moved on", not as a server error, or a stale confirm tab
// shows a scary failure for an ordinary race.
func TestRunPlanIsGoneOnceTheRunFinishes(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 10})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("plan-gone"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	h.awaitRun(t, runID, 60*time.Second)

	status, body := h.do(http.MethodGet, "/api/runs/"+runID+"/plan", nil)
	if status != http.StatusConflict {
		t.Fatalf("plan of a finished run = %d, want 409: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "no_plan" {
		t.Fatalf("error code = %v, want no_plan", errObj["code"])
	}

	// And a run that never existed is still a 404, not a 409.
	if status, _ := h.do(http.MethodGet, "/api/runs/does-not-exist/plan", nil); status != http.StatusNotFound {
		t.Fatalf("plan of an unknown run = %d, want 404", status)
	}
}

// A path picker must not try to render a share root with a million entries,
// and must say when it truncated rather than presenting a partial listing as
// the whole directory.
func TestBrowseCapsALargeListing(t *testing.T) {
	h := newHarness(t, nil)

	targetID := h.createTarget(smbTarget(uniqueName("browse-cap"), sambaA(t), shareGuest, "", ""))
	root := h.mountFor(t, targetID)

	scope := fmt.Sprintf("cap-%d", time.Now().UnixNano())
	dir := filepath.Join(root, scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the fixture directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// One more than the cap, so truncation is exercised by exactly one entry.
	const want = 2000
	for i := range want + 1 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("seeding entry %d: %v", i, err)
		}
	}
	// A directory whose name sorts *after* every one of those files. ReadDir
	// returns entries in name order, so capping before the dirs-first sort
	// would drop it — leaving a picker showing 2000 files and nothing to
	// navigate into, in the one directory the user is trying to get out of.
	if err := os.MkdirAll(filepath.Join(dir, "zzz-subdir"), 0o755); err != nil {
		t.Fatalf("seeding the directory: %v", err)
	}

	status, body := h.do(http.MethodGet, "/api/browse?target_id="+targetID+"&path="+scope, nil)
	if status != http.StatusOK {
		t.Fatalf("browse = %d, want 200: %v", status, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != want {
		t.Fatalf("browse returned %d entries, want it capped at %d", len(entries), want)
	}
	if truncated, _ := body["truncated"].(bool); !truncated {
		t.Fatal("a capped listing did not report truncated; the UI would show it as complete")
	}
	if total, _ := body["total"].(float64); int(total) != want+2 {
		t.Fatalf("total = %v, want %d", body["total"], want+2)
	}

	// Directories must survive the cap, and come first.
	first, _ := entries[0].(map[string]any)
	if isDir, _ := first["is_dir"].(bool); !isDir {
		t.Fatalf("the first entry is not a directory: %v — directories must sort before the cap", first)
	}
	if name, _ := first["name"].(string); name != "zzz-subdir" {
		t.Fatalf("the directory did not survive truncation; first entry is %v", first["name"])
	}
}

// A share being down is not a malformed request. Reporting it as 400 made the
// Filters tab say "your request was malformed" about an unplugged NAS.
func TestFilterTestReportsAnUnreachableSourceAsUnavailable(t *testing.T) {
	h := newHarness(t, nil)

	srcID := h.createTarget(smbTarget(uniqueName("offline-src"), offlineIP(t), shareGuest, "", ""))
	dstID := h.createTarget(smbTarget(uniqueName("dst"), sambaA(t), shareGuest, "", ""))

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("filter-offline"),
		"source_target_id": srcID,
		"destinations":     []map[string]any{{"dest_target_id": dstID}},
		"filters": []map[string]any{{
			"direction": "exclude", "source": "inline", "patterns": []string{"*.tmp"},
		}},
	})

	status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/filter-test", nil)
	if status != http.StatusBadGateway {
		t.Fatalf("filter-test against an unreachable source = %d, want 502: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "source_unavailable" {
		t.Fatalf("error code = %v, want source_unavailable: %v", errObj["code"], body)
	}
}

// pathsOf pulls the relpath out of a plan's action list.
func pathsOf(t *testing.T, raw any) []string {
	t.Helper()
	list, _ := raw.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		m, _ := item.(map[string]any)
		if p, _ := m["relpath"].(string); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// A job fetched from the API cannot be fed straight back into PATCH: decodeJSON
// sets DisallowUnknownFields, and the response carries server-owned fields
// (id, created_at, position, job_id) that jobPayload does not accept — on the
// job itself and on every nested destination and filter rule.
//
// This pins the contract the frontend's toJobPayload() exists to satisfy. It
// asserts both directions, because only asserting the happy path would let
// someone "fix" a future 400 by loosening DisallowUnknownFields, which is what
// stops a typo in a field name from silently not being applied.
func TestJobFetchMustBeProjectedBeforePatching(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, _ := previewFixture(t, h)

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("roundtrip"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": "exclude", "source": "inline", "patterns": []string{"*.tmp"},
		}},
	})

	_, fetched := h.do(http.MethodGet, "/api/jobs/"+jobID, nil)

	// Verbatim: refused, because of the fields the server owns.
	status, body := h.do(http.MethodPatch, "/api/jobs/"+jobID, fetched)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH of a verbatim fetched job = %d, want 400 — if this now succeeds, "+
			"DisallowUnknownFields was removed and a misspelled field would be silently dropped: %v",
			status, body)
	}

	// Projected to exactly what jobPayload accepts: succeeds, and the filter
	// rule survives.
	payload := projectJobPayload(fetched)
	status, body = h.do(http.MethodPatch, "/api/jobs/"+jobID, payload)
	if status != http.StatusOK {
		t.Fatalf("PATCH of a projected job = %d, want 200: %v", status, body)
	}
	filters, _ := body["filters"].([]any)
	if len(filters) != 1 {
		t.Fatalf("the filter rule did not survive the round trip: %v", body["filters"])
	}
}

// projectJobPayload mirrors the frontend's toJobPayload(): keep exactly the
// fields jobPayload declares, on the job and on each nested row.
func projectJobPayload(job map[string]any) map[string]any {
	keep := []string{
		"name", "source_target_id", "source_subpath", "mode", "compare",
		"compare_tolerance_sec", "ignore_dst_hour", "workers", "log_every_file",
		"on_error", "delete_policy", "unavailable_policy", "prompt_timeout_sec",
		"prompt_fallback", "parallel_destinations",
	}
	out := map[string]any{}
	for _, k := range keep {
		if v, ok := job[k]; ok {
			out[k] = v
		}
	}

	dests := []map[string]any{}
	for _, raw := range asSlice(job["destinations"]) {
		d, _ := raw.(map[string]any)
		dests = append(dests, map[string]any{
			"dest_target_id": d["dest_target_id"], "dest_subpath": d["dest_subpath"],
		})
	}
	out["destinations"] = dests

	rules := []map[string]any{}
	for _, raw := range asSlice(job["filters"]) {
		f, _ := raw.(map[string]any)
		rule := map[string]any{
			"scope": f["scope"], "direction": f["direction"], "source": f["source"],
			"case_sensitive": f["case_sensitive"], "on_error": f["on_error"],
		}
		for _, k := range []string{"scope_target_id", "patterns", "file_path", "json_key"} {
			if v, ok := f[k]; ok {
				rule[k] = v
			}
		}
		rules = append(rules, rule)
	}
	out["filters"] = rules
	return out
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// Deleting a job that has run history used to fail with a raw
// "FOREIGN KEY constraint failed (787)" shown verbatim to the user: runs.job_id
// references jobs(id) without ON DELETE CASCADE, unlike every other child
// table. The feature was broken for every job that had ever run — which is
// every job anyone would want to delete.
func TestDeletingAJobRemovesItsRunHistory(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 10})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("deletable"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeUpdate),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	h.awaitRun(t, runID, 60*time.Second)

	if status, body := h.do(http.MethodDelete, "/api/jobs/"+jobID, nil); status != http.StatusNoContent {
		t.Fatalf("deleting a job with run history = %d, want 204: %v", status, body)
	}

	if status, _ := h.do(http.MethodGet, "/api/jobs/"+jobID, nil); status != http.StatusNotFound {
		t.Fatalf("the job survived deletion: %d", status)
	}
	// The history goes with it — an orphaned run would point at a job that no
	// longer exists.
	if status, _ := h.do(http.MethodGet, "/api/runs/"+runID, nil); status != http.StatusNotFound {
		t.Fatalf("the run survived its job's deletion: %d", status)
	}
	if events := h.eventsOf(t, jobID); len(events) != 0 {
		t.Fatalf("run events survived: %d", len(events))
	}
}

// A job cannot be deleted while it is running: the runner holds it in memory,
// and the history the delete would remove is still being written.
func TestJobCannotBeDeletedWhileRunning(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 10})

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("busy"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"prompt_timeout_sec": 30,
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	// A preview parks, which is the easiest way to hold a job open long enough
	// to attempt the delete deterministically.
	_, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", map[string]any{"preview": true})
	runID, _ := body["id"].(string)
	h.awaitRunStatus(t, runID, 30*time.Second, string(store.RunAwaitingConfirmation))

	status, resp := h.do(http.MethodDelete, "/api/jobs/"+jobID, nil)
	if status != http.StatusConflict {
		t.Fatalf("deleting a running job = %d, want 409: %v", status, resp)
	}
	errObj, _ := resp["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "job_running" {
		t.Fatalf("error code = %v, want job_running", errObj["code"])
	}

	// And once it is over, the delete goes through.
	h.awaitRun(t, runID, 60*time.Second)
	if status, body := h.do(http.MethodDelete, "/api/jobs/"+jobID, nil); status != http.StatusNoContent {
		t.Fatalf("deleting after the run ended = %d, want 204: %v", status, body)
	}
}

// eventsOf returns every log event recorded for a job.
func (h *harness) eventsOf(t *testing.T, jobID string) []any {
	t.Helper()
	_, body := h.do(http.MethodGet, "/api/logs?job_id="+jobID, nil)
	events, _ := body["events"].([]any)
	return events
}

// Under the `prompt` policy a missing destination folder is a question, not a
// silent creation: a mistyped subpath would otherwise be brought into
// existence and synced into, which looks exactly like success. Answering
// "create" makes it and the run proceeds.
func TestMissingDestinationFolderIsPromptedAndCreatedOnAnswer(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 9})

	fresh := scope + "/ask-first"
	dstRoot := filepath.Join(h.mountFor(t, dstID), fresh)
	t.Cleanup(func() { _ = os.RemoveAll(dstRoot) })

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("ask-create"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		// Deliberately NOT unavailable_policy: prompt. A missing folder asks
		// on its own terms, which is the whole point of the separate setting.
		"create_dest_dirs":   string(store.CreateDirsAsk),
		"prompt_timeout_sec": 120,
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": fresh}},
	})

	runID := h.runJob(t, jobID)
	h.awaitDestStatus(t, runID, dstID, 60*time.Second, string(store.DestAwaitingPrompt))

	// Nothing may have been created merely by asking.
	if _, err := os.Stat(dstRoot); !os.IsNotExist(err) {
		t.Fatalf("the folder was created before anyone answered (stat err: %v)", err)
	}

	// The prompt must say creating is an option, so the UI can offer it
	// without guessing from the message text.
	_, detail := h.do(http.MethodGet, "/api/runs/"+runID, nil)
	progress, _ := detail["progress"].(map[string]any)
	var canCreate bool
	for _, raw := range progress["destinations"].([]any) {
		d, _ := raw.(map[string]any)
		if d["dest_target_id"] == dstID {
			canCreate, _ = d["prompt_can_create"].(bool)
		}
	}
	if !canCreate {
		t.Fatalf("the prompt does not offer creation: %v", progress["destinations"])
	}

	status, resp := h.do(http.MethodPost, "/api/runs/"+runID+"/prompt",
		map[string]any{"action": "create", "dest_target_id": dstID})
	if status != http.StatusOK {
		t.Fatalf("answering create = %d, want 200: %v", status, resp)
	}

	if run := h.awaitRun(t, runID, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("run after creating the folder = %v: %v", run["status"], run)
	}
	assertSMBTreesMatch(t, srcRoot, dstRoot)
}

// With nobody to answer, the fallback must not create anything: an unattended
// run is exactly where a typo would go unnoticed.
func TestUnansweredMissingFolderPromptCreatesNothing(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 9})

	fresh := scope + "/never-answered"
	dstRoot := filepath.Join(h.mountFor(t, dstID), fresh)

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("no-answer-create"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"create_dest_dirs":   string(store.CreateDirsAsk),
		"prompt_timeout_sec": 5, // the floor
		"prompt_fallback":    string(store.FallbackSkip),
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": fresh}},
	})

	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 90*time.Second)
	if run["status"] != string(store.RunFailed) && run["status"] != string(store.RunPartial) {
		t.Fatalf("unanswered prompt ended %v, want failed or partial: %v", run["status"], run)
	}
	if _, err := os.Stat(dstRoot); !os.IsNotExist(err) {
		t.Fatalf("an unanswered prompt created the folder anyway (stat err: %v)", err)
	}
}

// create_dest_dirs=always is the opted-out path: no question, just make it.
func TestDestinationSubpathIsCreatedOnFirstRun(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 12, "nested/b.txt": 8})

	// Deliberately NOT created: the point of the test.
	fresh := scope + "/never-made/deeper"
	dstRoot := filepath.Join(h.mountFor(t, dstID), fresh)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(h.mountFor(t, dstID), scope+"/never-made")) })

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("autocreate"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"create_dest_dirs": string(store.CreateDirsAlways),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": fresh}},
	})

	runID := h.runJob(t, jobID)
	if run := h.awaitRun(t, runID, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("first run into a missing destination folder = %v: %v", run["status"], run)
	}
	assertSMBTreesMatch(t, srcRoot, dstRoot)
}

// The source is never created. A missing source is a misconfiguration, and
// inventing one would turn a typo into a run that copies nothing and calls
// itself a success.
func TestMissingSourceSubpathIsNotCreated(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, _ := previewFixture(t, h)

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("nosrc"),
		"source_target_id": srcID,
		"source_subpath":   scope + "/does-not-exist",
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 60*time.Second)
	if run["status"] != string(store.RunFailed) {
		t.Fatalf("a run with a missing source = %v, want failed: %v", run["status"], run)
	}

	// And it must not have been quietly brought into existence.
	if _, err := os.Stat(filepath.Join(h.mountFor(t, srcID), scope, "does-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("the missing source subpath was created (stat err: %v)", err)
	}
}

// A preview creates nothing — not even a directory. Planning must leave the
// destination exactly as it found it, or "nothing has been written yet" on the
// confirm screen is a lie. This is the guarantee that broke when destination
// folders started being created at resolve time.
func TestPreviewDoesNotCreateTheDestinationFolder(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 7})

	fresh := scope + "/preview-must-not-make-this"
	dstRoot := filepath.Join(h.mountFor(t, dstID), fresh)
	t.Cleanup(func() { _ = os.RemoveAll(dstRoot) })

	// create_dest_dirs=always is the path that *does* create on a real run, so
	// this pins that preview is the exception to it.
	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("preview-nocreate"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		// Even told to create unconditionally, a preview must not.
		"create_dest_dirs":   string(store.CreateDirsAlways),
		"prompt_timeout_sec": 5,
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": fresh}},
	})

	_, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", map[string]any{"preview": true})
	runID, _ := body["id"].(string)
	h.awaitRun(t, runID, 60*time.Second)

	if _, err := os.Stat(dstRoot); !os.IsNotExist(err) {
		t.Fatalf("a preview created the destination folder (stat err: %v)", err)
	}

	// And the real run still creates it, so the guarantee costs nothing.
	realRun := h.runJob(t, jobID)
	if run := h.awaitRun(t, realRun, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("the real run = %v: %v", run["status"], run)
	}
	if _, err := os.Stat(dstRoot); err != nil {
		t.Fatalf("the real run did not create the folder: %v", err)
	}
}

// create_dest_dirs=never treats a missing folder as an unavailable
// destination: nothing is created and nothing is asked.
func TestCreateDestDirsNeverSkipsTheDestination(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 5})

	fresh := scope + "/never-create-this"
	dstRoot := filepath.Join(h.mountFor(t, dstID), fresh)

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("never-create"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"create_dest_dirs": string(store.CreateDirsNever),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": fresh}},
	})

	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 60*time.Second)
	if run["status"] == string(store.RunSuccess) {
		t.Fatalf("a run with create_dest_dirs=never succeeded against a missing folder: %v", run)
	}
	if _, err := os.Stat(dstRoot); !os.IsNotExist(err) {
		t.Fatalf("create_dest_dirs=never created the folder anyway (stat err: %v)", err)
	}
}

// A new job defaults to asking, so the safe behaviour is the one you get
// without choosing anything.
func TestNewJobsDefaultToAskingBeforeCreating(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, _ := previewFixture(t, h)

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("default-ask"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	_, body := h.do(http.MethodGet, "/api/jobs/"+jobID, nil)
	if got := body["create_dest_dirs"]; got != string(store.CreateDirsAsk) {
		t.Fatalf("create_dest_dirs defaulted to %v, want %q", got, store.CreateDirsAsk)
	}
}

// setGlobalFilters replaces the global exclusion set.
func (h *harness) setGlobalFilters(t *testing.T, rules []map[string]any) {
	t.Helper()
	status, body := h.do(http.MethodPut, "/api/settings/filters", map[string]any{"filters": rules})
	if status != http.StatusOK {
		t.Fatalf("PUT global filters = %d, want 200: %v", status, body)
	}
}

// A global exclusion must reach a job that has **no filter rules of its own**.
// resolveChains used to return early on len(job.Filters) == 0, which would skip
// globals entirely — and, worse, skip the degradation a broken global rule has
// to cause. Most jobs have no rules, so this is the common case, not an edge.
func TestGlobalExclusionAppliesToAJobWithNoRulesOfItsOwn(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"keep.txt": 10, "Thumbs.db": 6, "sub/keep2.txt": 8})

	h.setGlobalFilters(t, []map[string]any{{
		"source": "inline", "patterns": []string{"Thumbs.db"},
		"case_sensitive": false, "on_error": "fail_run",
	}})

	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("global-norules"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	if run := h.awaitRun(t, runID, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("run = %v: %v", run["status"], run)
	}

	if _, err := os.Stat(filepath.Join(dstRoot, "Thumbs.db")); !os.IsNotExist(err) {
		t.Fatalf("a globally excluded file was copied (stat err: %v)", err)
	}
	for _, want := range []string{"keep.txt", "sub/keep2.txt"} {
		if _, err := os.Stat(filepath.Join(dstRoot, want)); err != nil {
			t.Fatalf("%s was not copied: %v", want, err)
		}
	}
}

// The deletion-safety invariant, at global scope: an excluded path must be
// invisible on BOTH sides. If a global rule prunes the source walk but is
// missing from a destination's diff chain, that subtree looks "missing at the
// source" and mirror deletes it — destroying exactly what the exclusion was
// written to protect.
func TestGloballyExcludedDestinationFilesAreNotDeleted(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 10})

	// Present at the destination, absent from the source, and globally
	// excluded. A mirror would remove it if the exclusion did not reach the
	// diff chain.
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	seedTree(t, dstRoot, map[string]int{"a.txt": 10, "Thumbs.db": 4, "_ARCHIVE/old.bin": 32})

	h.setGlobalFilters(t, []map[string]any{{
		"source": "inline", "patterns": []string{"Thumbs.db", "_ARCHIVE/"},
		"case_sensitive": false, "on_error": "fail_run",
	}})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("global-nodelete"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	if run := h.awaitRun(t, runID, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("run = %v: %v", run["status"], run)
	}

	for _, kept := range []string{"Thumbs.db", "_ARCHIVE/old.bin"} {
		if _, err := os.Stat(filepath.Join(dstRoot, kept)); err != nil {
			t.Fatalf("mirror deleted the globally excluded %q: %v", kept, err)
		}
	}
}

// A global rule that cannot be loaded must disable deletions for every job —
// including one with no rules of its own. A dropped rule widens what the job
// sees, and in mirror mode a widened view turns protected files into
// extraneous ones.
func TestBrokenGlobalRuleDisablesDeletionsEverywhere(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 10})

	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	seedTree(t, dstRoot, map[string]int{"a.txt": 10, "extraneous.bin": 20})

	// A list file that does not exist, tolerated rather than fatal.
	h.setGlobalFilters(t, []map[string]any{{
		"source": "listfile", "file_path": "/tmp/definitely-not-here.txt",
		"case_sensitive": false, "on_error": "ignore_rule",
	}})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("global-degraded"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 60*time.Second)

	// The extraneous file must survive: deletions are off because the filter
	// is narrower than configured.
	if _, err := os.Stat(filepath.Join(dstRoot, "extraneous.bin")); err != nil {
		t.Fatalf("a degraded global filter still allowed a deletion: %v", err)
	}
	if got := run["status"]; got != string(store.RunPartial) {
		t.Fatalf("run = %v, want partial when a rule was dropped: %v", got, run)
	}
	if summary, _ := run["error_summary"].(string); summary == "" {
		t.Fatal("the run does not say why it was partial")
	}
}

// With no global rules, chains must be exactly what they were before globals
// existed — otherwise every job in the suite starts exercising a different
// code path than it did.
func TestNoGlobalRulesLeavesBehaviourUnchanged(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 10, "Thumbs.db": 4})

	h.setGlobalFilters(t, []map[string]any{}) // explicitly none

	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("no-globals"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	if run := h.awaitRun(t, runID, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("run = %v", run["status"])
	}
	// Thumbs.db is copied, because nothing excludes it any more.
	if _, err := os.Stat(filepath.Join(dstRoot, "Thumbs.db")); err != nil {
		t.Fatalf("with no global rules the file should have been copied: %v", err)
	}
	assertSMBTreesMatch(t, srcRoot, dstRoot)
}

// A fresh install ships with the seeded defaults, so the noise is excluded
// before anyone configures anything.
func TestGlobalFiltersAreSeededOnAFreshInstall(t *testing.T) {
	h := newHarness(t, nil)

	status, body := h.do(http.MethodGet, "/api/settings/filters", nil)
	if status != http.StatusOK {
		t.Fatalf("GET global filters = %d: %v", status, body)
	}
	rules, _ := body["filters"].([]any)
	if len(rules) == 0 {
		t.Fatal("a fresh install has no seeded global filters")
	}

	var patterns []string
	for _, raw := range rules {
		m, _ := raw.(map[string]any)
		for _, p := range m["patterns"].([]any) {
			s, _ := p.(string)
			patterns = append(patterns, s)
		}
	}
	for _, want := range []string{".DS_Store", "Thumbs.db", "_ARCHIVE/"} {
		if !slices.Contains(patterns, want) {
			t.Fatalf("the seeded defaults do not exclude %q: %v", want, patterns)
		}
	}
}

// The global log filters by destination (SPEC.md §9's Logs page).
//
// Two destinations, deliberately: with one, a filter that silently matched
// everything would look exactly like a working one. The interesting question
// this answers is "what has been going wrong against *this* NAS", and the
// evidence for it is scattered one row at a time across every job that writes
// there — which is why no per-run view can substitute.
func TestLogsFilterByDestination(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 100, "b.txt": 100})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("logs-dest"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"log_every_file":   true,
		"destinations": []map[string]any{
			{"dest_target_id": dstAID, "dest_subpath": scope},
			{"dest_target_id": dstBID, "dest_subpath": scope},
		},
	})
	h.awaitRun(t, h.runJob(t, jobID), 120*time.Second)

	// dests reports how many events came back and which destinations they
	// were attributed to.
	dests := func(query string) (int, map[string]int) {
		t.Helper()
		status, body := h.do(http.MethodGet, "/api/logs?limit=1000&"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /api/logs?%s = %d, want 200: %v", query, status, body)
		}
		events, _ := body["events"].([]any)
		byDest := map[string]int{}
		for _, raw := range events {
			e, _ := raw.(map[string]any)
			id, _ := e["dest_target_id"].(string)
			byDest[id]++
		}
		return len(events), byDest
	}

	total, all := dests("job_id=" + jobID)
	if all[dstAID] == 0 || all[dstBID] == 0 {
		t.Fatalf("the run logged nothing against one of its destinations: %v", all)
	}
	// ListEvents caps a page at 1000. If the fixture ever grows past that,
	// both counts saturate there and the narrowing check below compares 1000
	// with 1000 — passing or failing for reasons that have nothing to do with
	// the filter. Fail loudly instead of silently testing nothing.
	if total >= 1000 {
		t.Fatalf("the fixture logs %d events and has outgrown the page cap; the checks below are meaningless", total)
	}

	got, byDest := dests("job_id=" + jobID + "&dest=" + dstAID)
	if got == 0 {
		t.Fatal("filtering by a destination that was written to returned nothing")
	}
	if len(byDest) != 1 || byDest[dstAID] != got {
		t.Fatalf("dest=%s returned events for other destinations: %v", dstAID, byDest)
	}
	// The filter has to *narrow*: the run also logs events that belong to no
	// destination at all, so a filtered page can never be the whole page.
	if got >= total {
		t.Fatalf("dest=%s returned %d of %d events — the filter did not narrow anything", dstAID, got, total)
	}

	if n, _ := dests("dest=no-such-target"); n != 0 {
		t.Fatalf("filtering by an unknown destination returned %d events", n)
	}
}

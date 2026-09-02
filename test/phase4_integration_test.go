//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// awaitRunStatus polls until the run reports one of the wanted statuses. Used
// for the non-terminal states — a parked preview never reaches a terminal
// status on its own.
func (h *harness) awaitRunStatus(t *testing.T, runID string, timeout time.Duration, want ...string) map[string]any {
	t.Helper()

	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w] = true
	}

	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		status, body := h.do(http.MethodGet, "/api/runs/"+runID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET run = %d: %v", status, body)
		}
		last = body
		if s, _ := body["status"].(string); wanted[s] {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %v within %v; last was %v", runID, want, timeout, last["status"])
	return nil
}

// awaitDestStatus polls until one destination of a run reports a status.
func (h *harness) awaitDestStatus(t *testing.T, runID, destTargetID string, timeout time.Duration, want string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, body := h.do(http.MethodGet, "/api/runs/"+runID, nil)
		for _, d := range decodeDestinations(body) {
			if d["dest_target_id"] == destTargetID {
				if s, _ := d["status"].(string); s == want {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("destination %s of run %s never reached %q within %v", destTargetID, runID, want, timeout)
}

// destStatus reads one destination's current status without waiting for it.
func (h *harness) destStatus(t *testing.T, runID, destTargetID string) string {
	t.Helper()

	_, body := h.do(http.MethodGet, "/api/runs/"+runID, nil)
	for _, d := range decodeDestinations(body) {
		if d["dest_target_id"] == destTargetID {
			s, _ := d["status"].(string)
			return s
		}
	}
	t.Fatalf("run %s has no destination %s", runID, destTargetID)
	return ""
}

func decodeDestinations(body map[string]any) []map[string]any {
	raw, _ := body["destinations"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Phase 4a exit criterion 1: the API is closed to strangers, a password opens
// it, and signing out closes it again.
func TestSessionAuthGuardsTheAPI(t *testing.T) {
	h := newHarness(t, nil)

	// newHarness has already completed setup and signed in, so the
	// authenticated client works.
	if status, body := h.do(http.MethodGet, "/api/targets", nil); status != http.StatusOK {
		t.Fatalf("authenticated GET /api/targets = %d, want 200: %v", status, body)
	}

	protected := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/targets"},
		{http.MethodPost, "/api/targets"},
		{http.MethodGet, "/api/jobs"},
		{http.MethodGet, "/api/runs"},
		{http.MethodGet, "/api/logs"},
		{http.MethodGet, "/api/browse?target_id=whatever"},
	}
	for _, tc := range protected {
		t.Run("anonymous "+tc.method+" "+tc.path, func(t *testing.T) {
			status, body := h.doAnon(tc.method, tc.path, nil)
			if status != http.StatusUnauthorized {
				t.Fatalf("%s %s anonymously = %d, want 401: %v", tc.method, tc.path, status, body)
			}
		})
	}

	// The liveness probe stays open: a container health check has no cookie.
	if status, _ := h.doAnon(http.MethodGet, "/healthz", nil); status != http.StatusOK {
		t.Fatalf("anonymous /healthz = %d, want 200", status)
	}

	// Setup cannot be replayed to take over a configured server.
	if status, body := h.doAnon(http.MethodPost, "/api/auth/setup",
		map[string]any{"password": "an-attackers-password"}); status != http.StatusConflict {
		t.Fatalf("replayed setup = %d, want 409: %v", status, body)
	}

	// A wrong password is refused; the right one issues a session.
	if status, _ := h.doAnon(http.MethodPost, "/api/auth/login",
		map[string]any{"password": "not-the-password"}); status != http.StatusUnauthorized {
		t.Fatalf("login with a wrong password = %d, want 401", status)
	}

	// Signing out invalidates the session the harness is holding.
	if status, body := h.do(http.MethodPost, "/api/auth/logout", nil); status != http.StatusOK {
		t.Fatalf("logout = %d, want 200: %v", status, body)
	}
	if status, body := h.do(http.MethodGet, "/api/targets", nil); status != http.StatusUnauthorized {
		t.Fatalf("GET /api/targets after logout = %d, want 401: %v", status, body)
	}

	// And signing back in restores access.
	if status, body := h.do(http.MethodPost, "/api/auth/login",
		map[string]any{"password": adminPassword}); status != http.StatusOK {
		t.Fatalf("login = %d, want 200: %v", status, body)
	}
	if status, body := h.do(http.MethodGet, "/api/targets", nil); status != http.StatusOK {
		t.Fatalf("GET /api/targets after signing back in = %d, want 200: %v", status, body)
	}
}

// A cookie the server did not issue must not authenticate. Expiry itself is
// unit-tested in internal/store, where the clock comparison lives; what the
// API layer owns is refusing a token that does not resolve to a live session.
func TestForgedSessionCookieIsRejected(t *testing.T) {
	h := newHarness(t, nil)

	forged := []string{
		"not-a-real-token",
		"",
		strings.Repeat("A", 43), // right shape, wrong value
	}
	for _, value := range forged {
		t.Run("cookie "+value, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.server.URL+"/api/targets", nil)
			if err != nil {
				t.Fatalf("building the request: %v", err)
			}
			req.AddCookie(&http.Cookie{Name: "smbsync_session", Value: value})

			resp, err := h.anonClient.Do(req)
			if err != nil {
				t.Fatalf("GET /api/targets: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("a forged cookie was accepted: status %d", resp.StatusCode)
			}
		})
	}
}

// Phase 4a exit criterion 2: a preview plans and holds, changing nothing until
// it is confirmed.
func TestPreviewHoldsUntilConfirmed(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"a.txt": 100, "nested/b.txt": 200})
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("preview"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", map[string]any{"preview": true})
	if status != http.StatusAccepted {
		t.Fatalf("preview run = %d, want 202: %v", status, body)
	}
	runID, _ := body["id"].(string)

	parked := h.awaitRunStatus(t, runID, 30*time.Second, string(store.RunAwaitingConfirmation))

	// The plan must be visible — it is what the user is being asked about.
	progress, _ := parked["progress"].(map[string]any)
	plans, _ := progress["plans"].([]any)
	if len(plans) != 1 {
		t.Fatalf("expected one plan while parked, got %v", progress["plans"])
	}
	plan, _ := plans[0].(map[string]any)
	if copies, _ := plan["copies"].(float64); copies != 2 {
		t.Fatalf("plan reports %v copies, want 2: %v", plan["copies"], plan)
	}

	// Nothing may have been written yet. This is the whole point of a preview.
	if n := countFiles(t, dstRoot); n != 0 {
		t.Fatalf("a parked preview wrote %d files to the destination; it must write none", n)
	}

	if status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm = %d, want 200: %v", status, body)
	}

	run := h.awaitRun(t, runID, 60*time.Second)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("confirmed run status = %v, want success: %v", got, run)
	}
	assertSMBTreesMatch(t, srcRoot, dstRoot)
}

// An unconfirmed preview cancels itself and still changes nothing. The
// countdown exists because a run may be unattended.
func TestUnconfirmedPreviewCancelsAndChangesNothing(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"a.txt": 100})
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("preview-timeout"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"prompt_timeout_sec": 5, // the floor; the real default is 600
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	_, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", map[string]any{"preview": true})
	runID, _ := body["id"].(string)

	h.awaitRunStatus(t, runID, 30*time.Second, string(store.RunAwaitingConfirmation))

	// Wait it out rather than confirming.
	run := h.awaitRun(t, runID, 30*time.Second)
	if got := run["status"]; got != string(store.RunCancelled) {
		t.Fatalf("unconfirmed preview ended %v, want cancelled: %v", got, run)
	}
	if n := countFiles(t, dstRoot); n != 0 {
		t.Fatalf("an unconfirmed preview wrote %d files; it must write none", n)
	}

	if summary, _ := run["error_summary"].(string); !strings.Contains(summary, "not confirmed") {
		t.Fatalf("the run does not say why it stopped: %q", summary)
	}
}

// A job holding a parked preview cannot be edited. The plan is already made:
// editing the job does not change what a later confirm executes, so an admin
// who adds an exclude rule and then confirms would watch the *old* plan delete
// the files the new rule existed to protect. The guard has to be keyed by job
// — Progress is keyed by run ID and never matches a job ID, which is how this
// went unnoticed — and it has to cover awaiting_confirmation, not just running.
func TestJobCannotBeEditedWhileAPreviewIsParked(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"keep.txt": 10})

	body := map[string]any{
		"name":               uniqueName("parked-edit"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"prompt_timeout_sec": 5, // the floor, so the parked run cleans itself up
		"destinations":       []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	}
	jobID := h.createJob(t, body)

	_, run := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", map[string]any{"preview": true})
	runID, _ := run["id"].(string)
	h.awaitRunStatus(t, runID, 30*time.Second, string(store.RunAwaitingConfirmation))

	edit := map[string]any{}
	for k, v := range body {
		edit[k] = v
	}
	edit["filters"] = []map[string]any{{
		"direction": "exclude", "source": "inline", "patterns": []string{"*.tmp"},
	}}
	status, resp := h.do(http.MethodPatch, "/api/jobs/"+jobID, edit)
	if status != http.StatusConflict {
		t.Fatalf("PATCH while a preview is parked = %d, want 409: %v", status, resp)
	}
	errObj, _ := resp["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "job_running" {
		t.Fatalf("error code = %v, want job_running: %v", errObj["code"], resp)
	}

	// Once the park lapses the job is editable again, so the guard releases.
	h.awaitRun(t, runID, 30*time.Second)
	if status, resp := h.do(http.MethodPatch, "/api/jobs/"+jobID, edit); status != http.StatusOK {
		t.Fatalf("PATCH after the park ended = %d, want 200: %v", status, resp)
	}
}

// Confirming a job with no parked run is a clear 409 rather than a silent
// no-op, so a stale browser tab cannot look like it worked.
func TestConfirmWithoutAPreviewIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, _ := previewFixture(t, h)

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("no-preview"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	if status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/confirm", nil); status != http.StatusConflict {
		t.Fatalf("confirm without a preview = %d, want 409: %v", status, body)
	}
}

// Phase 4a exit criterion 3: an unreachable destination under the prompt
// policy parks and waits, while the healthy destination carries on.
func TestPromptPolicyParksAndAnswersSkip(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 100})

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("prompt-skip"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"unavailable_policy": string(store.PolicyPrompt),
		"prompt_timeout_sec": 120,
		"destinations": []map[string]any{
			{"dest_target_id": dstAID, "dest_subpath": scope},
			{"dest_target_id": dstBID, "dest_subpath": scope},
		},
	})

	blackhole(t, sambaB(t))
	runID := h.runJob(t, jobID)

	h.awaitDestStatus(t, runID, dstBID, 90*time.Second, string(store.DestAwaitingPrompt))

	// The countdown the UI shows must be present.
	_, body := h.do(http.MethodGet, "/api/runs/"+runID, nil)
	progress, _ := body["progress"].(map[string]any)
	var sawDeadline bool
	for _, raw := range progress["destinations"].([]any) {
		d, _ := raw.(map[string]any)
		if d["dest_target_id"] == dstBID {
			if s, _ := d["prompt_deadline"].(string); s != "" {
				sawDeadline = true
			}
		}
	}
	if !sawDeadline {
		t.Fatalf("a waiting destination reports no prompt deadline: %v", progress["destinations"])
	}

	// The exit criterion is that B parks *while A completes*, and this is the
	// assertion that makes it non-vacuous: A's files must already be at the
	// destination while B is still waiting for an answer. Checking A only
	// after the run ends passes whether A copied concurrently or ten minutes
	// later, which is exactly how a regression that stalled every destination
	// behind one prompt went unnoticed.
	dstARoot := filepath.Join(h.mountFor(t, dstAID), scope)
	deadline := time.Now().Add(60 * time.Second)
	var aDone bool
	for time.Now().Before(deadline) {
		if countFiles(t, dstARoot) > 0 {
			aDone = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !aDone {
		t.Fatal("the healthy destination copied nothing while the unavailable one was parked: " +
			"a prompt on one destination is stalling the others")
	}
	// And B really is still parked — otherwise the check above proves nothing.
	if st := h.destStatus(t, runID, dstBID); st != string(store.DestAwaitingPrompt) {
		t.Fatalf("the unavailable destination is %q, want still awaiting_prompt; "+
			"the concurrency assertion above is only meaningful while it waits", st)
	}

	status, resp := h.do(http.MethodPost, "/api/runs/"+runID+"/prompt",
		map[string]any{"action": "skip", "dest_target_id": dstBID})
	if status != http.StatusOK {
		t.Fatalf("answering the prompt = %d, want 200: %v", status, resp)
	}

	run := h.awaitRun(t, runID, 90*time.Second)
	if got := run["status"]; got != string(store.RunPartial) {
		t.Fatalf("run status = %v, want partial: %v", got, run)
	}

	// The healthy destination still got its files.
	assertSMBTreesMatch(t, srcRoot, filepath.Join(h.mountFor(t, dstAID), scope))
}

// Answering `abort` ends the run.
func TestPromptPolicyAnswersAbort(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 100})

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("prompt-abort"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"unavailable_policy": string(store.PolicyPrompt),
		"prompt_timeout_sec": 120,
		"destinations": []map[string]any{
			{"dest_target_id": dstBID, "dest_subpath": scope},
			{"dest_target_id": dstAID, "dest_subpath": scope},
		},
	})

	blackhole(t, sambaB(t))
	runID := h.runJob(t, jobID)

	h.awaitDestStatus(t, runID, dstBID, 90*time.Second, string(store.DestAwaitingPrompt))

	if status, resp := h.do(http.MethodPost, "/api/runs/"+runID+"/prompt",
		map[string]any{"action": "abort", "dest_target_id": dstBID}); status != http.StatusOK {
		t.Fatalf("answering abort = %d, want 200: %v", status, resp)
	}

	run := h.awaitRun(t, runID, 90*time.Second)
	if got := run["status"]; got != string(store.RunFailed) {
		t.Fatalf("run status = %v, want failed: %v", got, run)
	}
}

// With nobody watching, the prompt falls back and says so. This is the case
// that matters most: from Phase 5 a run may be started by cron.
func TestUnansweredPromptFallsBackToSkip(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 100})

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("prompt-timeout"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"unavailable_policy": string(store.PolicyPrompt),
		"prompt_timeout_sec": 5,
		"prompt_fallback":    string(store.FallbackSkip),
		"destinations": []map[string]any{
			{"dest_target_id": dstAID, "dest_subpath": scope},
			{"dest_target_id": dstBID, "dest_subpath": scope},
		},
	})

	blackhole(t, sambaB(t))
	runID := h.runJob(t, jobID)

	run := h.awaitRun(t, runID, 120*time.Second)
	if got := run["status"]; got != string(store.RunPartial) {
		t.Fatalf("run status = %v, want partial: %v", got, run)
	}

	// The fallback must be stated, not silent: a destination was skipped
	// because nobody answered, and that has to be discoverable afterwards.
	_, events := h.do(http.MethodGet, "/api/runs/"+runID+"/events?limit=500", nil)
	raw, _ := json.Marshal(events)
	if !strings.Contains(string(raw), "no answer after") {
		t.Fatalf("the log never records the fallback: %s", raw)
	}

	assertSMBTreesMatch(t, srcRoot, filepath.Join(h.mountFor(t, dstAID), scope))
}

// Phase 4a exit criterion 4: a WebSocket client sees a run happen.
func TestWebSocketStreamsRunProgress(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)

	// Enough files that the run spans several broadcast ticks: a run that
	// finishes inside one tick is reported as finished but never as
	// in-progress, which would make the progress assertion a race.
	tree := map[string]int{}
	for i := range 3000 {
		tree[fmt.Sprintf("d%02d/f%04d.bin", i%20, i)] = 512
	}
	seedTree(t, srcRoot, tree)

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("ws"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/ws"
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: h.client})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dialling the event feed: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	runID := h.runJob(t, jobID)

	var sawProgress, sawFinished bool
	for !sawFinished {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("reading from the event feed: %v (progress seen: %v)", err, sawProgress)
		}
		var ev struct {
			Event string `json:"event"`
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("decoding an event frame: %v", err)
		}
		if ev.RunID != runID {
			continue
		}
		switch ev.Event {
		case "run_progress":
			sawProgress = true
		case "run_finished":
			sawFinished = true
		}
	}

	if !sawProgress {
		t.Fatal("the feed reported completion without ever reporting progress")
	}
}

// A client that stops reading must be dropped, never allowed to hold up a run.
// The hub is the one place where a browser could reach into the sync path.
func TestStalledWebSocketClientDoesNotDelayARun(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 100, "nested/b.txt": 200})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("ws-stall"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/ws"
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: h.client})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dialling the event feed: %v", err)
	}
	// Deliberately never read from conn: this client is the stalled one.
	defer func() { _ = conn.CloseNow() }()

	started := time.Now()
	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 60*time.Second)

	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("run status = %v, want success even with a stalled client: %v", got, run)
	}
	// The run should take about as long as it always does. A generous bound:
	// the point is that it finished at all, not a precise timing.
	if elapsed := time.Since(started); elapsed > 45*time.Second {
		t.Fatalf("the run took %v with a stalled websocket client attached", elapsed)
	}
	assertSMBTreesMatch(t, srcRoot, filepath.Join(h.mountFor(t, dstID), scope))
}

// Phase 4a exit criterion 5, part one: the path picker lists a share and
// cannot be talked out of its root.
func TestBrowseListsAShareAndRefusesEscapes(t *testing.T) {
	h := newHarness(t, nil)
	targetID := h.createTarget(smbTarget(uniqueName("browse"), sambaA(t), shareCredentialed, userName, userPassword))

	status, body := h.do(http.MethodGet, "/api/browse?target_id="+targetID, nil)
	if status != http.StatusOK {
		t.Fatalf("browse = %d, want 200: %v", status, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) == 0 {
		t.Fatalf("browsing the share root returned nothing: %v", body)
	}

	var sawDir, sawFile bool
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		name, _ := e["name"].(string)
		isDir, _ := e["is_dir"].(bool)
		if name == fixtureDir && isDir {
			sawDir = true
		}
		if name == fixtureFile && !isDir {
			sawFile = true
		}
	}
	if !sawDir || !sawFile {
		t.Fatalf("browse did not list the fixtures (dir %v, file %v): %v", sawDir, sawFile, entries)
	}

	// Descending works.
	status, body = h.do(http.MethodGet, "/api/browse?target_id="+targetID+"&path="+fixtureDir, nil)
	if status != http.StatusOK {
		t.Fatalf("browse into %s = %d: %v", fixtureDir, status, body)
	}

	escapes := []string{"..", "../..", "nested/../..", "/etc"}
	for _, bad := range escapes {
		t.Run("refuses "+bad, func(t *testing.T) {
			status, body := h.do(http.MethodGet, "/api/browse?target_id="+targetID+"&path="+bad, nil)
			if status != http.StatusBadRequest {
				t.Fatalf("browse %q = %d, want 400: %v", bad, status, body)
			}
		})
	}
}

// Phase 4a exit criterion 5, part two: the global log reads across runs.
func TestLogsReadAcrossRuns(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 100})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("logs"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	// Two runs, so the global view has more than one to read across.
	for range 2 {
		h.awaitRun(t, h.runJob(t, jobID), 60*time.Second)
	}

	status, body := h.do(http.MethodGet, "/api/logs?limit=500", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/logs = %d, want 200: %v", status, body)
	}
	events, _ := body["events"].([]any)
	if len(events) < 2 {
		t.Fatalf("the global log returned %d events, want several: %v", len(events), body)
	}

	runIDs := map[string]bool{}
	for _, raw := range events {
		e, _ := raw.(map[string]any)
		if id, _ := e["run_id"].(string); id != "" {
			runIDs[id] = true
		}
	}
	if len(runIDs) < 2 {
		t.Fatalf("the global log only covers %d run(s); it should span both", len(runIDs))
	}

	// Filtering by job keeps everything; filtering by a job that does not
	// exist keeps nothing. Together those prove the filter is applied.
	_, body = h.do(http.MethodGet, "/api/logs?job_id="+jobID+"&limit=500", nil)
	if events, _ := body["events"].([]any); len(events) == 0 {
		t.Fatalf("filtering by the job's own id returned nothing")
	}
	_, body = h.do(http.MethodGet, "/api/logs?job_id=no-such-job", nil)
	if events, _ := body["events"].([]any); len(events) != 0 {
		t.Fatalf("filtering by an unknown job returned %d events", len(events))
	}

	// Level filtering.
	_, body = h.do(http.MethodGet, "/api/logs?level=info&limit=500", nil)
	events, _ = body["events"].([]any)
	if len(events) == 0 {
		t.Fatalf("filtering by level=info returned nothing")
	}
	for _, raw := range events {
		e, _ := raw.(map[string]any)
		if lvl, _ := e["level"].(string); lvl != "info" {
			t.Fatalf("level=info returned a %q event: %v", lvl, e)
		}
	}

	if status, _ := h.do(http.MethodGet, "/api/logs?level=shouty", nil); status != http.StatusBadRequest {
		t.Fatalf("an unknown level = %d, want 400", status)
	}
}

// A job cannot be edited out from under a run: changing destinations or
// filters mid-diff is not something the engine is built to survive.
func TestJobUpdateReplacesDestinationsAndFilters(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"keep.txt": 10, "drop.tmp": 10})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("editable"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	update := map[string]any{
		"name":             uniqueName("edited"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeUpdate),
		"workers":          2,
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": "exclude", "source": "inline", "patterns": []string{"*.tmp"},
		}},
	}
	status, body := h.do(http.MethodPatch, "/api/jobs/"+jobID, update)
	if status != http.StatusOK {
		t.Fatalf("PATCH job = %d, want 200: %v", status, body)
	}
	if got := body["mode"]; got != string(store.ModeUpdate) {
		t.Fatalf("mode after update = %v, want update", got)
	}
	filters, _ := body["filters"].([]any)
	if len(filters) != 1 {
		t.Fatalf("expected one filter after the update, got %v", body["filters"])
	}

	// Updating again replaces rather than accumulates: this is the bug a
	// wholesale replace exists to avoid.
	if status, body := h.do(http.MethodPatch, "/api/jobs/"+jobID, update); status != http.StatusOK {
		t.Fatalf("second PATCH = %d: %v", status, body)
	}
	_, body = h.do(http.MethodGet, "/api/jobs/"+jobID, nil)
	if filters, _ := body["filters"].([]any); len(filters) != 1 {
		t.Fatalf("filters accumulated across updates: %v", body["filters"])
	}
	if dests, _ := body["destinations"].([]any); len(dests) != 1 {
		t.Fatalf("destinations accumulated across updates: %v", body["destinations"])
	}

	// The new filter takes effect on the next run.
	runID := h.runJob(t, jobID)
	if run := h.awaitRun(t, runID, 60*time.Second); run["status"] != string(store.RunSuccess) {
		t.Fatalf("run after the update = %v", run["status"])
	}
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	if _, err := os.Stat(filepath.Join(dstRoot, "drop.tmp")); !os.IsNotExist(err) {
		t.Fatalf("the excluded file was copied anyway (stat err: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "keep.txt")); err != nil {
		t.Fatalf("the included file was not copied: %v", err)
	}
}

// previewFixture creates a source and one destination, each scoped to a
// unique subdirectory.
func previewFixture(t *testing.T, h *harness) (srcID, dstID, scope, srcRoot string) {
	t.Helper()

	scope = fmt.Sprintf("p4-%d", time.Now().UnixNano())

	srcID = h.createTarget(smbTarget(uniqueName("src"), sambaA(t), shareCredentialed, userName, userPassword))
	dstID = h.createTarget(smbTarget(uniqueName("dst"), sambaA(t), shareGuest, "", ""))

	srcRoot = filepath.Join(h.mountFor(t, srcID), scope)
	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	for _, root := range []string{srcRoot, dstRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("creating %s: %v", root, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	return srcID, dstID, scope, srcRoot
}

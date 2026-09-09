//go:build integration

package test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// hookedJob builds a real SMB mirror job and issues it a trigger token.
func hookedJob(t *testing.T, h *harness) (jobID, token, srcRoot string) {
	t.Helper()

	srcID, dstID, scope, srcRoot := previewFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"triggered.txt": 64})

	jobID = h.createJob(t, map[string]any{
		"name":             uniqueName("hooked"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/token", nil)
	if status != http.StatusCreated {
		t.Fatalf("issuing a token = %d, want 201: %v", status, body)
	}
	token, _ = body["token"].(string)
	if token == "" {
		t.Fatalf("issuing a token returned no token: %v", body)
	}
	return jobID, token, srcRoot
}

// Phase 5b exit criterion: /api/hooks/* is reachable without a session while
// the rest of /api/* is not.
//
// Both directions, because the two failure modes are opposite and both are
// serious: hooks behind the guard breaks every integration silently, and
// anything else outside it makes the whole API public. The mount works by
// ServeMux pattern precedence — "/api/hooks/" beating "/api/" — which is
// correct and completely invisible when reading the routing table.
func TestHooksAreReachableWithoutASessionButNothingElseIs(t *testing.T) {
	h := newHarness(t, nil)
	jobID, token, _ := hookedJob(t, h)

	// Anonymous, with a valid token: allowed.
	status, _, body := h.rawGet("/api/hooks/jobs/"+jobID+"/status?token="+token, true)
	if status != http.StatusOK {
		t.Fatalf("anonymous hook call with a valid token = %d, want 200: %s", status, truncate(body))
	}

	// Anonymous, no token: refused, and refused as 401 rather than 404, so the
	// route is demonstrably present and it is authentication doing the work.
	status, _, _ = h.rawGet("/api/hooks/jobs/"+jobID+"/status", true)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous hook call with no token = %d, want 401", status)
	}

	// Everything else is still shut.
	for _, path := range []string{"/api/jobs", "/api/targets", "/api/runs", "/api/logs"} {
		status, _, _ := h.rawGet(path, true)
		if status != http.StatusUnauthorized {
			t.Fatalf("anonymous GET %s = %d, want 401 — the hooks mount widened the guard", path, status)
		}
	}

	// And issuing a token is *not* reachable with a token: a leaked credential
	// must not be able to mint its own replacements.
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/jobs/"+jobID+"/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.anonClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("minting a token with a trigger token = %d, want 401", resp.StatusCode)
	}
}

// Phase 5b exit criterion: a valid token starts a run; an invalid, revoked or
// absent one is refused.
func TestTriggerTokenStartsARunAndBadTokensDoNot(t *testing.T) {
	h := newHarness(t, nil)
	jobID, token, _ := hookedJob(t, h)

	// The header form is the documented one, so it is the one exercised here.
	runID := h.hookRun(t, jobID, token, "")
	run := h.awaitRun(t, runID, 90*time.Second)
	if got, _ := run["status"].(string); got != string(store.RunSuccess) {
		t.Fatalf("a webhook-triggered run finished as %v: %v", got, run["error_summary"])
	}
	if got, _ := run["trigger"].(string); got != string(store.TriggerWebhook) {
		t.Fatalf("trigger = %q, want %q", got, store.TriggerWebhook)
	}

	// Wrong, empty and structurally-plausible-but-unknown tokens all fail the
	// same way, and none of them start anything.
	for _, bad := range []string{"", "not-a-token", strings.Repeat("A", 43)} {
		status := h.hookRunStatus(t, jobID, bad)
		if status != http.StatusUnauthorized {
			t.Fatalf("token %q = %d, want 401", bad, status)
		}
	}

	// Revoked means revoked.
	if status, body := h.do(http.MethodDelete, "/api/jobs/"+jobID+"/token", nil); status != http.StatusNoContent {
		t.Fatalf("revoking = %d, want 204: %v", status, body)
	}
	if status := h.hookRunStatus(t, jobID, token); status != http.StatusUnauthorized {
		t.Fatalf("a revoked token = %d, want 401", status)
	}

	// Regenerating invalidates the previous token in the same act.
	status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/token", nil)
	if status != http.StatusCreated {
		t.Fatalf("re-issuing = %d: %v", status, body)
	}
	fresh, _ := body["token"].(string)
	if fresh == token {
		t.Fatal("re-issuing returned the same token")
	}
	if status := h.hookRunStatus(t, jobID, token); status != http.StatusUnauthorized {
		t.Fatalf("the old token still works after regenerating = %d, want 401", status)
	}
}

// A token authorises exactly one job. A valid token for job A must not read
// job B's runs, or one integration's credential becomes a key to everything.
func TestATokenIsScopedToItsOwnJob(t *testing.T) {
	h := newHarness(t, nil)
	jobA, tokenA, _ := hookedJob(t, h)
	jobB, _, _ := hookedJob(t, h)

	if status := h.hookRunStatus(t, jobB, tokenA); status != http.StatusUnauthorized {
		t.Fatalf("job A's token read job B's status = %d, want 401", status)
	}

	// And it cannot read another job's run by id either.
	runID := h.hookRun(t, jobA, tokenA, "")
	h.awaitRun(t, runID, 90*time.Second)

	otherRun := h.runJob(t, jobB)
	h.awaitRun(t, otherRun, 90*time.Second)

	status, _, _ := h.rawGet("/api/hooks/runs/"+otherRun+"/status?token="+tokenA, true)
	if status != http.StatusNotFound {
		t.Fatalf("job A's token read job B's run = %d, want 404", status)
	}
}

// hookRun triggers a run through the webhook endpoint and returns its id.
func (h *harness) hookRun(t *testing.T, jobID, token, query string) string {
	t.Helper()

	url := h.server.URL + "/api/hooks/jobs/" + jobID + "/run" + query
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := h.anonClient.Do(req)
	if err != nil {
		t.Fatalf("triggering: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("trigger = %d, want 202", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	runID, _ := body["run_id"].(string)
	if runID == "" {
		t.Fatalf("trigger returned no run_id: %v", body)
	}
	return runID
}

// hookRunStatus attempts a status call and returns only the status code.
func (h *harness) hookRunStatus(t *testing.T, jobID, token string) int {
	t.Helper()
	status, _, _ := h.rawGet("/api/hooks/jobs/"+jobID+"/status?token="+token, true)
	return status
}

// decodeBody reads a JSON response body into a map.
func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the response body: %v", err)
	}
	return out
}

// Phase 5b exit criterion: a second trigger while running is 409 unless
// ?queue=1, and a queued run actually follows.
func TestSecondTriggerIs409UnlessQueued(t *testing.T) {
	h := newHarness(t, nil)
	jobID, token, srcRoot := hookedJob(t, h)
	seedManyFiles(t, srcRoot, 3000)

	first := h.hookRun(t, jobID, token, "")
	h.awaitCopying(t, first, 60*time.Second)

	// Without ?queue=1: refused outright.
	if status := h.hookRunPost(t, jobID, token, ""); status != http.StatusConflict {
		t.Fatalf("a second trigger during a run = %d, want 409", status)
	}

	// With it: accepted, and only one is held however many times we ask.
	if status := h.hookRunPost(t, jobID, token, "?queue=1"); status != http.StatusAccepted {
		t.Fatalf("queueing = %d, want 202", status)
	}
	if status := h.hookRunPost(t, jobID, token, "?queue=1"); status != http.StatusConflict {
		t.Fatalf("queueing twice = %d, want 409 — the queue holds one run, not a pile", status)
	}
	if !h.runner.Queued(jobID) {
		t.Fatal("nothing was queued")
	}

	h.awaitRun(t, first, 4*time.Minute)

	// The promise was "one run will follow", so one must actually appear.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		runs := h.runsForJob(t, jobID)
		if len(runs) >= 2 {
			var newest string
			for _, r := range runs {
				if id, _ := r["id"].(string); id != first {
					newest = id
					break
				}
			}
			if newest == "" {
				t.Fatal("a second run row exists but is the first run")
			}
			h.awaitRun(t, newest, 4*time.Minute)
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("the queued run never started; ?queue=1 promised one would follow")
}

// Phase 5b exit criterion, and the one that matters most: a callback endpoint
// that is down or slow is retried, logged at warn, and **does not delay or
// fail the sync**.
//
// The receiver here accepts the connection and then says nothing at all, which
// is the case that hangs a naive client forever — worse than a refused
// connection, because nothing ever errors. The run must finish on time and
// succeed regardless.
func TestAHangingCallbackDoesNotDelayOrFailTheSync(t *testing.T) {
	h := newHarness(t, nil)

	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })

	var hits int64
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		<-release
	}))
	defer silent.Close()

	jobID, token, _ := hookedJob(t, h)

	status, body := h.do(http.MethodPost, "/api/webhooks", map[string]any{
		"job_id": jobID, "url": silent.URL, "secret": "s3cret", "min_interval_sec": 1,
	})
	if status != http.StatusCreated {
		t.Fatalf("creating a callback = %d: %v", status, body)
	}

	started := time.Now()
	runID := h.hookRun(t, jobID, token, "")
	run := h.awaitRun(t, runID, 2*time.Minute)
	elapsed := time.Since(started)

	if got, _ := run["status"].(string); got != string(store.RunSuccess) {
		t.Fatalf("the run finished as %v with a dead callback attached; delivery must not fail a sync: %v",
			got, run["error_summary"])
	}
	// A single tiny file. If the run waited on the callback at all, this would
	// be tens of seconds rather than a few.
	if elapsed > 45*time.Second {
		t.Fatalf("the run took %v with a hanging callback attached; it did not wait on delivery, it was delayed by it", elapsed)
	}
	if atomic.LoadInt64(&hits) == 0 {
		t.Fatal("the callback was never attempted, so this proves nothing about hanging ones")
	}

	once.Do(func() { close(release) })
}

// hookRunPost triggers and returns only the status code.
func (h *harness) hookRunPost(t *testing.T, jobID, token, query string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/hooks/jobs/"+jobID+"/run"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.anonClient.Do(req)
	if err != nil {
		t.Fatalf("triggering: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// Phase 5b exit criterion: an outbound callback carries a verifiable
// HMAC-SHA256 signature.
//
// End to end, from a real run through the real delivery path, and verified the
// way a receiver would: recompute the MAC over the exact bytes received using
// the shared secret. Deliberately not by calling notify.Sign, which would pass
// even if both sides were wrong in the same way.
func TestOutboundCallbacksAreSignedAndCoverTheRunLifecycle(t *testing.T) {
	h := newHarness(t, nil)

	const secret = "the-receivers-shared-key"
	type delivery struct {
		event string
		valid bool
	}
	got := make(chan delivery, 32)

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))

		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		event, _ := payload["event"].(string)

		got <- delivery{event: event, valid: hmac.Equal([]byte(r.Header.Get("X-Signature")), []byte(want))}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	jobID, token, _ := hookedJob(t, h)
	status, body := h.do(http.MethodPost, "/api/webhooks", map[string]any{
		"job_id": jobID, "url": receiver.URL, "secret": secret,
		"events": []string{"run_started", "run_completed"},
	})
	if status != http.StatusCreated {
		t.Fatalf("creating a callback = %d: %v", status, body)
	}
	// The secret must never come back out.
	if _, leaked := body["secret"]; leaked {
		t.Fatalf("the create response carried the secret: %v", body)
	}
	if has, _ := body["has_secret"].(bool); !has {
		t.Fatalf("has_secret is false for a callback created with one: %v", body)
	}

	runID := h.hookRun(t, jobID, token, "")
	h.awaitRun(t, runID, 90*time.Second)

	seen := map[string]bool{}
	deadline := time.After(60 * time.Second)
	for len(seen) < 2 {
		select {
		case d := <-got:
			if !d.valid {
				t.Fatalf("the %s callback carried a signature that does not verify", d.event)
			}
			seen[d.event] = true
		case <-deadline:
			t.Fatalf("only saw %v; want both run_started and run_completed", seen)
		}
	}

	// And a hook that did not subscribe to an event must not receive it.
	if seen["progress"] {
		t.Fatal("received a progress callback for a hook that subscribed only to start and completion")
	}
}

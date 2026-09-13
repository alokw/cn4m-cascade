package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

func plainLookup(encrypted string) (string, error) { return encrypted, nil }

func testNotifier(t *testing.T) *Notifier {
	t.Helper()
	n := New(nil, plainLookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	t.Cleanup(func() {
		stopCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = n.Stop(stopCtx)
		cancel()
	})
	return n
}

// The signature must verify the way a *receiver* would compute it — from the
// exact bytes on the wire and the shared secret — not by calling the same
// helper that produced it, which would pass even if both sides were wrong.
func TestSignatureVerifiesIndependently(t *testing.T) {
	const secret = "shared-secret"
	body := []byte(`{"event":"run_completed","run_id":"abc"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if got := Sign(secret, body); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	// A different secret must not verify, or the signature proves nothing.
	if Sign("other-secret", body) == want {
		t.Fatal("two different secrets produced the same signature")
	}
	// Neither may a changed body.
	if Sign(secret, []byte(`{"event":"run_failed","run_id":"abc"}`)) == want {
		t.Fatal("a modified body produced the same signature")
	}
}

// A delivery carries the signature of the exact body sent, and a receiver can
// check it without knowing anything about this package.
func TestDeliveredBodyCarriesAVerifiableSignature(t *testing.T) {
	const secret = "receiver-shared-key"

	type received struct {
		body []byte
		sig  string
	}
	got := make(chan received, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- received{body: body, sig: r.Header.Get("X-Signature")}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(t)
	n.enqueue(job{
		hook:  store.Webhook{ID: "w1", URL: srv.URL, SecretEncrypted: secret, Enabled: true},
		event: EventRunCompleted, runID: "run-1",
		payload: Payload{"event": EventRunCompleted, "run_id": "run-1", "status": "success"},
	})

	select {
	case r := <-got:
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(r.body)
		if want := hex.EncodeToString(mac.Sum(nil)); r.sig != want {
			t.Fatalf("X-Signature = %q, want %q", r.sig, want)
		}
		var decoded map[string]any
		if err := json.Unmarshal(r.body, &decoded); err != nil {
			t.Fatalf("the body is not valid JSON: %v", err)
		}
		if decoded["event"] != EventRunCompleted {
			t.Fatalf("event = %v, want %s", decoded["event"], EventRunCompleted)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no callback arrived")
	}
}

// A receiver that accepts the connection and then never answers must not hold
// anything up. This is the property the whole package exists for: the caller
// returns immediately, and the hung delivery is bounded and abandoned.
func TestAHangingReceiverNeverBlocksTheCaller(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never answers until the test lets go
	}))
	defer srv.Close()

	n := testNotifier(t)

	// Park the hook's lane on a hanging delivery and fill its queue behind it,
	// then time the next enqueue. Every lane holds queueDepth, so this has to
	// push past that to reach the "full" path.
	for i := range queueDepth + 8 {
		n.enqueue(job{
			hook:  store.Webhook{ID: "hang", URL: srv.URL, Enabled: true},
			event: EventProgress, runID: "run-hang", payload: Payload{"i": i},
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			// Full is the designed outcome: enqueue reports it and returns,
			// never blocks.
			_ = n.enqueue(job{
				hook:  store.Webhook{ID: "hang", URL: srv.URL, Enabled: true},
				event: EventProgress, runID: "run-hang", payload: Payload{"j": i},
			})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queueing blocked behind a hanging receiver; a dead callback URL would stall the sync")
	}

	once.Do(func() { close(release) })
}

// Progress is throttled per webhook per run, or a multi-hour run would post
// once a second for its whole duration.
func TestProgressIsThrottledPerRun(t *testing.T) {
	n := New(nil, plainLookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook := store.Webhook{ID: "w1", Enabled: true, MinIntervalSec: 30}

	if !n.progressDue(hook, "run-a") {
		t.Fatal("the first progress event was throttled")
	}
	if n.progressDue(hook, "run-a") {
		t.Fatal("a second progress event fired inside the interval")
	}
	// A different run has its own budget.
	if !n.progressDue(hook, "run-b") {
		t.Fatal("a different run was throttled by the first run's timer")
	}

	// And the throttle state does not outlive the run.
	n.Forget("run-a")
	if !n.progressDue(hook, "run-a") {
		t.Fatal("Forget did not clear the run's throttle state")
	}
}

// An empty subscription list means every event, so a hook created without
// choosing is useful rather than silently inert.
func TestSubscriptionMatching(t *testing.T) {
	cases := []struct {
		name  string
		hook  store.Webhook
		event string
		want  bool
	}{
		{"no list means all", store.Webhook{Enabled: true}, EventRunFailed, true},
		{"listed", store.Webhook{Enabled: true, Events: []string{EventRunFailed}}, EventRunFailed, true},
		{"not listed", store.Webhook{Enabled: true, Events: []string{EventRunStarted}}, EventRunFailed, false},
		{"disabled hooks get nothing", store.Webhook{Enabled: false}, EventRunFailed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.hook.Subscribes(tc.event); got != tc.want {
				t.Fatalf("Subscribes(%q) = %v, want %v", tc.event, got, tc.want)
			}
		})
	}
}

// A webhook's events are delivered in the order they were queued.
//
// This became a requirement with cn4m's `progress` level: a progress post is a
// per-app slot that the app's next non-progress post clears, so if a progress
// request lands *after* the run's outcome it re-fills the slot and "97%" sits
// on the rail for two minutes after the sync finished. A shared worker pool
// reorders freely under any receiver latency; per-hook lanes must not.
//
// The receiver's latency is jittered so that a pool would be caught reliably
// rather than occasionally.
func TestDeliveriesForOneHookArriveInOrder(t *testing.T) {
	const events = 40
	arrived := make(chan int, events)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		seq, _ := body["seq"].(float64)
		// Odd-numbered posts dawdle; under a pool the even one queued after
		// them overtakes.
		if int(seq)%2 == 1 {
			time.Sleep(15 * time.Millisecond)
		}
		arrived <- int(seq)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(t)
	hook := store.Webhook{ID: "ordered", URL: srv.URL, Enabled: true, Format: store.FormatJSON}
	for i := range events {
		if !n.enqueue(job{hook: hook, event: EventProgress, runID: "run-1", payload: Payload{"seq": i}}) {
			t.Fatalf("enqueue %d reported full", i)
		}
	}

	for want := range events {
		select {
		case got := <-arrived:
			if got != want {
				t.Fatalf("delivery %d arrived where %d was expected; a later event overtook an earlier one", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d deliveries arrived", want, events)
		}
	}
}

// Lanes are per hook, so one receiver that hangs must not hold up another
// hook's deliveries — the isolation a shared pool gave for free.
func TestAHangingHookDoesNotDelayAnotherHook(t *testing.T) {
	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer hanging.Close()
	// Deferred after Close so it runs first: Close waits for the parked
	// handler, which only returns once release is closed.
	defer close(release)

	got := make(chan struct{}, 1)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	n := testNotifier(t)
	n.enqueue(job{hook: store.Webhook{ID: "hang", URL: hanging.URL, Enabled: true}, event: EventProgress, runID: "r"})
	n.enqueue(job{hook: store.Webhook{ID: "ok", URL: healthy.URL, Enabled: true}, event: EventProgress, runID: "r"})

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("a healthy hook waited behind a hanging one; lanes are not isolated")
	}
}

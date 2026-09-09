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
	n.queue <- job{
		hook:  store.Webhook{ID: "w1", URL: srv.URL, SecretEncrypted: secret, Enabled: true},
		event: EventRunCompleted, runID: "run-1",
		payload: Payload{"event": EventRunCompleted, "run_id": "run-1", "status": "success"},
	}

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

	// Fill every worker with a hanging delivery, then time the next enqueue.
	for i := range 8 {
		n.queue <- job{
			hook:  store.Webhook{ID: "hang", URL: srv.URL, Enabled: true},
			event: EventProgress, runID: "run-hang", payload: Payload{"i": i},
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			select {
			case n.queue <- job{
				hook:  store.Webhook{ID: "hang", URL: srv.URL, Enabled: true},
				event: EventProgress, runID: "run-hang", payload: Payload{"j": i},
			}:
			default:
				// Full is the designed outcome: dropped, never blocked.
			}
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

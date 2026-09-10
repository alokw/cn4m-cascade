package notify

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// A cn4m callback must arrive as form fields, not as JSON.
//
// The receiver here parses the request the way cn4m's endpoint does —
// r.ParseForm and three named values — rather than by inspecting the body this
// package produced. Posting JSON to /suite/status delivers a body it cannot
// read, which is exactly the bug this test exists to catch.
func TestCN4MCallbackArrivesAsFormFields(t *testing.T) {
	type received struct {
		contentType string
		app         string
		message     string
		level       string
	}
	got := make(chan received, 1)

	cn4m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("cn4m could not parse the request: %v", err)
		}
		got <- received{
			contentType: r.Header.Get("Content-Type"),
			app:         r.PostFormValue("app"),
			message:     r.PostFormValue("message"),
			level:       r.PostFormValue("level"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cn4m.Close()

	n := testNotifier(t)
	n.queue <- job{
		hook: store.Webhook{
			ID: "cn4m", URL: cn4m.URL, Enabled: true, Format: store.FormatCN4M,
		},
		event: EventRunCompleted, runID: "run-1",
		payload: Payload{
			"event": EventRunCompleted, "job": "photos-to-nas",
			"status": "success", "files_done": 1204, "errors_count": 0,
		},
	}

	select {
	case r := <-got:
		if r.contentType != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q, want application/x-www-form-urlencoded", r.contentType)
		}
		if r.app != AppName {
			t.Fatalf("app = %q, want %q", r.app, AppName)
		}
		if r.level == "" {
			t.Fatal("no level was sent")
		}
		// The message is what a person reads in the suite view, so it has to
		// say what happened in the first few words.
		if !contains(r.message, "Sync Complete") {
			t.Fatalf("message %q does not say what happened", r.message)
		}
		// And it must never carry an identifier. The payload has job_id and
		// no job name, so any attempt to name the job prints a 32-character
		// hex string and fills the row with the least useful thing on it
		// (D-128).
		if contains(r.message, "photos-to-nas") || contains(r.message, "job-1") {
			t.Fatalf("message %q carries a job identifier", r.message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cn4m received nothing")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A generic JSON hook must be unaffected by the cn4m path existing.
func TestJSONFormatStillPostsJSON(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(t)
	n.queue <- job{
		hook:    store.Webhook{ID: "j", URL: srv.URL, Enabled: true, Format: store.FormatJSON},
		event:   EventRunStarted,
		payload: Payload{"event": EventRunStarted, "job": "x"},
	}

	select {
	case ct := <-got:
		if ct != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", ct)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// A cn4m callback that cannot be delivered must not touch the run log.
//
// n.db is nil here, so any attempt to write a run_event would panic — which is
// the assertion: absence of cn4m is a normal state, not a fault of the sync.
func TestBestEffortFailureNeverReachesTheRunLog(t *testing.T) {
	n := New(nil, plainLookup, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A port nothing is listening on, so every attempt fails fast.
	n.deliver(t.Context(), job{
		hook: store.Webhook{
			ID: "cn4m", URL: "http://127.0.0.1:1/suite/status",
			Enabled: true, Format: store.FormatCN4M,
		},
		event: EventRunFailed, runID: "run-1",
		payload: Payload{"event": EventRunFailed, "job": "x"},
	})
	// Reaching here without a panic is the result.
}

// The message stays to one line whatever the error contained: cn4m shows a
// single row per app, so a wrapped multi-line error would push the rest of the
// suite view off the screen.
func TestCN4MMessageIsAlwaysOneShortLine(t *testing.T) {
	long := "mounting //192.168.1.50/media failed:\nconnection timed out\n" +
		"after 20s while the host was unreachable and several other things besides, at length"

	msg := cn4mMessage(EventRunFailed, Payload{"job": "backup", "last_error": long})

	for _, r := range msg {
		if r == '\n' || r == '\r' {
			t.Fatalf("the message contains a newline: %q", msg)
		}
	}
	if len(msg) > 200 {
		t.Fatalf("the message is %d characters; cn4m shows one row", len(msg))
	}
	if !contains(msg, "Sync Failed") {
		t.Fatalf("message %q does not lead with what happened", msg)
	}
	if contains(msg, "backup") {
		t.Fatalf("message %q carries a job identifier (D-128)", msg)
	}
}

// The format the suite view is actually read for: how far along, and how fast.
func TestCN4MProgressReportsPercentAndSpeed(t *testing.T) {
	tests := []struct {
		name    string
		payload Payload
		want    string
	}{
		{
			name:    "percent and speed",
			payload: Payload{"bytes_done": 650, "bytes_total": 1000, "throughput_bps": 910000000.0},
			want:    "Sync in Progress: 65%, 910 MB/s",
		},
		{
			name:    "bytes win over files, being the better measure of remaining work",
			payload: Payload{"bytes_done": 100, "bytes_total": 1000, "files_done": 9, "files_total": 10},
			want:    "Sync in Progress: 10%",
		},
		{
			name:    "files are used when no byte total is known yet",
			payload: Payload{"files_done": 9, "files_total": 10},
			want:    "Sync in Progress: 90%",
		},
		{
			name:    "speed alone while the scan has not produced a total",
			payload: Payload{"throughput_bps": 2500000.0},
			want:    "Sync in Progress: 2 MB/s",
		},
		{
			// Totals are revised while the scan runs, so done can briefly
			// exceed the total known so far. "104%" reads as a bug.
			name:    "percent is clamped",
			payload: Payload{"bytes_done": 1040, "bytes_total": 1000},
			want:    "Sync in Progress: 100%",
		},
		{
			name:    "neither known: no invented zero",
			payload: Payload{},
			want:    "Sync in Progress",
		},
		{
			name:    "a stalled transfer reports no speed rather than 0 B/s",
			payload: Payload{"bytes_done": 500, "bytes_total": 1000, "throughput_bps": 0.0},
			want:    "Sync in Progress: 50%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cn4mMessage(EventProgress, tt.payload); got != tt.want {
				t.Fatalf("message = %q, want %q", got, tt.want)
			}
		})
	}
}

// Every message a person can see, in one place, so a change to the wording is
// a deliberate edit to this list rather than a surprise in the suite view.
func TestCN4MMessagesCarryNoIdentifiers(t *testing.T) {
	const id = "9fc012f2ac2524cc9bf41333e51cfc6b"
	events := []struct {
		event   string
		payload Payload
	}{
		{EventRunStarted, Payload{"job_id": id}},
		{EventProgress, Payload{"job_id": id, "bytes_done": 1, "bytes_total": 2}},
		{EventPrompt, Payload{"job_id": id}},
		{EventRunCompleted, Payload{"job_id": id, "status": "success", "files_done": 3}},
		{EventRunCompleted, Payload{"job_id": id, "status": "partial", "files_done": 3, "errors_count": 1}},
		{EventRunCompleted, Payload{"job_id": id, "status": "cancelled", "files_done": 3}},
		{EventRunFailed, Payload{"job_id": id, "last_error": "the host is down"}},
	}
	for _, e := range events {
		msg := cn4mMessage(e.event, e.payload)
		if contains(msg, id) {
			t.Errorf("%s: message %q contains the job id", e.event, msg)
		}
		if !contains(msg, "Sync") {
			t.Errorf("%s: message %q does not read as a sync status", e.event, msg)
		}
	}
}

// The level has to distinguish the three outcomes `run_completed` covers.
//
// Sending green for a partial run would report "one destination was skipped
// because the NAS was off" as an unqualified success — the exact thing a
// separate warning level exists to prevent.
func TestCN4MLevelsCoverEveryOutcome(t *testing.T) {
	cases := []struct {
		name   string
		event  string
		status string
		want   string
	}{
		{"a run beginning", EventRunStarted, "running", levelWorking},
		{"progress", EventProgress, "running", levelWorking},
		{"waiting on a person", EventPrompt, "running", levelBlocked},
		{"a clean finish", EventRunCompleted, "success", levelOK},
		{"a partial finish", EventRunCompleted, "partial", levelWarning},
		{"a cancelled run", EventRunCompleted, "cancelled", levelWarning},
		{"a failure", EventRunFailed, "failed", levelError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cn4mLevel(tc.event, Payload{"status": tc.status})
			if got != tc.want {
				t.Fatalf("level for %s/%s = %q, want %q", tc.event, tc.status, got, tc.want)
			}
		})
	}
}

// Every level this sends must be one cn4m actually accepts. A typo here is
// invisible until the suite view shows the wrong colour for a backup.
func TestEveryEmittedLevelIsInCN4MsVocabulary(t *testing.T) {
	accepted := map[string]bool{
		levelIdle: true, levelWorking: true, levelOK: true,
		levelWarning: true, levelBlocked: true, levelError: true,
	}

	for _, event := range []string{
		EventRunStarted, EventProgress, EventPrompt, EventRunCompleted, EventRunFailed,
		"some_event_added_later",
	} {
		for _, status := range []string{"running", "success", "partial", "cancelled", "failed", ""} {
			if got := cn4mLevel(event, Payload{"status": status}); !accepted[got] {
				t.Fatalf("event %q status %q produced %q, which cn4m does not accept",
					event, status, got)
			}
		}
	}
}

// A best-effort endpoint that is simply absent must be left alone rather than
// retried on every event for the life of the process.
//
// cn4m's own client documents this: it backs off from a minute, doubling per
// consecutive failure. Without it, an installation without cn4m does three
// pointless connection attempts per event, per run, forever.
func TestAnAbsentBestEffortEndpointIsBackedOffNotHammered(t *testing.T) {
	n := New(nil, plainLookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook := store.Webhook{
		ID: "cn4m", URL: "http://127.0.0.1:1/suite/status",
		Enabled: true, Format: store.FormatCN4M,
	}

	if n.backedOff(hook) {
		t.Fatal("an endpoint that has never failed is already backed off")
	}

	n.noteOutcome(hook, false)
	if !n.backedOff(hook) {
		t.Fatal("a failed best-effort endpoint is not backed off")
	}

	// The window widens with each consecutive failure.
	n.mu.Lock()
	first := n.down[hook.ID].retryAt
	n.mu.Unlock()

	n.noteOutcome(hook, false)

	n.mu.Lock()
	second := n.down[hook.ID].retryAt
	n.mu.Unlock()

	if !second.After(first) {
		t.Fatalf("the retry window did not widen: %v then %v", first, second)
	}

	// And one success ends it outright — a receiver that has come back should
	// not spend the next half hour being ignored.
	n.noteOutcome(hook, true)
	if n.backedOff(hook) {
		t.Fatal("an endpoint that answered is still backed off")
	}
}

// The breaker applies only to best-effort hooks. A JSON webhook's failures are
// reported against the run, so suppressing its deliveries would hide something
// the user explicitly asked to be told about.
func TestTheBreakerDoesNotApplyToJSONWebhooks(t *testing.T) {
	n := New(nil, plainLookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook := store.Webhook{ID: "j", URL: "http://127.0.0.1:1/hook", Enabled: true, Format: store.FormatJSON}

	n.noteOutcome(hook, false)
	n.noteOutcome(hook, false)

	if n.backedOff(hook) {
		t.Fatal("a JSON webhook was backed off; its failures are visible and should keep being retried")
	}
}

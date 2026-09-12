package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// The line a person reads while scrolling a channel. The emoji is the part that
// carries at a distance, so each outcome must have its own.
func TestDiscordContent(t *testing.T) {
	tests := []struct {
		name     string
		event    string
		payload  Payload
		want     string
		wantIcon string
	}{
		{
			name:     "success uses the run's own summary",
			event:    EventRunCompleted,
			payload:  Payload{"status": "success", "files_done": 6, "last_error": "6 succeeded, 0 failed, 1 skipped"},
			want:     "**Sync complete** — 6 succeeded, 0 failed, 1 skipped",
			wantIcon: discordOK,
		},
		{
			name:     "partial is not reported as success",
			event:    EventRunCompleted,
			payload:  Payload{"status": "partial", "files_done": 4, "last_error": "1 succeeded, 0 failed, 1 skipped"},
			want:     "**Sync incomplete** — 1 succeeded, 0 failed, 1 skipped",
			wantIcon: discordWarning,
		},
		{
			name:     "failed completion",
			event:    EventRunCompleted,
			payload:  Payload{"status": "failed", "files_done": 0, "last_error": "0 succeeded, 1 failed, 0 skipped"},
			want:     "**Sync failed** — 0 succeeded, 1 failed, 0 skipped",
			wantIcon: discordFailed,
		},
		{
			name:     "cancelled is distinct from failed",
			event:    EventRunCompleted,
			payload:  Payload{"status": "cancelled", "files_done": 3, "last_error": "cancelled by the operator"},
			want:     "**Sync cancelled**",
			wantIcon: discordStopped,
		},
		{
			// Without a summary the file count is the fallback, so the message
			// still says something.
			name:     "no summary falls back to the file count",
			event:    EventRunCompleted,
			payload:  Payload{"status": "success", "files_done": 12},
			want:     "**Sync complete** — 12 file(s)",
			wantIcon: discordOK,
		},
		{
			name:     "run_failed carries the reason",
			event:    EventRunFailed,
			payload:  Payload{"last_error": "mounting //10.10.20.42/media failed: the host is down"},
			want:     "the host is down",
			wantIcon: discordFailed,
		},
		{
			// The path that actually fires for a failed run, and the one the
			// first implementation missed: run_failed, not run_completed.
			name:  "run_failed keeps the tally and drops the detail",
			event: EventRunFailed,
			payload: Payload{"last_error": "0 succeeded, 1 failed, 0 skipped — 3dec3ad71492f4a18813bb49ae65c44a " +
				"failed: destination \"broken\" (C:/Temp/missing) is unavailable: does not exist"},
			want:     "**Sync failed** — 0 succeeded, 1 failed, 0 skipped",
			wantIcon: discordFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := discordContent(tc.event, tc.payload)

			if !strings.Contains(got, tc.want) {
				t.Errorf("message %q does not contain %q", got, tc.want)
			}
			if !strings.Contains(got, tc.wantIcon) {
				t.Errorf("message %q is missing the %q icon", got, tc.wantIcon)
			}
			if !strings.Contains(got, discordSync) {
				t.Errorf("message %q is missing the sync icon", got)
			}
		})
	}
}

// Discord rejects a body it cannot parse, and an over-long content field, so
// the payload has to be valid JSON with a non-empty content string whatever it
// was handed.
func TestDiscordBodyIsValidJSON(t *testing.T) {
	events := []struct {
		event   string
		payload Payload
	}{
		{EventRunStarted, Payload{}},
		{EventProgress, Payload{"bytes_done": 5, "bytes_total": 10}},
		{EventPrompt, Payload{}},
		{EventRunCompleted, Payload{"status": "success", "files_done": 1}},
		{EventRunFailed, Payload{"last_error": strings.Repeat("a very long error ", 200)}},
		{"some_unmapped_event", Payload{}},
	}

	for _, e := range events {
		body, err := discordBody(e.event, e.payload)
		if err != nil {
			t.Fatalf("%s: %v", e.event, err)
		}
		var msg discordMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("%s: body is not valid JSON: %v", e.event, err)
		}
		if strings.TrimSpace(msg.Content) == "" {
			t.Errorf("%s: empty content, which Discord rejects", e.event)
		}
		// Discord's limit is 2000 characters.
		if len([]rune(msg.Content)) > 2000 {
			t.Errorf("%s: content is %d characters; Discord's limit is 2000", e.event, len([]rune(msg.Content)))
		}
	}
}

// Delivery failures must never reach the run log or fail a sync: a chat
// notification is somebody else's service being down, not a problem with the
// backup (the same rule cn4m follows, §5o).
func TestDiscordIsBestEffort(t *testing.T) {
	if !(&store.Webhook{Format: store.FormatDiscord}).BestEffort() {
		t.Fatal("a Discord webhook is not best-effort; a failed chat message would fail a run")
	}
}

// The wire shape Discord expects, and the content type that goes with it.
func TestDiscordBodyForUsesJSON(t *testing.T) {
	body, contentType, err := bodyFor(
		store.Webhook{Format: store.FormatDiscord},
		EventRunCompleted,
		Payload{"status": "success", "files_done": 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "application/json" {
		t.Errorf("content type = %q, want application/json", contentType)
	}
	if !strings.Contains(string(body), "Sync complete") {
		t.Errorf("body %q does not carry the message", body)
	}
}

// A chat line has to be scannable. Neither a 32-character destination id nor a
// full filesystem path belongs in one, and the detail is in the run log.
func TestDiscordDropsIdentifiersAndPathsFromFailures(t *testing.T) {
	const id = "3dec3ad71492f4a18813bb49ae65c44a"
	summary := "0 succeeded, 1 failed, 0 skipped — " + id +
		" failed: destination \"broken\" (C:/Users/me/Temp/missing) is unavailable: does not exist"

	for _, event := range []string{EventRunFailed, EventRunCompleted} {
		payload := Payload{"last_error": summary, "status": "failed", "files_done": 0}
		msg := discordContent(event, payload)

		if strings.Contains(msg, id) {
			t.Errorf("%s: message carries the destination id: %q", event, msg)
		}
		if strings.Contains(msg, "C:") {
			t.Errorf("%s: message carries a filesystem path: %q", event, msg)
		}
		if !strings.Contains(msg, "0 succeeded, 1 failed, 0 skipped") {
			t.Errorf("%s: message lost the tally: %q", event, msg)
		}
		if len([]rune(msg)) > 120 {
			t.Errorf("%s: message is %d characters; a chat line should be scannable: %q", event, len([]rune(msg)), msg)
		}
	}
}

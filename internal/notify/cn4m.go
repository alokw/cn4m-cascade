package notify

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// AppName is what this service calls itself to cn4m.
//
// cn4m keys a suite entry on it — the sibling service in the same suite
// reports as "inbound" — so it must stay stable across releases.
const AppName = "cascade"

// cn4m's status levels, as of 2026-09-06.
//
// Gathered here because this is the only place in the codebase that spells a
// level, and because getting one wrong is **silent**.
//
// Verified against a live cn4m (2026-09-09): an unrecognised level is not
// rejected. `/suite/status` answers 201 and coerces it to `idle` — grey,
// "nothing happening". So a typo would render a *failed* backup as a resting
// one, with no error, no warning, and nothing in any log to notice. The
// server cannot tell us we are wrong, which is why
// TestEveryEmittedLevelIsInCN4MsVocabulary exists: it is the only thing
// standing between a typo here and a suite view that quietly misreports
// whether backups are working.
const (
	levelIdle     = "idle"     // grey — nothing happening, the resting state
	levelWorking  = "working"  // orange — in progress right now; a real event
	levelProgress = "progress" // orange — a live counter, replaced not appended
	levelOK       = "ok"       // green — finished, nothing to do
	levelWarning  = "warning"  // yellow — finished, but not cleanly
	levelBlocked  = "blocked"  // purple — waiting on a person, timeout running
	levelError    = "error"    // red — finished badly, or could not run at all
)

// `progress` (added to cn4m 2026-09-12) is the one level with different
// semantics from the rest: it goes to a **per-app slot** rather than the feed.
// Each post replaces the previous line, and it never reaches the tray or the
// log — the same rule cn4m's own set_app_progress() follows. Before it existed,
// a five-second progress cadence put a dozen "Sync in Progress: 16%" rows into
// the feed per minute, every one of them a stale duplicate of the next.
//
// The slot is cleared by the app's next non-progress post, which is why the
// outcome still goes out at its own level, and it is dropped after two minutes
// of silence so a cascade that dies mid-sync does not leave "16%" up for good.
// Our cadence is far inside that: the runner emits progress every flush tick,
// on a timer rather than on file boundaries, so even a single 100 GB file keeps
// the line alive.

// cn4mLevel picks the level for one event.
//
// A function rather than a table because `run_completed` covers three
// different outcomes: a clean run, a partial one, and a cancelled one. Sending
// green for all three would report "one destination was skipped because the
// NAS was off" as an unqualified success, which is the exact failure a
// separate `warning` exists to prevent.
//
// `blocked` rather than `warning` for a waiting destination is the other
// distinction worth keeping: a prompt has a timeout running and is actionable
// *now*, whereas a partial run is history.
func cn4mLevel(event string, payload Payload) string {
	switch event {
	case EventRunStarted:
		// A real event, so it lands in the feed and the log; the progress
		// lines that follow it only ever replace one another in the slot.
		return levelWorking

	case EventProgress:
		return levelProgress

	case EventPrompt:
		return levelBlocked

	case EventRunFailed:
		return levelError

	case EventRunCompleted:
		switch stringField(payload, "status") {
		case string(store.RunSuccess):
			return levelOK
		case string(store.RunFailed):
			return levelError
		default:
			// partial and cancelled: finished, nothing broken, but it should
			// not read as green.
			return levelWarning
		}
	}
	// An unmapped event still reports, and `working` is the value least
	// likely to mislead: it never claims something is wrong, and never claims
	// everything is fine.
	return levelWorking
}

// cn4mForm renders one event as the form fields /suite/status accepts.
//
// Form-encoded, not JSON: the reference client posts `-d app=… -d message=…
// -d level=…`, and a JSON body to that endpoint delivers three fields it
// cannot see.
func cn4mForm(event string, payload Payload) url.Values {
	return url.Values{
		"app":     {AppName},
		"message": {cn4mMessage(event, payload)},
		"level":   {cn4mLevel(event, payload)},
	}
}

// cn4mMessage renders the one line a person reads in the suite view.
//
// Short and specific, because that is the whole contract: cn4m shows a single
// status line per app, so a person scanning the suite view wants to know
// whether a sync is running and how far along it is, in the first few words.
//
// **No job identifier appears here, deliberately** (D-128). The payload of
// SPEC.md §8.1 carries `job_id` and no job *name*, so naming the job meant
// printing a 32-character hex string — "Started 9fc012f2ac2524cc9bf41333e51cfc6b"
// — which fills the row with the least useful thing on it. Percentage and
// throughput are what a person actually reads.
func cn4mMessage(event string, payload Payload) string {
	switch event {
	case EventRunStarted:
		return "Sync Started"

	case EventProgress:
		return "Sync in Progress" + progressDetail(payload)

	case EventPrompt:
		return "Sync Paused: waiting on an unreachable destination"

	case EventRunCompleted:
		files := intField(payload, "files_done")
		switch stringField(payload, "status") {
		case string(store.RunSuccess):
			return fmt.Sprintf("Sync Complete: %d files", files)
		case string(store.RunCancelled):
			return fmt.Sprintf("Sync Cancelled: %d files copied", files)
		case string(store.RunFailed):
			if last := stringField(payload, "last_error"); last != "" {
				return fmt.Sprintf("Sync Failed: %s", truncate(last, 160))
			}
			return "Sync Failed"
		default:
			// partial: finished, nothing broken, but not clean either.
			if errs := intField(payload, "errors_count"); errs > 0 {
				return fmt.Sprintf("Sync Incomplete: %d files, %d destination errors", files, errs)
			}
			return fmt.Sprintf("Sync Incomplete: %d files", files)
		}

	case EventRunFailed:
		if last := stringField(payload, "last_error"); last != "" {
			return fmt.Sprintf("Sync Failed: %s", truncate(last, 160))
		}
		return "Sync Failed"
	}
	return "Sync: " + event
}

// progressDetail renders ": 65%, 910 MB/s" — whichever of the two is known.
//
// Both are omitted rather than guessed at. A percentage needs a total, and the
// scan that produces one runs concurrently with the copy, so early progress
// events legitimately have nothing to divide by; printing "0%" there would
// show a stalled sync that is in fact working.
func progressDetail(payload Payload) string {
	var parts []string

	if pct, ok := percentDone(payload); ok {
		parts = append(parts, fmt.Sprintf("%d%%", pct))
	}
	if bps := floatField(payload, "throughput_bps"); bps > 0 {
		parts = append(parts, perSecond(bps))
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, ", ")
}

// percentDone prefers bytes over files: a run whose remaining files are the
// large ones is not as far along as a file count suggests.
func percentDone(payload Payload) (int, bool) {
	for _, pair := range [][2]string{
		{"bytes_done", "bytes_total"},
		{"files_done", "files_total"},
	} {
		done, total := floatField(payload, pair[0]), floatField(payload, pair[1])
		if total <= 0 {
			continue
		}
		pct := int(done / total * 100)
		// Clamped, not trusted: totals are revised while the scan is still
		// running, so done can briefly exceed the total known so far, and
		// "104%" in a suite view reads as a bug in the sync.
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		return pct, true
	}
	return 0, false
}

// perSecond formats a byte rate in the decimal units people quote transfer
// speeds in, so 910 MB/s reads the way it would on a network graph.
func perSecond(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.1f GB/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.0f MB/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.0f KB/s", bps/1e3)
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

func stringField(p Payload, key string) string {
	s, _ := p[key].(string)
	return s
}

// floatField reads a numeric field, tolerating both the float64 a decoded JSON
// number becomes and the int a payload built in-process carries.
func floatField(p Payload, key string) float64 {
	switch v := p[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// intField reads a count, tolerating the float64 a decoded JSON number becomes.
func intField(p Payload, key string) int {
	switch v := p[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// truncate keeps a status line to one line. cn4m shows a single row per app,
// so a wrapped stack trace would push everything else off it.
func truncate(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// bodyFor renders a payload in the format a hook expects, and returns the
// content type to send it with.
func bodyFor(hook store.Webhook, event string, payload Payload) ([]byte, string, error) {
	switch hook.Format {
	case store.FormatCN4M:
		return []byte(cn4mForm(event, payload).Encode()),
			"application/x-www-form-urlencoded", nil
	case store.FormatDiscord:
		body, err := discordBody(event, payload)
		return body, "application/json", err
	}
	body, err := jsonBody(payload)
	return body, "application/json", err
}

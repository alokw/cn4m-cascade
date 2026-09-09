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
	levelIdle    = "idle"    // grey — nothing happening, the resting state
	levelWorking = "working" // orange — in progress right now
	levelOK      = "ok"      // green — finished, nothing to do
	levelWarning = "warning" // yellow — finished, but not cleanly
	levelBlocked = "blocked" // purple — waiting on a person, timeout running
	levelError   = "error"   // red — finished badly, or could not run at all
)

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
	case EventRunStarted, EventProgress:
		return levelWorking

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
// status line per app, so "Mirroring photos-to-nas" beats a serialised
// snapshot, and a run that failed has to say so in the first few words.
func cn4mMessage(event string, payload Payload) string {
	job := stringField(payload, "job")
	if job == "" {
		job = stringField(payload, "job_id")
	}

	switch event {
	case EventRunStarted:
		return fmt.Sprintf("Started %s", job)

	case EventProgress:
		done, total := intField(payload, "files_done"), intField(payload, "files_total")
		if total > 0 {
			return fmt.Sprintf("Syncing %s — %d/%d files", job, done, total)
		}
		return fmt.Sprintf("Syncing %s", job)

	case EventPrompt:
		return fmt.Sprintf("%s is waiting: a destination is unreachable", job)

	case EventRunCompleted:
		status := stringField(payload, "status")
		files := intField(payload, "files_done")
		if errs := intField(payload, "errors_count"); errs > 0 {
			return fmt.Sprintf("%s finished %s — %d file(s), %d destination error(s)",
				job, status, files, errs)
		}
		return fmt.Sprintf("%s finished %s — %d file(s)", job, status, files)

	case EventRunFailed:
		if last := stringField(payload, "last_error"); last != "" {
			return fmt.Sprintf("%s failed: %s", job, truncate(last, 160))
		}
		return fmt.Sprintf("%s failed", job)
	}
	return fmt.Sprintf("%s: %s", job, event)
}

func stringField(p Payload, key string) string {
	s, _ := p[key].(string)
	return s
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
	if hook.Format == store.FormatCN4M {
		return []byte(cn4mForm(event, payload).Encode()),
			"application/x-www-form-urlencoded", nil
	}
	body, err := jsonBody(payload)
	return body, "application/json", err
}

package notify

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// Discord renders one line into a chat channel, so the constraints are the same
// as cn4m's (§5o) for the same reason: a person skims it between other messages
// and needs the outcome in the first glance, not a serialised snapshot.
//
// The emoji carry the status at a distance — a channel is read by scrolling, and
// colour-and-shape is quicker than words.
const (
	discordSync    = "🔄"
	discordOK      = "✅"
	discordWarning = "⚠️"
	discordFailed  = "❌"
	discordStopped = "⏹️"
)

// discordMessage is Discord's webhook body. `content` alone is deliberate:
// embeds render as a bordered card, which is heavier than a status line needs
// and looks broken when several arrive together.
type discordMessage struct {
	Content string `json:"content"`
}

// discordBody renders one event as a Discord webhook payload.
func discordBody(event string, payload Payload) ([]byte, error) {
	return json.Marshal(discordMessage{Content: discordContent(event, payload)})
}

// discordContent is the line a person reads.
//
// No job identifier appears, for the reason D-128 records: SPEC.md §8.1's
// payload carries `job_id` and no job *name*, so naming the job would print a
// 32-character hex string — the least useful thing the line could hold.
func discordContent(event string, payload Payload) string {
	switch event {
	case EventRunCompleted:
		files := intField(payload, "files_done")
		switch stringField(payload, "status") {
		case string(store.RunSuccess):
			return fmt.Sprintf("%s %s **Sync complete** — %s", discordSync, discordOK, outcome(payload, files))
		case string(store.RunCancelled):
			return fmt.Sprintf("%s %s **Sync cancelled** — %s", discordSync, discordStopped, outcome(payload, files))
		case string(store.RunFailed):
			return fmt.Sprintf("%s %s **Sync failed** — %s", discordSync, discordFailed, outcome(payload, files))
		default:
			// partial: finished, nothing broken, but not clean either.
			return fmt.Sprintf("%s %s **Sync incomplete** — %s", discordSync, discordWarning, outcome(payload, files))
		}

	case EventRunFailed:
		// A failed run emits run_failed, not run_completed, so this branch needs
		// the same trimming — missing that is how the first version still put a
		// wall of text and a hex id into the channel.
		if last := stringField(payload, "last_error"); last != "" {
			return fmt.Sprintf("%s %s **Sync failed** — %s", discordSync, discordFailed, shortReason(last))
		}
		return fmt.Sprintf("%s %s **Sync failed**", discordSync, discordFailed)

	case EventRunStarted:
		return fmt.Sprintf("%s **Sync started**", discordSync)

	case EventPrompt:
		return fmt.Sprintf("%s %s **Sync waiting** — a destination is unreachable and the run is asking what to do",
			discordSync, discordWarning)

	case EventProgress:
		return fmt.Sprintf("%s **Sync in progress**%s", discordSync, progressDetail(payload))
	}
	return fmt.Sprintf("%s Sync: %s", discordSync, event)
}

// outcome is the run's own per-destination tally: "6 succeeded, 0 failed, 1
// skipped". The runner already composes that phrasing when it finishes a run,
// and it is what an operator sees everywhere else, so it is reused rather than
// rebuilt from the counters where it could drift.
//
// **Only the tally, never the detail that follows it.** A failed run's summary
// continues past the counts with the destination id, the path, and the full
// error — which in a chat channel is a wall of text ending in a mid-word
// truncation, led by a 32-character hex string nobody can act on (the same
// reason D-128 took identifiers out of the cn4m line). The detail is in the run
// log, which is where somebody goes once the message has told them to look.
func outcome(payload Payload, files int) string {
	summary := stringField(payload, "last_error")
	if summary == "" {
		return fmt.Sprintf("%d file(s)", files)
	}
	return shortReason(summary)
}

// shortReason reduces a run summary to the part worth putting in a chat line.
//
// The runner joins its tally to the detail with an em dash — "0 succeeded, 1
// failed, 0 skipped — <destination id> failed: <path> is unavailable: …" — so
// everything before that separator is the scannable half. A summary with no
// separator is already a plain sentence (a run that failed before any
// destination was reached, say) and is simply capped.
func shortReason(summary string) string {
	if counts, _, found := strings.Cut(summary, " — "); found {
		return strings.TrimSpace(counts)
	}
	return truncate(summary, 200)
}

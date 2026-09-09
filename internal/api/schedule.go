package api

import (
	"net/http"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// schedulePreviewRequest asks what a cron expression actually means.
type schedulePreviewRequest struct {
	Expression string `json:"expression"`
}

// schedulePreviewResponse answers it.
//
// Advisory, like /api/filters/check-file: it never blocks a save and a bad
// expression is a 200 with Valid false, not a 4xx. The editor asks on every
// keystroke, and half-typed input is the normal case, not an error.
type schedulePreviewResponse struct {
	Valid bool   `json:"valid"`
	Error string `json:"error,omitempty"`
	// Timezone the server evaluated in, so "2am" can be checked against the
	// zone that will actually run it rather than assumed.
	Timezone string `json:"timezone"`
	// Next is the upcoming firings, as RFC 3339 instants with an offset. Three
	// of them rather than one: a single time cannot show that "0 2 * * 1"
	// means weekly, and an empty list is how an expression that parses but
	// never occurs — "0 0 30 2 *", the 30th of February — makes itself
	// visible instead of silently never running.
	Next []time.Time `json:"next"`
	// NextHere is the same firings rendered in the *server's* zone.
	//
	// Sent preformatted because it is the only way the browser can show it: an
	// instant always renders in the viewer's zone, so "16 2 * * *" on a UTC
	// server appears to a reader in UTC-7 as 7:16pm and looks like a bug in
	// the scheduler. It is not — it is the same moment — but "you typed 2:16
	// and we are showing 7:16" needs both numbers on screen to be anything
	// other than alarming.
	NextHere []string `json:"next_here"`
}

// previewCount is how many upcoming firings to show.
const previewCount = 3

// handleSchedulePreview resolves a cron expression to real times.
//
// This exists because a cron string cannot be checked by reading it. Whether
// "0 2 * * *" means what someone intended depends on the server's timezone,
// which is invisible in the expression, and typos like a six-field expression
// or an out-of-range hour produce something that looks plausible. Showing the
// next three firings turns all of that into something answerable at a glance.
func (s *Server) handleSchedulePreview(w http.ResponseWriter, r *http.Request) {
	var req schedulePreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"The request body is not valid JSON for a schedule preview.", err.Error())
		return
	}

	resp := schedulePreviewResponse{
		Timezone: time.Local.String(),
		Next:     []time.Time{},
		NextHere: []string{},
	}

	sched, err := store.ParseSchedule(req.Expression)
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Valid = true

	// Local, matching how the scheduler evaluates it. A UTC instant here would
	// make the preview disagree with the thing it is previewing the moment
	// anyone sets TZ — which the UI tells them to do.
	at := time.Now()
	for range previewCount {
		next := sched.Next(at)
		if next.IsZero() {
			// A valid expression for a date that never comes. Stop rather than
			// looping on a zero that will never advance.
			break
		}
		resp.Next = append(resp.Next, next)
		resp.NextHere = append(resp.NextHere, next.Format("Mon 2 Jan, 15:04"))
		at = next
	}
	writeJSON(w, http.StatusOK, resp)
}

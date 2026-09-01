package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// handleLogs serves the global log view: events across every run, filtered by
// level, job and time (SPEC.md §8's /api/logs, §9's Logs page).
//
// Newest first, unlike a single run's task log — this is the view someone
// leaves open to watch for errors, not a transcript read top to bottom.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := store.EventFilter{
		JobID:  q.Get("job_id"),
		RunID:  q.Get("run_id"),
		Level:  store.EventLevel(q.Get("level")),
		Newest: true,
	}

	if filter.Level != "" {
		switch filter.Level {
		case store.LevelInfo, store.LevelWarn, store.LevelError:
		default:
			writeError(w, http.StatusBadRequest, "invalid_level",
				`level must be "info", "warn" or "error".`, string(filter.Level))
			return
		}
	}

	if raw := q.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_since",
				"since must be an RFC 3339 timestamp, for example 2026-09-01T12:00:00Z.", err.Error())
			return
		}
		filter.Since = since
	}

	if limit, err := strconv.Atoi(q.Get("limit")); err == nil {
		filter.Limit = limit
	}
	if offset, err := strconv.Atoi(q.Get("offset")); err == nil {
		filter.Offset = offset
	}

	events, err := s.db.ListEvents(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", "Could not read the log.", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

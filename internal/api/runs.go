package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// runResponse is a run record plus, while it is still going, live progress
// that has not yet been flushed to the database.
type runResponse struct {
	*store.Run
	Progress *runner.RunSnapshot        `json:"progress,omitempty"`
	Counts   map[store.EventLevel]int64 `json:"event_counts,omitempty"`
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	filter := store.RunFilter{
		JobID:  r.URL.Query().Get("job_id"),
		Status: store.RunStatus(r.URL.Query().Get("status")),
	}
	if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		filter.Limit = limit
	}

	runs, err := s.db.ListRuns(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", "Could not list runs.", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.db.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, err)
		return
	}

	resp := runResponse{Run: run}
	// A running run's counters live in memory and are only flushed once a
	// second, so serve the live view rather than the stale row.
	if snap, ok := s.runner.Progress(run.ID); ok {
		resp.Progress = &snap
	}
	if counts, err := s.db.CountEventsByLevel(r.Context(), run.ID); err == nil {
		resp.Counts = counts
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := s.db.GetRun(r.Context(), runID); err != nil {
		s.writeRunError(w, err)
		return
	}

	filter := store.EventFilter{
		RunID:        runID,
		Level:        store.EventLevel(r.URL.Query().Get("level")),
		DestTargetID: r.URL.Query().Get("dest"),
	}
	if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		filter.Limit = limit
	}
	if offset, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil {
		filter.Offset = offset
	}

	events, err := s.db.ListEvents(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", "Could not list run events.", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	if err := s.runner.Cancel(runID); err != nil {
		if errors.Is(err, runner.ErrNotRunning) {
			// Distinguish "never existed" from "already finished".
			if _, dbErr := s.db.GetRun(r.Context(), runID); dbErr != nil {
				s.writeRunError(w, dbErr)
				return
			}
			writeError(w, http.StatusConflict, "not_running",
				"That run has already finished, so there is nothing to cancel.", "")
			return
		}
		writeError(w, http.StatusInternalServerError, "cancel_failed", "Could not cancel the run.", err.Error())
		return
	}

	s.log.Info("run cancellation requested", "run_id", runID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func (s *Server) writeRunError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No such run.", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "run_error", err.Error(), "")
}

// promptRequest answers a target-unavailable prompt (SPEC.md §8).
type promptRequest struct {
	Action       runner.PromptAction `json:"action"`
	DestTargetID string              `json:"dest_target_id"`
}

// handlePrompt records a human's decision about an unavailable destination,
// completing the `prompt` policy of SPEC.md §6.6.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := s.db.GetRun(r.Context(), runID); err != nil {
		s.writeRunError(w, err)
		return
	}

	var req promptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The request body is not valid JSON.", err.Error())
		return
	}
	if req.DestTargetID == "" {
		writeError(w, http.StatusBadRequest, "missing_destination",
			"Say which destination this answer is about.", "")
		return
	}
	if !runner.ValidPromptAction(req.Action) {
		writeError(w, http.StatusBadRequest, "invalid_action",
			`action must be "skip", "retry" or "abort".`, string(req.Action))
		return
	}

	switch err := s.runner.AnswerPrompt(runID, req.DestTargetID, req.Action); {
	case err == nil:
		s.log.Info("prompt answered", "run_id", runID,
			"dest_target_id", req.DestTargetID, "action", req.Action)
		writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
	case errors.Is(err, runner.ErrNotRunning):
		writeError(w, http.StatusConflict, "not_running",
			"That run has already finished.", "")
	case errors.Is(err, runner.ErrNoSuchPrompt):
		// The countdown ran out, or somebody else answered first. Not an
		// error the user did anything about.
		writeError(w, http.StatusConflict, "no_such_prompt",
			"That destination is no longer waiting for an answer.", "")
	default:
		writeError(w, http.StatusInternalServerError, "prompt_failed",
			"Could not record that answer.", err.Error())
	}
}

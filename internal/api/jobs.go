package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// jobPayload is the request body for creating a job.
type jobPayload struct {
	Name           string `json:"name"`
	SourceTargetID string `json:"source_target_id"`
	SourceSubpath  string `json:"source_subpath"`

	Mode    string `json:"mode"`
	Compare string `json:"compare"`

	CompareToleranceSec *int `json:"compare_tolerance_sec"`
	IgnoreDSTHour       bool `json:"ignore_dst_hour"`
	Workers             int  `json:"workers"`
	LogEveryFile        bool `json:"log_every_file"`

	OnError      string `json:"on_error"`
	DeletePolicy string `json:"delete_policy"`

	UnavailablePolicy    string `json:"unavailable_policy"`
	PromptTimeoutSec     int    `json:"prompt_timeout_sec"`
	PromptFallback       string `json:"prompt_fallback"`
	CreateDestDirs       string `json:"create_dest_dirs"`
	ParallelDestinations bool   `json:"parallel_destinations"`

	// ScheduleCron is a five-field cron expression, or empty for a job that
	// only runs when asked.
	//
	// Note the asymmetry with Enabled below on a PATCH, which is a full
	// replace: omitting this *clears* the schedule, while omitting Enabled
	// *turns scheduling on*. Both follow from each field's own default, but a
	// hand-written client that sends neither un-pauses a job it has just
	// unscheduled. The UI always sends both (toJobPayload).
	ScheduleCron string `json:"schedule_cron"`
	// Enabled is a pointer so an absent field means "yes".
	//
	// A plain bool would make omitting it mean *disabled*, which for a field
	// that gates whether backups happen is the wrong way round: every existing
	// client, and every hand-written curl, would silently turn scheduling off.
	// nil means the caller did not express an opinion, and the answer to that
	// is yes.
	Enabled *bool `json:"enabled"`

	Destinations []destPayload   `json:"destinations"`
	Filters      []filterPayload `json:"filters"`
}

// filterPayload is one filter rule as the API accepts it (SPEC.md §6.5).
type filterPayload struct {
	Scope         string   `json:"scope"`
	ScopeTargetID string   `json:"scope_target_id"`
	Direction     string   `json:"direction"`
	Source        string   `json:"source"`
	Patterns      []string `json:"patterns"`
	FilePath      string   `json:"file_path"`
	JSONKey       string   `json:"json_key"`
	CaseSensitive bool     `json:"case_sensitive"`
	OnError       string   `json:"on_error"`
}

type destPayload struct {
	DestTargetID string `json:"dest_target_id"`
	DestSubpath  string `json:"dest_subpath"`
}

func (p *jobPayload) toJob() *store.Job {
	job := &store.Job{
		Name:                 p.Name,
		SourceTargetID:       p.SourceTargetID,
		SourceSubpath:        p.SourceSubpath,
		Mode:                 store.SyncMode(p.Mode),
		UnavailablePolicy:    store.UnavailablePolicy(p.UnavailablePolicy),
		PromptTimeoutSec:     p.PromptTimeoutSec,
		PromptFallback:       store.PromptFallback(p.PromptFallback),
		CreateDestDirs:       store.CreateDestDirs(p.CreateDestDirs),
		ParallelDestinations: p.ParallelDestinations,
		Compare:              store.CompareMethod(p.Compare),
		IgnoreDSTHour:        p.IgnoreDSTHour,
		Workers:              p.Workers,
		LogEveryFile:         p.LogEveryFile,
		OnError:              store.ErrorPolicy(p.OnError),
		DeletePolicy:         store.DeletePolicy(p.DeletePolicy),
		ScheduleCron:         strings.TrimSpace(p.ScheduleCron),
		Enabled:              p.Enabled == nil || *p.Enabled,
	}
	if p.CompareToleranceSec != nil {
		job.CompareToleranceSec = *p.CompareToleranceSec
	}
	for _, d := range p.Destinations {
		job.Destinations = append(job.Destinations, store.JobDestination{
			DestTargetID: d.DestTargetID,
			DestSubpath:  d.DestSubpath,
		})
	}
	for _, f := range p.Filters {
		job.Filters = append(job.Filters, store.FilterRule{
			Scope:         store.FilterScope(f.Scope),
			ScopeTargetID: f.ScopeTargetID,
			Direction:     store.FilterDirection(f.Direction),
			Source:        store.FilterSource(f.Source),
			Patterns:      f.Patterns,
			FilePath:      f.FilePath,
			JSONKey:       f.JSONKey,
			CaseSensitive: f.CaseSensitive,
			OnError:       store.FilterErrorPolicy(f.OnError),
		})
	}
	return job
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var p jobPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "The request body is not valid JSON for a job.", err.Error())
		return
	}

	job := p.toJob()
	if err := s.db.CreateJob(r.Context(), job); err != nil {
		s.writeJobError(w, err)
		return
	}

	s.log.Info("job created", "job_id", job.ID, "name", job.Name, "mode", job.Mode)
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.db.ListJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", "Could not list jobs.", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.db.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeJobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Same guard as editing: a run holds the job in memory, so deleting the
	// row underneath it leaves a run writing history for a job that no longer
	// exists — and that history is about to be deleted too.
	if s.runner.ActiveForJob(id) {
		writeError(w, http.StatusConflict, "job_running",
			"This job is running. Wait for it to finish, or cancel it, before deleting.", "")
		return
	}

	runs, err := s.db.CountRuns(r.Context(), id)
	if err != nil {
		s.log.Error("could not count a job's runs before deleting it", "job_id", id, "error", err)
	}

	if err := s.db.DeleteJob(r.Context(), id); err != nil {
		s.writeJobError(w, err)
		return
	}
	s.log.Info("job deleted", "job_id", id, "runs_deleted", runs)
	w.WriteHeader(http.StatusNoContent)
}

// runRequest is the body of POST /api/jobs/{id}/run (SPEC.md §8).
type runRequest struct {
	// Preview plans the run and holds it until confirmed, without copying,
	// deleting or creating anything (SPEC.md §6.1 step 6).
	Preview bool `json:"preview"`
}

// handleRunJob starts a run and returns 202 with the run record. The run
// outlives this request.
func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.db.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeJobError(w, err)
		return
	}

	// An absent body means a plain run: the flag is optional.
	var req runRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"The request body is not valid JSON for a run.", err.Error())
		return
	}

	start := s.runner.Start
	if req.Preview {
		start = s.runner.StartPreview
	}

	run, err := start(r.Context(), job)
	if err != nil {
		if errors.Is(err, runner.ErrAlreadyRunning) {
			// "Already in progress" is misleading for the common case, which
			// is a *preview* of this job parked waiting to be confirmed:
			// nothing is progressing, and the fix is to go and answer it
			// rather than to wait.
			message := "A run of this job is already in progress."
			detail := ""
			if prev, err := s.db.ListRuns(r.Context(), store.RunFilter{JobID: job.ID, Limit: 1}); err == nil &&
				len(prev) > 0 && prev[0].Status == store.RunAwaitingConfirmation {
				message = "A preview of this job is waiting to be confirmed."
				detail = "Open it to confirm or cancel it, and this job can run again. " +
					"It cancels itself on its own deadline if nobody answers."
			}
			writeError(w, http.StatusConflict, "already_running", message, detail)
			return
		}
		s.writeJobError(w, err)
		return
	}

	s.log.Info("run started", "run_id", run.ID, "job_id", job.ID, "name", job.Name, "preview", req.Preview)
	writeJSON(w, http.StatusAccepted, run)
}

// filterTestRequest is the body of POST /api/jobs/{id}/filter-test.
type filterTestRequest struct {
	SampleLimit int `json:"sample_limit"`
}

// handleFilterTest previews what a job's filters would include and exclude,
// against a sample of the real source (SPEC.md §8).
func (s *Server) handleFilterTest(w http.ResponseWriter, r *http.Request) {
	job, err := s.db.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeJobError(w, err)
		return
	}

	// The body is optional: an empty request means "use the defaults".
	var req filterTestRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body",
				"The request body is not valid JSON for a filter test.", err.Error())
			return
		}
	}

	result, err := s.runner.FilterTest(r.Context(), job, req.SampleLimit)
	if err != nil {
		// A share being down is not a malformed request. Reporting it as 400
		// made the UI say "your request was malformed" about an unplugged NAS,
		// which sends the user to look at their rules.
		if errors.Is(err, runner.ErrSourceUnavailable) {
			writeError(w, http.StatusBadGateway, "source_unavailable", err.Error(), "")
			return
		}
		// A broken rule is the user's to fix, and its message already says
		// which rule and why.
		writeError(w, http.StatusBadRequest, "filter_test_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) writeJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such job.", err.Error())
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", err.Error(), "")
	default:
		writeError(w, http.StatusBadRequest, "invalid_job", err.Error(), "")
	}
}

// handleUpdateJob replaces a job, its destinations and its filter rules
// (SPEC.md §8's "create/update job").
//
// §8 lists only POST /api/jobs for both create and update. A PATCH on the job
// is used instead: replacing a job with nested destinations and filters is a
// materially different operation from creating one, and the UI needs to
// address an existing job by id. Recorded as an extension of §8 rather than a
// silent deviation.
func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// A job whose destinations or filters change under a live diff is not
	// something the engine is built to survive, so editing is refused while
	// it runs rather than raced. This must be keyed by job, not run: a
	// previewed run parked at awaiting_confirmation still holds a plan that
	// will execute, and editing the job does not change that held plan.
	if s.runner.ActiveForJob(id) {
		writeError(w, http.StatusConflict, "job_running",
			"This job is running. Wait for it to finish, or cancel it, before editing.", "")
		return
	}
	if runs, err := s.db.ListRuns(r.Context(), store.RunFilter{JobID: id, Status: store.RunRunning, Limit: 1}); err == nil && len(runs) > 0 {
		writeError(w, http.StatusConflict, "job_running",
			"This job is running. Wait for it to finish, or cancel it, before editing.", "")
		return
	}

	var p jobPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "The request body is not valid JSON for a job.", err.Error())
		return
	}

	job := p.toJob()
	if err := s.db.UpdateJob(r.Context(), id, job); err != nil {
		s.writeJobError(w, err)
		return
	}

	updated, err := s.db.GetJob(r.Context(), id)
	if err != nil {
		s.writeJobError(w, err)
		return
	}
	s.log.Info("job updated", "job_id", id, "name", updated.Name)
	writeJSON(w, http.StatusOK, updated)
}

// handleConfirmJob releases a previewed run so it executes the plan it is
// holding (SPEC.md §8's POST /api/jobs/{id}/confirm).
func (s *Server) handleConfirmJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if _, err := s.db.GetJob(r.Context(), jobID); err != nil {
		s.writeJobError(w, err)
		return
	}

	// Only one run of a job can be in flight, so the parked run is
	// unambiguous; asking the runner avoids racing the database row.
	runID, parked := s.runner.AwaitingConfirmation(jobID)
	if !parked {
		writeError(w, http.StatusConflict, "not_awaiting_confirmation",
			"This job has no previewed run waiting to be confirmed.", "")
		return
	}

	if err := s.runner.Confirm(runID); err != nil {
		if errors.Is(err, runner.ErrNotRunning) {
			writeError(w, http.StatusConflict, "not_running", "That run has already finished.", "")
			return
		}
		writeError(w, http.StatusInternalServerError, "confirm_failed",
			"Could not confirm that run.", err.Error())
		return
	}

	s.log.Info("previewed run confirmed", "run_id", runID, "job_id", jobID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed", "run_id": runID})
}

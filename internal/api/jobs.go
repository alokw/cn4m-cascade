package api

import (
	"errors"
	"net/http"

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
	ParallelDestinations bool   `json:"parallel_destinations"`

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
		ParallelDestinations: p.ParallelDestinations,
		Compare:              store.CompareMethod(p.Compare),
		IgnoreDSTHour:        p.IgnoreDSTHour,
		Workers:              p.Workers,
		LogEveryFile:         p.LogEveryFile,
		OnError:              store.ErrorPolicy(p.OnError),
		DeletePolicy:         store.DeletePolicy(p.DeletePolicy),
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
	if err := s.db.DeleteJob(r.Context(), r.PathValue("id")); err != nil {
		s.writeJobError(w, err)
		return
	}
	s.log.Info("job deleted", "job_id", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// handleRunJob starts a run and returns 202 with the run record. The run
// outlives this request.
func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.db.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeJobError(w, err)
		return
	}

	run, err := s.runner.Start(r.Context(), job)
	if err != nil {
		if errors.Is(err, runner.ErrAlreadyRunning) {
			writeError(w, http.StatusConflict, "already_running",
				"A run of this job is already in progress.", "")
			return
		}
		s.writeJobError(w, err)
		return
	}

	s.log.Info("run started", "run_id", run.ID, "job_id", job.ID, "name", job.Name)
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

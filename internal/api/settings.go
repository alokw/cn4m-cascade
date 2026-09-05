package api

import (
	"fmt"
	"net/http"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// globalFilterPayload is one global exclusion as the API accepts it. Server-
// owned fields (id, position, timestamps) are absent: position comes from array
// order, exactly as it does for a job's rules.
type globalFilterPayload struct {
	Source        string   `json:"source"`
	Patterns      []string `json:"patterns"`
	FilePath      string   `json:"file_path"`
	JSONKey       string   `json:"json_key"`
	CaseSensitive bool     `json:"case_sensitive"`
	OnError       string   `json:"on_error"`
}

type globalFiltersRequest struct {
	Filters []globalFilterPayload `json:"filters"`
}

// handleGetGlobalFilters returns the exclusions applied to every job.
func (s *Server) handleGetGlobalFilters(w http.ResponseWriter, r *http.Request) {
	rules, err := s.db.ListGlobalFilterRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed",
			"Could not read the global filters.", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"filters": rules})
}

// handleReplaceGlobalFilters swaps the whole set.
//
// Wholesale, like a job's rules: they are positional, and reordering the array
// is how evaluation order is changed. The transaction matters more here than
// for one job, because a half-applied list would apply to every job at once.
func (s *Server) handleReplaceGlobalFilters(w http.ResponseWriter, r *http.Request) {
	var req globalFiltersRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"The request body is not valid JSON for global filters.", err.Error())
		return
	}

	rules := make([]store.GlobalFilterRule, 0, len(req.Filters))
	for _, f := range req.Filters {
		rules = append(rules, store.GlobalFilterRule{
			Source:        store.FilterSource(f.Source),
			Patterns:      f.Patterns,
			FilePath:      f.FilePath,
			JSONKey:       f.JSONKey,
			CaseSensitive: f.CaseSensitive,
			OnError:       store.FilterErrorPolicy(f.OnError),
		})
	}

	// Validate before touching the store, so a rejected rule is a 400 with a
	// message naming it, and anything the store then reports is genuinely a
	// store failure — not the caller's payload. Reporting SQLITE_BUSY as
	// "invalid_filter" tells the client its request was wrong when it was not,
	// and hands it a Go error string to read.
	for i := range rules {
		rules[i].ApplyDefaults()
		if err := rules[i].Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_filter",
				fmt.Sprintf("global filter rule %d: %v", i+1, err), "")
			return
		}
	}

	if err := s.db.ReplaceGlobalFilterRules(r.Context(), rules); err != nil {
		s.log.Error("could not replace the global filters", "error", err)
		writeError(w, http.StatusInternalServerError, "save_failed",
			"Could not save the global filters.", err.Error())
		return
	}

	s.log.Info("global filters replaced", "rules", len(rules))
	s.handleGetGlobalFilters(w, r)
}

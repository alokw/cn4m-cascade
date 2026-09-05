package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// filterCheckTimeout bounds the stat. This answers a form field, so it fails
// fast rather than making someone watch a spinner.
const filterCheckTimeout = 5 * time.Second

type filterCheckRequest struct {
	FilePath string `json:"file_path"`
}

type filterCheckResponse struct {
	// Checked is false when the path could not be examined at all, as opposed
	// to being examined and found missing. The UI must not report "this file
	// does not exist" about a path it never looked at.
	Checked bool   `json:"checked"`
	Exists  bool   `json:"exists"`
	Message string `json:"message,omitempty"`
}

// handleCheckFilterFile reports whether a rule's list/JSON file is there yet.
//
// Advisory only: it never blocks a save. A rule file is live configuration
// read at the start of every run (SPEC.md §6.5), so configuring a job before
// the file exists is legitimate — the run will fail per the rule's on_error if
// it is still missing then. What this prevents is the silent case, where a
// typo sits unnoticed until a run fails hours later.
func (s *Server) handleCheckFilterFile(w http.ResponseWriter, r *http.Request) {
	var req filterCheckRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"The request body is not valid JSON.", err.Error())
		return
	}

	path := strings.TrimSpace(req.FilePath)
	if path == "" {
		writeJSON(w, http.StatusOK, filterCheckResponse{Checked: false, Message: "No path given."})
		return
	}

	// A target:// reference lives on a share, and mounting one to answer a
	// form-field question is far too expensive — a mount can take seconds and
	// wakes a sleeping NAS. Say plainly that it was not checked.
	if strings.HasPrefix(path, store.TargetRefPrefix) {
		if _, _, ok := store.ParseTargetRef(path); !ok {
			writeJSON(w, http.StatusOK, filterCheckResponse{
				Checked: false,
				Message: "That does not look like a valid target:// reference.",
			})
			return
		}
		writeJSON(w, http.StatusOK, filterCheckResponse{
			Checked: false,
			Message: "On a share — not checked here; it is read when the job runs.",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), filterCheckTimeout)
	defer cancel()

	// Stat, not read: a rule file can be large, and this only needs to know
	// whether it is there. Bounded because the path may be a bind mount
	// backed by something slow.
	if _, err := engine.StatBounded(ctx, filterCheckTimeout, path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, http.StatusOK, filterCheckResponse{
				Checked: true, Exists: false,
				Message: "No file at that path yet.",
			})
			return
		}
		// Not "checked: true, exists: false". A timeout, a permission error or
		// a slow bind mount means the path was never determined either way,
		// and claiming it is missing would be asserting something we do not
		// know — the exact distinction this response type exists to make.
		writeJSON(w, http.StatusOK, filterCheckResponse{
			Checked: false,
			Message: "Could not check that path: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, filterCheckResponse{Checked: true, Exists: true})
}

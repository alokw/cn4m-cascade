package api

import (
	"net/http"
	"path"
	"sort"
	"time"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// browseTimeout bounds a single directory listing. A path picker is
// interactive, so it fails fast rather than making the user watch a spinner
// while a dead share times out at the mount layer.
const browseTimeout = 15 * time.Second

// browseEntry is one row in a path picker.
type browseEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

type browseResponse struct {
	TargetID string        `json:"target_id"`
	Path     string        `json:"path"`
	Parent   string        `json:"parent,omitempty"`
	Entries  []browseEntry `json:"entries"`
}

// handleBrowse lists one directory of a target, for the UI's path pickers
// (SPEC.md §8's /api/browse).
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	targetID := r.URL.Query().Get("target_id")
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "missing_target", "A target_id is required.", "")
		return
	}
	sub := r.URL.Query().Get("path")

	// The same validation job subpaths get: no absolutes, no "..", nothing
	// that escapes the share root.
	if err := store.ValidateSubpath("path", sub); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error(), "")
		return
	}

	target, err := s.db.GetTarget(r.Context(), targetID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// The browse path is applied to a copy, so listing never mutates the
	// stored target — the same trick the runner uses for job subpaths.
	scoped := *target
	scoped.Subpath = joinSubpaths(target.Subpath, sub)

	st, err := s.provider.For(&scoped)
	if err != nil {
		writeMountError(w, err)
		return
	}
	root, err := st.Resolve(r.Context())
	if err != nil {
		writeMountError(w, err)
		return
	}
	defer func() { _ = st.Release(r.Context()) }()

	// A share path: it must never be listed with a bare os.ReadDir, because
	// a dead server would park this request in the kernel indefinitely.
	entries, err := engine.ReadDirBounded(r.Context(), browseTimeout, root)
	if err != nil {
		writeError(w, http.StatusBadGateway, "listing_failed",
			"Could not list that directory.", err.Error())
		return
	}

	resp := browseResponse{TargetID: targetID, Path: sub, Entries: []browseEntry{}}
	if sub != "" {
		resp.Parent = path.Dir(sub)
		if resp.Parent == "." {
			resp.Parent = ""
		}
	}

	for _, e := range entries {
		row := browseEntry{Name: e.Name(), Path: path.Join(sub, e.Name()), IsDir: e.IsDir()}
		// Size is a nicety, and stat-ing every entry over SMB is the
		// expensive part of a listing. A file whose details cannot be read
		// still belongs in the list.
		if !e.IsDir() {
			// Info() is a syscall on anything the listing did not cache, so
			// on a share it gets the same bound as the listing itself.
			if info, err := engine.InfoBounded(r.Context(), browseTimeout, e, row.Path); err == nil {
				row.Size = info.Size()
			}
		}
		resp.Entries = append(resp.Entries, row)
	}

	// Directories first, then names: what a file picker is expected to do.
	sort.Slice(resp.Entries, func(i, j int) bool {
		if resp.Entries[i].IsDir != resp.Entries[j].IsDir {
			return resp.Entries[i].IsDir
		}
		return resp.Entries[i].Name < resp.Entries[j].Name
	})

	writeJSON(w, http.StatusOK, resp)
}

// joinSubpaths combines a target's own subpath with a browse path. Both are
// already validated as relative and free of "..", so the join cannot escape.
func joinSubpaths(base, sub string) string {
	switch {
	case base == "":
		return sub
	case sub == "":
		return base
	default:
		return path.Join(base, sub)
	}
}

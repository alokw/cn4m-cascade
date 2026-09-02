package api

import (
	"context"
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

// browseBudget bounds the *whole* listing, not one call within it.
//
// Every InfoBounded below is individually bounded, which satisfies the hard
// rule but not the arithmetic: a directory with 200k entries on a share that
// dies mid-listing is 200k sequential stats, each waiting out browseTimeout
// and each holding an OS thread parked in the kernel. The per-call bound makes
// no single call hang; only this one stops the sum from being days.
const browseBudget = 30 * time.Second

// browseLimit caps how many entries one listing returns. A path picker is for
// choosing a directory, not for reading a million-entry share root into a
// browser. The response says when it truncated so the UI can tell the user to
// narrow the path rather than silently showing a partial listing as complete.
const browseLimit = 2000

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
	// Truncated reports that the directory held more than browseLimit
	// entries and only the first are listed.
	Truncated bool `json:"truncated"`
	// Total is how many entries the directory actually holds.
	Total int `json:"total"`
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

	ctx, cancel := context.WithTimeout(r.Context(), browseBudget)
	defer cancel()

	target, err := s.db.GetTarget(ctx, targetID)
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
	root, err := st.Resolve(ctx)
	if err != nil {
		writeMountError(w, err)
		return
	}
	// Release with the request context, not the budgeted one: dropping a
	// mount reference must still happen when the budget has already expired.
	defer func() { _ = st.Release(context.WithoutCancel(r.Context())) }()

	// A share path: it must never be listed with a bare os.ReadDir, because
	// a dead server would park this request in the kernel indefinitely.
	entries, err := engine.ReadDirBounded(ctx, browseTimeout, root)
	if err != nil {
		writeError(w, http.StatusBadGateway, "listing_failed",
			"Could not list that directory.", err.Error())
		return
	}

	resp := browseResponse{TargetID: targetID, Path: sub, Entries: []browseEntry{}, Total: len(entries)}

	// Sort BEFORE truncating, not after. ReadDir returns entries in name
	// order, so slicing first would keep the alphabetically-first 2000 names
	// and drop every directory that sorts after them — a picker that lists
	// 2000 files and no directories, in the one directory the user was trying
	// to navigate out of. Ordering directories first and then capping keeps
	// navigation possible however many files are alongside them.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	if len(entries) > browseLimit {
		entries = entries[:browseLimit]
		resp.Truncated = true
	}
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
		//
		// The ctx check is load-bearing, not decorative: engine.bounded
		// launches its goroutine and *then* selects, with no pre-check, so an
		// expired context does not stop it starting another syscall. Without
		// this, a share that dies mid-listing would fire one lstat goroutine
		// per remaining entry in a burst — up to browseLimit threads parked in
		// the kernel per request, and Go's hard limit is 10,000. Sizes are
		// dropped rather than the rows: a listing without sizes is still a
		// usable picker.
		if !e.IsDir() && ctx.Err() == nil {
			if info, err := engine.InfoBounded(ctx, browseTimeout, e, row.Path); err == nil {
				row.Size = info.Size()
			}
		}
		resp.Entries = append(resp.Entries, row)
	}

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

package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// mkdirBudget bounds resolving the target and then creating the folder. Both
// halves can land on a share, so the whole request is capped rather than only
// the mkdir within it — the same arithmetic browseBudget exists for.
const mkdirBudget = 45 * time.Second

// mkdirTimeout bounds the mkdir itself. A mkdir on a share is a metadata round
// trip like any other and must not hang.
const mkdirTimeout = 30 * time.Second

type mkdirRequest struct {
	Path string `json:"path"`
}

type mkdirResponse struct {
	TargetID string `json:"target_id"`
	Path     string `json:"path"`
	// Created distinguishes "made it" from "it was already there", so the UI
	// can say which happened rather than claiming to have created a folder
	// somebody else made in the meantime.
	Created bool `json:"created"`
}

// handleMakeDir creates one folder beneath a target, for the job editor's
// offer to make a missing source folder.
//
// It deliberately creates only the *subpath*: the target is resolved at its
// own root first, so a share that is down or a bind mount that is wrong still
// fails here. That mirrors storage.CreateSubpath — the target existing is what
// proves it is configured correctly at all, and this endpoint must not be a
// way to paper over a broken one.
func (s *Server) handleMakeDir(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")

	var req mkdirRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Could not read that request.", err.Error())
		return
	}
	sub := strings.TrimSpace(req.Path)
	if sub == "" {
		writeError(w, http.StatusBadRequest, "invalid_path",
			"A path is required — the target root already exists.", "")
		return
	}
	// The same validation job subpaths get: no absolutes, no "..", nothing
	// that escapes the target root.
	if err := store.ValidateSubpath("path", sub); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error(), "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mkdirBudget)
	defer cancel()

	target, err := s.db.GetTarget(ctx, targetID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	st, err := s.provider.For(target)
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

	// sub is validated as relative and free of "..", so this cannot escape
	// root. Cleaning through a leading slash is how storage joins a subpath.
	full := filepath.Join(root, filepath.Clean("/"+filepath.FromSlash(sub)))

	if _, err := engine.StatBounded(ctx, mkdirTimeout, full); err == nil {
		writeJSON(w, http.StatusOK, mkdirResponse{TargetID: targetID, Path: sub, Created: false})
		return
	}

	if err := engine.MkdirAllBounded(ctx, mkdirTimeout, full); err != nil {
		writeError(w, http.StatusBadGateway, "mkdir_failed",
			"Could not create that folder.", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mkdirResponse{TargetID: targetID, Path: sub, Created: true})
}

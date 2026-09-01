package api

import (
	"errors"
	"net/http"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/mountmgr"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// targetPayload is the request body for create and update. Every field is a
// pointer so that PATCH can distinguish "absent" from "set to empty" —
// notably, omitting password leaves the stored one untouched.
type targetPayload struct {
	Name              *string `json:"name"`
	Type              *string `json:"type"`
	Host              *string `json:"host"`
	Share             *string `json:"share"`
	Subpath           *string `json:"subpath"`
	LocalPath         *string `json:"local_path"`
	Port              *int    `json:"port"`
	Username          *string `json:"username"`
	Password          *string `json:"password"`
	Domain            *string `json:"domain"`
	MountOptsOverride *string `json:"mount_opts_override"`
	Multichannel      *bool   `json:"multichannel"`
}

// targetResponse is a target as the API returns it. It never carries the
// password, encrypted or otherwise.
type targetResponse struct {
	*store.Target
	HasPassword bool          `json:"has_password"`
	Health      health.Status `json:"health"`
}

func (s *Server) present(t *store.Target) targetResponse {
	return targetResponse{
		Target:      t,
		HasPassword: t.PasswordEncrypted != "",
		Health:      s.healthc.Get(t.ID),
	}
}

// apply copies the payload onto a target, encrypting the password if one was
// supplied.
func (s *Server) apply(p *targetPayload, t *store.Target) error {
	if p.Name != nil {
		t.Name = *p.Name
	}
	if p.Type != nil {
		t.Type = store.TargetType(*p.Type)
	}
	if p.Host != nil {
		t.Host = *p.Host
	}
	if p.Share != nil {
		t.Share = *p.Share
	}
	if p.Subpath != nil {
		t.Subpath = *p.Subpath
	}
	if p.LocalPath != nil {
		t.LocalPath = *p.LocalPath
	}
	if p.Port != nil {
		t.Port = *p.Port
	}
	if p.Username != nil {
		t.Username = *p.Username
	}
	if p.Domain != nil {
		t.Domain = *p.Domain
	}
	if p.MountOptsOverride != nil {
		t.MountOptsOverride = *p.MountOptsOverride
	}
	if p.Multichannel != nil {
		t.Multichannel = *p.Multichannel
	}
	if p.Password != nil {
		enc, err := s.box.Encrypt(*p.Password)
		if err != nil {
			return err
		}
		t.PasswordEncrypted = enc
	}
	return nil
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var p targetPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "The request body is not valid JSON for a target.", err.Error())
		return
	}
	if p.Type == nil {
		writeError(w, http.StatusBadRequest, "invalid_target", `type is required: "smb" or "local"`, "")
		return
	}

	t := &store.Target{}
	if err := s.apply(&p, t); err != nil {
		writeError(w, http.StatusInternalServerError, "encrypt_failed", "Could not encrypt the credentials.", err.Error())
		return
	}

	if err := s.db.CreateTarget(r.Context(), t); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.log.Info("target created", "target_id", t.ID, "name", t.Name, "type", t.Type)
	writeJSON(w, http.StatusCreated, s.present(t))
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.db.ListTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", "Could not list targets.", err.Error())
		return
	}
	out := make([]targetResponse, 0, len(targets))
	for _, t := range targets {
		out = append(out, s.present(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

func (s *Server) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.GetTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.present(t))
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.GetTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	var p targetPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "The request body is not valid JSON for a target.", err.Error())
		return
	}
	if err := s.apply(&p, t); err != nil {
		writeError(w, http.StatusInternalServerError, "encrypt_failed", "Could not encrypt the credentials.", err.Error())
		return
	}
	if err := s.db.UpdateTarget(r.Context(), t); err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Connection settings may have changed, so the existing mount is no
	// longer trustworthy. Drop it if nothing is using it; a busy target
	// keeps its current mount until the jobs holding it finish.
	if err := s.mounts.Unmount(r.Context(), t.ID); err != nil && !errors.Is(err, mountmgr.ErrInUse) {
		s.log.Warn("could not unmount after a target change", "target_id", t.ID, "error", err)
	}

	s.log.Info("target updated", "target_id", t.ID, "name", t.Name)
	writeJSON(w, http.StatusOK, s.present(t))
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.GetTarget(r.Context(), id); err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Unmount first: deleting the row while a mount is live would orphan it.
	if err := s.mounts.Unmount(r.Context(), id); err != nil {
		if errors.Is(err, mountmgr.ErrInUse) {
			writeError(w, http.StatusConflict, "target_in_use",
				"This target is in use and cannot be deleted right now.", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "unmount_failed",
			"Could not unmount the target before deleting it.", err.Error())
		return
	}

	if err := s.db.DeleteTarget(r.Context(), id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.mounts.Forget(id)
	s.log.Info("target deleted", "target_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestTarget mounts the target, proves it responds, and lists its root
// (SPEC.md §8). Failures come back with the mount error classification.
func (s *Server) handleTestTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.GetTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	result, err := s.provider.Test(r.Context(), t)
	if err != nil {
		s.log.Warn("target test failed", "target_id", t.ID, "name", t.Name, "error", err)
		writeMountError(w, err)
		return
	}
	s.log.Info("target test succeeded",
		"target_id", t.ID, "name", t.Name, "vers", result.NegotiatedVers, "elapsed_ms", result.ElapsedMS)
	writeJSON(w, http.StatusOK, result)
}

// writeStoreError maps store errors onto status codes. Anything unrecognised
// is a validation failure: Validate returns plain errors by design, and its
// messages are already written for users.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such target.", err.Error())
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", err.Error(), "")
	default:
		writeError(w, http.StatusBadRequest, "invalid_target", err.Error(), "")
	}
}

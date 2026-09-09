package api

import (
	"errors"
	"net/http"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// webhookPayload is a callback as the API accepts it.
type webhookPayload struct {
	JobID  string   `json:"job_id"`
	URL    string   `json:"url"`
	Events []string `json:"events"`
	// Secret is write-only. An empty value on update means "leave it alone",
	// so the UI can save a hook whose secret it was never given — the same
	// arrangement target passwords use, and for the same reason: a form that
	// must round-trip a secret in order to save unrelated fields is a form
	// that leaks it.
	Secret  string `json:"secret"`
	Enabled *bool  `json:"enabled"`
	// Format is "json" or "cn4m"; empty defaults to json.
	Format         string `json:"format"`
	MinIntervalSec int    `json:"min_interval_sec"`
}

// webhookResponse never carries the secret, encrypted or otherwise.
type webhookResponse struct {
	*store.Webhook
	HasSecret bool `json:"has_secret"`
}

func present(w *store.Webhook) webhookResponse {
	return webhookResponse{Webhook: w, HasSecret: w.SecretEncrypted != ""}
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.db.ListWebhooks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", "Could not list the callbacks.", err.Error())
		return
	}
	out := make([]webhookResponse, 0, len(hooks))
	for i := range hooks {
		out = append(out, present(&hooks[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var p webhookPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "The request body is not valid JSON for a callback.", err.Error())
		return
	}

	hook, err := s.webhookFrom(&p, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error(), "")
		return
	}
	if err := s.db.CreateWebhook(r.Context(), hook); err != nil {
		s.writeWebhookError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, present(hook))
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	var p webhookPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "The request body is not valid JSON for a callback.", err.Error())
		return
	}

	hook, err := s.webhookFrom(&p, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error(), "")
		return
	}
	if err := s.db.UpdateWebhook(r.Context(), r.PathValue("id"), hook); err != nil {
		s.writeWebhookError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, present(hook))
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DeleteWebhook(r.Context(), r.PathValue("id")); err != nil {
		s.writeWebhookError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// webhookFrom maps a payload onto a store record, encrypting the secret.
func (s *Server) webhookFrom(p *webhookPayload, id string) (*store.Webhook, error) {
	hook := &store.Webhook{
		ID:             id,
		JobID:          p.JobID,
		URL:            p.URL,
		Events:         p.Events,
		Enabled:        p.Enabled == nil || *p.Enabled,
		Format:         p.Format,
		MinIntervalSec: p.MinIntervalSec,
	}
	if p.Secret != "" {
		encrypted, err := s.box.Encrypt(p.Secret)
		if err != nil {
			return nil, errors.New("the signing secret could not be stored")
		}
		hook.SecretEncrypted = encrypted
	}
	return hook, nil
}

func (s *Server) writeWebhookError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No such callback.", "")
		return
	}
	writeError(w, http.StatusBadRequest, "webhook_failed", err.Error(), "")
}

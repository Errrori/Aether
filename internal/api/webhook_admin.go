package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/aether-mq/aether/internal/store"
	"github.com/aether-mq/aether/internal/webhook"
)

type createWebhookRequest struct {
	Name            string `json:"name"`
	ChannelTemplate string `json:"channel_template"`
	KeyID           string `json:"key_id"`
}

type webhookListResponse struct {
	OK       bool                   `json:"ok"`
	Webhooks []webhook.WebhookMeta  `json:"webhooks"`
}

type webhookGetResponse struct {
	OK      bool                 `json:"ok"`
	Webhook *webhook.WebhookMeta `json:"webhook,omitempty"`
}

type webhookCreateResponse struct {
	OK      bool                `json:"ok"`
	Webhook webhook.WebhookMeta `json:"webhook"`
	Secret  string              `json:"secret"`
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.MaxPayloadSize+1)))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "failed to read request body")
		return
	}
	if len(body) > s.cfg.MaxPayloadSize {
		writeError(w, http.StatusRequestEntityTooLarge, ErrCodePayloadTooLarge, "payload exceeds maximum size")
		return
	}

	var req createWebhookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid json")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, ErrCodeMissingField, "name is required")
		return
	}
	if req.ChannelTemplate == "" {
		writeError(w, http.StatusBadRequest, ErrCodeMissingField, "channel_template is required")
		return
	}

	result, err := s.webhookManager.CreateWebhook(r.Context(), req.Name, req.ChannelTemplate, req.KeyID)
	if err != nil {
		if errors.Is(err, store.ErrWebhookNameConflict) {
			writeError(w, http.StatusConflict, ErrCodeWebhookNameConflict, "webhook name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to create webhook")
		return
	}

	writeJSON(w, http.StatusOK, webhookCreateResponse{
		OK:      true,
		Webhook: result.Webhook,
		Secret:  result.Secret,
	})
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	webhooks, err := s.webhookManager.ListWebhooks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to list webhooks")
		return
	}
	if webhooks == nil {
		webhooks = []webhook.WebhookMeta{}
	}
	writeJSON(w, http.StatusOK, webhookListResponse{OK: true, Webhooks: webhooks})
}

func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wh, err := s.webhookManager.GetWebhook(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrWebhookNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeWebhookNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to get webhook")
		return
	}
	writeJSON(w, http.StatusOK, webhookGetResponse{OK: true, Webhook: wh})
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.webhookManager.DeleteWebhook(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrWebhookNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeWebhookNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to delete webhook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	result, err := s.webhookManager.RotateSecret(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrWebhookNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeWebhookNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to rotate webhook secret")
		return
	}

	writeJSON(w, http.StatusOK, webhookCreateResponse{
		OK:      true,
		Webhook: result.Webhook,
		Secret:  result.Secret,
	})
}

package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/aether-mq/aether/internal/webhook"
)

type webhookReceiveResponse struct {
	OK        bool   `json:"ok"`
	SeqID     int64  `json:"seq_id"`
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
}

func (s *Server) handleWebhookReceive(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("id")

	if r.ContentLength > int64(s.cfg.MaxPayloadSize) {
		writeError(w, http.StatusRequestEntityTooLarge, ErrCodePayloadTooLarge, "payload exceeds maximum size")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.MaxPayloadSize+1)))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "failed to read request body")
		return
	}
	if len(body) > s.cfg.MaxPayloadSize {
		writeError(w, http.StatusRequestEntityTooLarge, ErrCodePayloadTooLarge, "payload exceeds maximum size")
		return
	}

	signature := r.Header.Get("X-Signature-256")

	result, err := s.webhookManager.ReceiveWebhook(r.Context(), token, body, signature)
	if err != nil {
		switch {
		case errors.Is(err, webhook.ErrWebhookNotFound):
			writeError(w, http.StatusNotFound, ErrCodeWebhookNotFound, "webhook not found")
		case errors.Is(err, webhook.ErrWebhookInactive):
			writeError(w, http.StatusNotFound, ErrCodeWebhookNotFound, "webhook is inactive")
		case errors.Is(err, webhook.ErrWebhookInvalidSig):
			writeError(w, http.StatusUnauthorized, ErrCodeWebhookInvalidSig, "invalid signature")
		case errors.Is(err, webhook.ErrWebhookChannelFail):
			writeError(w, http.StatusBadRequest, ErrCodeWebhookChannelFailed, "failed to resolve channel")
		default:
			writeError(w, http.StatusInternalServerError, ErrCodeInternal, "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, webhookReceiveResponse{
		OK:        true,
		SeqID:     result.SeqID,
		Channel:   result.Channel,
		Timestamp: result.Timestamp.Format(time.RFC3339),
	})
}

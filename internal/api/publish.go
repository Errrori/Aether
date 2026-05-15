package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/aether-mq/aether/internal/store"
)

type publishRequest struct {
	Channel        string `json:"channel"`
	Payload        any    `json:"payload"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type publishResponse struct {
	OK    bool   `json:"ok"`
	SeqID int64  `json:"seq_id"`
	Timestamp string `json:"timestamp"`
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
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

	var req publishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid json")
		return
	}

	if req.Channel == "" {
		writeError(w, http.StatusBadRequest, ErrCodeMissingField, "channel is required")
		return
	}
	if req.Payload == nil {
		writeError(w, http.StatusBadRequest, ErrCodeMissingField, "payload is required")
		return
	}

	if err := store.ValidateChannelName(req.Channel); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidChannel, err.Error())
		return
	}

	payloadBytes, err := json.Marshal(req.Payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "failed to marshal payload")
		return
	}

	var idempotencyKey *string
	if req.IdempotencyKey != "" {
		idempotencyKey = &req.IdempotencyKey
	}

	seqID, timestamp, err := s.hub.Publish(r.Context(), req.Channel, payloadBytes, idempotencyKey)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeStorageFailure, "failed to publish message")
		return
	}

	writeJSON(w, http.StatusOK, publishResponse{
		OK:        true,
		SeqID:     seqID,
		Timestamp: timestamp.Format("2006-01-02T15:04:05Z07:00"),
	})
}

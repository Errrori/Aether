package api

import (
	"context"
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

// --- Batch Publish ---

type batchPublishRequest struct {
	Messages []batchPublishMessage `json:"messages"`
}

type batchPublishMessage struct {
	Channel        string `json:"channel"`
	Payload        any    `json:"payload"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type batchPublishResponse struct {
	OK      bool                  `json:"ok"`
	Results []batchMessageResult `json:"results"`
}

type batchMessageResult struct {
	Index     int    `json:"index"`
	Status    string `json:"status"` // "success" | "error"
	SeqID     int64  `json:"seq_id,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Error     *batchMessageError `json:"error,omitempty"`
}

type batchMessageError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const maxBatchMessages = 100

func (s *Server) handleBatchPublish(w http.ResponseWriter, r *http.Request) {
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

	var req batchPublishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid json")
		return
	}

	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, ErrCodeBatchPublishEmpty, "messages array must not be empty")
		return
	}
	if len(req.Messages) > maxBatchMessages {
		writeError(w, http.StatusBadRequest, ErrCodeMissingField,
			"too many messages in batch (max 100)")
		return
	}

	results := make([]batchMessageResult, len(req.Messages))
	for i, msg := range req.Messages {
		result := batchMessageResult{Index: i}
		s.processBatchMessage(r.Context(), &msg, &result)
		results[i] = result
	}

	writeJSON(w, http.StatusOK, batchPublishResponse{OK: true, Results: results})
}

func (s *Server) processBatchMessage(ctx context.Context, msg *batchPublishMessage, result *batchMessageResult) {
	if msg.Channel == "" {
		result.Status = "error"
		result.Error = &batchMessageError{
			Code:    ErrCodeMissingField,
			Message: "channel is required",
		}
		return
	}
	if msg.Payload == nil {
		result.Status = "error"
		result.Error = &batchMessageError{
			Code:    ErrCodeMissingField,
			Message: "payload is required",
		}
		return
	}

	if err := store.ValidateChannelName(msg.Channel); err != nil {
		result.Status = "error"
		result.Error = &batchMessageError{
			Code:    ErrCodeInvalidChannel,
			Message: err.Error(),
		}
		return
	}

	payloadBytes, err := json.Marshal(msg.Payload)
	if err != nil {
		result.Status = "error"
		result.Error = &batchMessageError{
			Code:    ErrCodeInvalidJSON,
			Message: "failed to marshal payload",
		}
		return
	}

	var idempotencyKey *string
	if msg.IdempotencyKey != "" {
		idempotencyKey = &msg.IdempotencyKey
	}

	seqID, timestamp, err := s.hub.Publish(ctx, msg.Channel, payloadBytes, idempotencyKey)
	if err != nil {
		result.Status = "error"
		result.Error = &batchMessageError{
			Code:    ErrCodeStorageFailure,
			Message: "failed to publish message",
		}
		return
	}

	result.Status = "success"
	result.SeqID = seqID
	result.Timestamp = timestamp.Format("2006-01-02T15:04:05Z07:00")
}

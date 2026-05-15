package api

import (
	"net/http"
	"strconv"

	"github.com/aether-mq/aether/internal/store"
)

type historyResponse struct {
	OK       bool            `json:"ok"`
	Channel  string          `json:"channel"`
	Messages []store.Message `json:"messages"`
	HasMore  bool            `json:"has_more"`
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		writeError(w, http.StatusBadRequest, ErrCodeMissingField, "channel is required")
		return
	}

	if err := store.ValidateChannelName(channel); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidChannel, err.Error())
		return
	}

	afterSeqStr := r.URL.Query().Get("after_seq")
	var afterSeq int64
	if afterSeqStr != "" {
		var err error
		afterSeq, err = strconv.ParseInt(afterSeqStr, 10, 64)
		if err != nil || afterSeq < 0 {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "after_seq must be a non-negative integer")
			return
		}
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit < 1 {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "limit must be a positive integer")
			return
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	result, err := s.store.ReadHistory(r.Context(), channel, afterSeq, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeStorageFailure, "failed to read history")
		return
	}

	messages := result.Messages
	if messages == nil {
		messages = []store.Message{}
	}
	hasMore := len(messages) >= limit

	writeJSON(w, http.StatusOK, historyResponse{
		OK:       true,
		Channel:  channel,
		Messages: messages,
		HasMore:  hasMore,
	})
}

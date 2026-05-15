package api

import (
	"encoding/json"
	"net/http"
)

// Error codes per SPEC 4.4.
const (
	ErrCodeInvalidChannel  = 40001
	ErrCodeMissingField    = 40002
	ErrCodeInvalidJSON     = 40003
	ErrCodeUnknownFrame    = 40004
	ErrCodeInvalidAPIKey   = 40101
	ErrCodeInvalidToken    = 40102
	ErrCodeUnauthorizedChannel = 40301
	ErrCodePayloadTooLarge = 41301
	ErrCodeRateLimited     = 42901 // v2 预留
	ErrCodeStorageFailure  = 50301
)

type errorResponse struct {
	OK    bool        `json:"ok"`
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code int, message string) {
	writeJSON(w, status, errorResponse{
		OK: false,
		Error: errorDetail{
			Code:    code,
			Message: message,
		},
	})
}

package api

import (
	"net/http"
	"strings"
)

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := extractBearerToken(r)
		if key == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidAPIKey, "invalid or missing api key")
			return
		}
		result, err := s.auth.ValidateAPIKey(r.Context(), key)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternal, "auth service error")
			return
		}
		if !result.Valid {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidAPIKey, "invalid or missing api key")
			return
		}
		next(w, r)
	}
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return auth[7:]
}

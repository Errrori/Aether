package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/aether-mq/aether/internal/auth"
)

// keyResultCtxKey carries the validated API key identity from the auth
// middleware to handlers (used by the optional rate limiter).
type keyResultCtxKey struct{}

func keyResultFromContext(ctx context.Context) (auth.KeyValidationResult, bool) {
	result, ok := ctx.Value(keyResultCtxKey{}).(auth.KeyValidationResult)
	return result, ok
}

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
		ctx := context.WithValue(r.Context(), keyResultCtxKey{}, result)
		next(w, r.WithContext(ctx))
	}
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return auth[7:]
}

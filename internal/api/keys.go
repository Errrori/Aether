package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

type createKeyRequest struct {
	Name       string   `json:"name"`
	Publish    []string `json:"publish"`
	Subscribe  []string `json:"subscribe"`
	Admin      bool     `json:"admin"`
	ExpiresIn  string   `json:"expires_in"`
}

type keyListResponse struct {
	OK   bool             `json:"ok"`
	Keys []json.RawMessage `json:"keys"`
}

type keyGetResponse struct {
	OK   bool             `json:"ok"`
	Meta *json.RawMessage `json:"meta,omitempty"`
}

type keyCreateResponse struct {
	OK   bool             `json:"ok"`
	Key  string           `json:"key"`
	Meta json.RawMessage  `json:"meta"`
}

// adminMiddleware validates the API key and checks for admin permissions.
func (s *Server) adminMiddleware(next http.HandlerFunc) http.HandlerFunc {
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
		if !result.Permissions.Admin {
			writeError(w, http.StatusForbidden, ErrCodeNotAdmin, "admin access required")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.MaxPayloadSize)))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "failed to read request body")
		return
	}

	var req createKeyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid json")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, ErrCodeMissingField, "name is required")
		return
	}

	var expiresIn *time.Duration
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid expires_in format")
			return
		}
		expiresIn = &d
	}

	perms := store.KeyPermissions{
		Publish:   req.Publish,
		Subscribe: req.Subscribe,
		Admin:     req.Admin,
	}

	result, err := s.keyManager.CreateKey(r.Context(), req.Name, perms, expiresIn)
	if err != nil {
		if errors.Is(err, store.ErrAPIKeyDuplicateName) {
			writeError(w, http.StatusConflict, ErrCodeKeyNameConflict, "key name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to create key")
		return
	}

	meta, _ := json.Marshal(result.Meta)
	writeJSON(w, http.StatusOK, keyCreateResponse{OK: true, Key: result.Key, Meta: meta})
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.keyManager.ListKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to list keys")
		return
	}

	raw := make([]json.RawMessage, len(keys))
	for i := range keys {
		raw[i], _ = json.Marshal(keys[i])
	}
	if raw == nil {
		raw = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, keyListResponse{OK: true, Keys: raw})
}

func (s *Server) handleGetKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, err := s.keyManager.GetKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrAPIKeyNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeKeyNotFound, "key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to get key")
		return
	}

	raw, _ := json.Marshal(meta)
	writeJSON(w, http.StatusOK, keyGetResponse{OK: true, Meta: (*json.RawMessage)(&raw)})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Invalidate cache before the store changes.
	if oldKey, err := s.keyStore.GetAPIKey(r.Context(), id); err == nil {
		s.auth.InvalidateCache(oldKey.KeyHash)
	}

	if err := s.keyManager.RevokeKey(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrAPIKeyNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeKeyNotFound, "key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to revoke key")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Grab old hash before rotation so we can invalidate the cache.
	oldHash := ""
	if oldKey, err := s.keyStore.GetAPIKey(r.Context(), id); err == nil {
		oldHash = oldKey.KeyHash
	}

	result, err := s.keyManager.RotateKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrAPIKeyNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeKeyNotFound, "key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternal, "failed to rotate key")
		return
	}

	if oldHash != "" {
		s.auth.InvalidateCache(oldHash)
	}

	meta, _ := json.Marshal(result.Meta)
	writeJSON(w, http.StatusOK, keyCreateResponse{OK: true, Key: result.Key, Meta: meta})
}

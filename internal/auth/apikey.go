package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *authService) InvalidateCache(keyHash string) {
	s.cache.Delete(keyHash)
}

// ValidateAPIKey validates an API key against the api_keys table.
//
// The lookup is hash-based (SHA-256 of the plaintext key looked up by DB index)
// rather than using crypto/subtle.ConstantTimeCompare. This is intentional:
// ConstantTimeCompare prevents timing attacks when comparing plaintext secrets
// character-by-character, but here we compare pre-computed hashes via DB query.
// The DB index lookup is O(1) and does not leak timing information about the
// original key material.
func (s *authService) ValidateAPIKey(ctx context.Context, key string) (KeyValidationResult, error) {
	hash := sha256Hex(key)

	// Check cache first.
	if entry, ok := s.cache.Load(hash); ok {
		cached := entry.(*store.APIKey)
		if !isExpiredOrRevoked(cached) {
			return makeResult(cached, true), nil
		}
		s.cache.Delete(hash)
	}

	// Cache miss — query the database.
	k, err := s.keyStore.GetAPIKeyByHash(ctx, hash)
	if errors.Is(err, store.ErrAPIKeyNotFound) {
		return KeyValidationResult{}, nil
	}
	if err != nil {
		return KeyValidationResult{}, fmt.Errorf("validate api key: %w", err)
	}

	if isExpiredOrRevoked(k) {
		return KeyValidationResult{}, nil
	}

	s.cache.Store(hash, k)
	return makeResult(k, true), nil
}

func isExpiredOrRevoked(k *store.APIKey) bool {
	if k.RevokedAt != nil {
		return true
	}
	if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return true
	}
	return false
}

func makeResult(k *store.APIKey, valid bool) KeyValidationResult {
	return KeyValidationResult{
		Valid:       valid,
		KeyID:       k.ID,
		Permissions: k.Permissions,
	}
}

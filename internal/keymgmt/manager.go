package keymgmt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

const keyPrefix = "aek_"

// base64urlEncoding is the standard Base64url encoding without padding,
// used for encoding random key bytes.
var base64urlEncoding = base64.URLEncoding.WithPadding(base64.NoPadding)

type keyManager struct {
	store store.KeyStore
}

// New creates a KeyManager backed by the given KeyStore.
func New(ks store.KeyStore) KeyManager {
	return &keyManager{store: ks}
}

// generateKey produces a cryptographically random API key.
// Format: "aek_" + 32 bytes random → Base64url no-padding = 47 characters.
func generateKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return keyPrefix + base64urlEncoding.EncodeToString(raw), nil
}

// hashKey returns the hex-encoded SHA-256 hash of the given key.
func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// newUUIDv4 generates a random UUID v4 using crypto/rand.
func newUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	// Set version 4 (0100xxxx).
	b[6] = (b[6] & 0x0f) | 0x40
	// Set variant bits to 10xxxxxx (RFC 9562).
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// CreateKey generates a new API key and persists it.
func (km *keyManager) CreateKey(ctx context.Context, name string, perms store.KeyPermissions, expiresIn *time.Duration) (*CreatedKey, error) {
	plaintext, err := generateKey()
	if err != nil {
		return nil, err
	}

	id, err := newUUIDv4()
	if err != nil {
		return nil, err
	}

	var expiresAt *time.Time
	if expiresIn != nil {
		t := time.Now().Add(*expiresIn)
		expiresAt = &t
	}

	k := &store.APIKey{
		ID:          id,
		Name:        name,
		KeyHash:     hashKey(plaintext),
		KeyPrefix:   plaintext[:8],
		Permissions: perms,
		ExpiresAt:   expiresAt,
	}
	if err := km.store.CreateAPIKey(ctx, k); err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}

	return &CreatedKey{Key: plaintext, Meta: apiKeyToMeta(k)}, nil
}

// GetKey returns the metadata for a single key by ID.
func (km *keyManager) GetKey(ctx context.Context, id string) (*KeyMeta, error) {
	k, err := km.store.GetAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	m := apiKeyToMeta(k)
	return &m, nil
}

// ListKeys returns metadata for all keys.
func (km *keyManager) ListKeys(ctx context.Context) ([]KeyMeta, error) {
	keys, err := km.store.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]KeyMeta, len(keys))
	for i := range keys {
		out[i] = apiKeyToMeta(&keys[i])
	}
	return out, nil
}

// RotateKey generates a new secret for an existing key.
func (km *keyManager) RotateKey(ctx context.Context, id string) (*CreatedKey, error) {
	if _, err := km.store.GetAPIKey(ctx, id); err != nil {
		return nil, err
	}

	plaintext, err := generateKey()
	if err != nil {
		return nil, err
	}

	newHash := hashKey(plaintext)
	newPrefix := plaintext[:8]
	if err := km.store.RotateAPIKey(ctx, id, newHash, newPrefix); err != nil {
		return nil, fmt.Errorf("rotate key: %w", err)
	}

	k, err := km.store.GetAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	return &CreatedKey{Key: plaintext, Meta: apiKeyToMeta(k)}, nil
}

// RevokeKey marks a key as revoked.
func (km *keyManager) RevokeKey(ctx context.Context, id string) error {
	return km.store.RevokeAPIKey(ctx, id)
}

// apiKeyToMeta converts a store APIKey to public-facing metadata.
func apiKeyToMeta(k *store.APIKey) KeyMeta {
	return KeyMeta{
		ID:          k.ID,
		Name:        k.Name,
		KeyPrefix:   k.KeyPrefix,
		Permissions: k.Permissions,
		CreatedAt:   k.CreatedAt,
		ExpiresAt:   k.ExpiresAt,
		RevokedAt:   k.RevokedAt,
	}
}

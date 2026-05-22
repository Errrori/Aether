package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/store"
)

// BootstrapConfigKeys migrates statically configured API keys from the config file
// into the api_keys table. Keys that already exist (matched by hash) are skipped.
func BootstrapConfigKeys(ctx context.Context, ks store.KeyStore, cfgKeys []config.APIKeyEntry) error {
	for i, entry := range cfgKeys {
		hash := sha256Hex(entry.Key)

		_, err := ks.GetAPIKeyByHash(ctx, hash)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrAPIKeyNotFound) {
			return fmt.Errorf("bootstrap: check key hash: %w", err)
		}

		name := entry.Description
		if name == "" {
			name = fmt.Sprintf("config-key-%d", i)
		}

		id, err := newUUIDv4()
		if err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}

		k := &store.APIKey{
			ID:          id,
			Name:        name,
			KeyHash:     hash,
			KeyPrefix:   entry.Key[:8],
			Permissions: store.KeyPermissions{Admin: true},
		}
		if err := ks.CreateAPIKey(ctx, k); err != nil {
			return fmt.Errorf("bootstrap: create key %q: %w", name, err)
		}
	}
	return nil
}

func newUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

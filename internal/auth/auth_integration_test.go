//go:build integration

package auth

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/store"
)

func testDSN() string {
	if dsn := os.Getenv("AETHER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://aether:aether@localhost:5433/aether_test?sslmode=disable"
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbCfg := &config.DatabaseConfig{
		DSN:             testDSN(),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxIdleTime: time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
	}
	retCfg := &config.RetentionConfig{
		DefaultTTL:       720 * time.Hour,
		DefaultMaxCount:  10000,
		EvictionInterval: 5 * time.Minute,
	}

	st, err := store.New(ctx, dbCfg, retCfg)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return st
}

func newTestAuthWithKeyStore(t *testing.T, ks store.KeyStore) Auth {
	t.Helper()
	cfg := &config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  30 * time.Second,
	}
	a, err := New(cfg, ks)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// --- A-1: ValidateAPIKey round-trip with real DB ---

func TestIntegration_ValidateAPIKey_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ks := st.(store.KeyStore)
	a := newTestAuthWithKeyStore(t, ks)
	ctx := context.Background()

	rawKey := "integration-test-key-1234567890abcdefghij"
	hash := sha256Hex(rawKey)

	k := &store.APIKey{
		ID:        "integ-roundtrip-id",
		Name:      "integ-roundtrip",
		KeyHash:   hash,
		KeyPrefix: rawKey[:8],
		Permissions: store.KeyPermissions{
			Publish: []string{"orders.*"},
			Admin:   true,
		},
	}
	if err := ks.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	result, err := a.ValidateAPIKey(ctx, rawKey)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if !result.Valid {
		t.Error("expected valid=true")
	}
	if result.KeyID != k.ID {
		t.Errorf("KeyID: expected %q, got %q", k.ID, result.KeyID)
	}
	if !result.Permissions.Admin {
		t.Error("expected Admin=true")
	}
	if len(result.Permissions.Publish) != 1 || result.Permissions.Publish[0] != "orders.*" {
		t.Errorf("Publish: expected [orders.*], got %v", result.Permissions.Publish)
	}
}

// --- A-1: ValidateAPIKey cache hit ---

func TestIntegration_ValidateAPIKey_CacheHit(t *testing.T) {
	st := newTestStore(t)
	ks := st.(store.KeyStore)
	a := newTestAuthWithKeyStore(t, ks)
	ctx := context.Background()

	rawKey := "integration-cache-hit-key-1234567890abcdef"
	hash := sha256Hex(rawKey)

	k := &store.APIKey{
		ID:        "integ-cachehit-id",
		Name:      "integ-cachehit",
		KeyHash:   hash,
		KeyPrefix: rawKey[:8],
	}
	if err := ks.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// First call — cache miss, queries DB.
	result1, err := a.ValidateAPIKey(ctx, rawKey)
	if err != nil {
		t.Fatalf("first ValidateAPIKey: %v", err)
	}
	if !result1.Valid {
		t.Fatal("expected valid on first call")
	}

	// Second call — should hit cache.
	result2, err := a.ValidateAPIKey(ctx, rawKey)
	if err != nil {
		t.Fatalf("second ValidateAPIKey: %v", err)
	}
	if !result2.Valid {
		t.Error("expected valid on second call (cache hit)")
	}
	if result1.KeyID != result2.KeyID {
		t.Errorf("KeyID mismatch: first=%q, second=%q", result1.KeyID, result2.KeyID)
	}
}

// --- A-1: ValidateAPIKey revoked key ---

func TestIntegration_ValidateAPIKey_RevokedKey(t *testing.T) {
	st := newTestStore(t)
	ks := st.(store.KeyStore)
	a := newTestAuthWithKeyStore(t, ks)
	ctx := context.Background()

	rawKey := "integration-revoked-key-1234567890abcdefgh"
	hash := sha256Hex(rawKey)

	k := &store.APIKey{
		ID:        "integ-revoked-id",
		Name:      "integ-revoked",
		KeyHash:   hash,
		KeyPrefix: rawKey[:8],
	}
	if err := ks.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Verify key is valid before revocation.
	result, err := a.ValidateAPIKey(ctx, rawKey)
	if err != nil {
		t.Fatalf("ValidateAPIKey before revoke: %v", err)
	}
	if !result.Valid {
		t.Fatal("expected valid before revocation")
	}

	// Revoke the key.
	if err := ks.RevokeAPIKey(ctx, k.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	// Invalidate cache so auth re-queries DB.
	a.InvalidateCache(hash)

	// Verify key is now invalid.
	result, err = a.ValidateAPIKey(ctx, rawKey)
	if err != nil {
		t.Fatalf("ValidateAPIKey after revoke: %v", err)
	}
	if result.Valid {
		t.Error("expected valid=false after revocation")
	}
}

// --- A-1: ValidateAPIKey expired key ---

func TestIntegration_ValidateAPIKey_ExpiredKey(t *testing.T) {
	st := newTestStore(t)
	ks := st.(store.KeyStore)
	a := newTestAuthWithKeyStore(t, ks)
	ctx := context.Background()

	rawKey := "integration-expired-key-1234567890abcdefghi"
	hash := sha256Hex(rawKey)
	past := time.Now().Add(-time.Hour)

	k := &store.APIKey{
		ID:        "integ-expired-id",
		Name:      "integ-expired",
		KeyHash:   hash,
		KeyPrefix: rawKey[:8],
		ExpiresAt: &past,
	}
	if err := ks.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	result, err := a.ValidateAPIKey(ctx, rawKey)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if result.Valid {
		t.Error("expected valid=false for expired key")
	}
}

// --- A-1: ValidateAPIKey not found ---

func TestIntegration_ValidateAPIKey_NotFound(t *testing.T) {
	st := newTestStore(t)
	ks := st.(store.KeyStore)
	a := newTestAuthWithKeyStore(t, ks)

	result, err := a.ValidateAPIKey(context.Background(), "nonexistent-key-1234567890abcdefgh")
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if result.Valid {
		t.Error("expected valid=false for nonexistent key")
	}
}

// --- K-10: BootstrapConfigKeys ---

func TestIntegration_BootstrapConfigKeys(t *testing.T) {
	st := newTestStore(t)
	ks := st.(store.KeyStore)
	ctx := context.Background()

	cfgKeys := []config.APIKeyEntry{
		{Key: "bootstrap-key-1-1234567890abcdefghijklmno", Description: "first-key"},
		{Key: "bootstrap-key-2-1234567890abcdefghijklmno", Description: "second-key"},
	}

	if err := BootstrapConfigKeys(ctx, ks, cfgKeys); err != nil {
		t.Fatalf("BootstrapConfigKeys: %v", err)
	}

	// Verify both keys exist and are valid.
	a := newTestAuthWithKeyStore(t, ks)
	for i, entry := range cfgKeys {
		result, err := a.ValidateAPIKey(ctx, entry.Key)
		if err != nil {
			t.Fatalf("key %d ValidateAPIKey: %v", i, err)
		}
		if !result.Valid {
			t.Errorf("key %d: expected valid=true", i)
		}
		if !result.Permissions.Admin {
			t.Errorf("key %d: expected Admin=true", i)
		}
	}
}

// --- K-10: BootstrapConfigKeys idempotent ---

func TestIntegration_BootstrapConfigKeys_Idempotent(t *testing.T) {
	st := newTestStore(t)
	ks := st.(store.KeyStore)
	ctx := context.Background()

	cfgKeys := []config.APIKeyEntry{
		{Key: "idempotent-key-1234567890abcdefghijklmno", Description: "idempotent-test"},
	}

	// First call — creates the key.
	if err := BootstrapConfigKeys(ctx, ks, cfgKeys); err != nil {
		t.Fatalf("first BootstrapConfigKeys: %v", err)
	}

	// Second call — should skip existing key (no error).
	if err := BootstrapConfigKeys(ctx, ks, cfgKeys); err != nil {
		t.Fatalf("second BootstrapConfigKeys: %v", err)
	}

	// Verify key still valid.
	a := newTestAuthWithKeyStore(t, ks)
	result, err := a.ValidateAPIKey(ctx, cfgKeys[0].Key)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if !result.Valid {
		t.Error("expected valid=true after idempotent bootstrap")
	}
}

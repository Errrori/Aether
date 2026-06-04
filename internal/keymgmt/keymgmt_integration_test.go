//go:build integration

package keymgmt

import (
	"context"
	"errors"
	"os"
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

func newTestKeyManager(t *testing.T) (KeyManager, store.KeyStore) {
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

	ks, ok := st.(store.KeyStore)
	if !ok {
		t.Fatal("store does not implement KeyStore")
	}

	return New(ks), ks
}

// TestIntegration_CreateKey_RoundTrip creates a key and verifies it exists in the DB.
func TestIntegration_CreateKey_RoundTrip(t *testing.T) {
	km, _ := newTestKeyManager(t)
	ctx := context.Background()

	created, err := km.CreateKey(ctx, "int-create-"+t.Name(), store.KeyPermissions{Admin: true}, nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	if len(created.Key) != 47 {
		t.Fatalf("expected key length 47, got %d", len(created.Key))
	}
	if created.Key[:4] != "aek_" {
		t.Fatalf("expected key prefix aek_, got %s", created.Key[:4])
	}
	if created.Meta.Name != "int-create-"+t.Name() {
		t.Fatalf("unexpected name: %s", created.Meta.Name)
	}
	if !created.Meta.Permissions.Admin {
		t.Fatal("expected admin=true")
	}

	// Verify it can be retrieved.
	got, err := km.GetKey(ctx, created.Meta.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if got.ID != created.Meta.ID {
		t.Fatalf("ID mismatch: %s vs %s", got.ID, created.Meta.ID)
	}
}

// TestIntegration_CreateKey_DuplicateName verifies the UNIQUE constraint on name.
func TestIntegration_CreateKey_DuplicateName(t *testing.T) {
	km, _ := newTestKeyManager(t)
	ctx := context.Background()
	name := "int-dupname-" + t.Name()

	if _, err := km.CreateKey(ctx, name, store.KeyPermissions{Admin: false}, nil); err != nil {
		t.Fatalf("first CreateKey: %v", err)
	}

	_, err := km.CreateKey(ctx, name, store.KeyPermissions{Admin: false}, nil)
	if err == nil {
		t.Fatal("expected duplicate name error, got nil")
	}
	if !errors.Is(err, store.ErrAPIKeyDuplicateName) {
		t.Fatalf("expected ErrAPIKeyDuplicateName, got: %v", err)
	}
}

// TestIntegration_RotateKey verifies rotation changes the key hash.
func TestIntegration_RotateKey(t *testing.T) {
	km, ks := newTestKeyManager(t)
	ctx := context.Background()

	created, err := km.CreateKey(ctx, "int-rotate-"+t.Name(), store.KeyPermissions{Admin: true}, nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	oldKey := created.Key

	// Rotate.
	rotated, err := km.RotateKey(ctx, created.Meta.ID)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if rotated.Key == oldKey {
		t.Fatal("expected rotated key to be different from original")
	}
	if rotated.Meta.ID != created.Meta.ID {
		t.Fatal("expected same key ID after rotation")
	}

	// Old key hash should no longer match.
	oldHash := hashKey(oldKey)
	_, err = ks.GetAPIKeyByHash(ctx, oldHash)
	if err == nil {
		t.Fatal("expected old key hash to not be found")
	}

	// New key hash should be findable.
	newHash := hashKey(rotated.Key)
	got, err := ks.GetAPIKeyByHash(ctx, newHash)
	if err != nil {
		t.Fatalf("GetAPIKeyByHash(new): %v", err)
	}
	if got.ID != created.Meta.ID {
		t.Fatal("new hash should map to same key ID")
	}
}

// TestIntegration_RevokeKey verifies revocation persists.
func TestIntegration_RevokeKey(t *testing.T) {
	km, _ := newTestKeyManager(t)
	ctx := context.Background()

	created, err := km.CreateKey(ctx, "int-revoke-"+t.Name(), store.KeyPermissions{Admin: false}, nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	if err := km.RevokeKey(ctx, created.Meta.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	got, err := km.GetKey(ctx, created.Meta.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if got.RevokedAt == nil {
		t.Fatal("expected RevokedAt to be set")
	}
}

// TestIntegration_ListKeys verifies ordering and completeness.
func TestIntegration_ListKeys(t *testing.T) {
	km, _ := newTestKeyManager(t)
	ctx := context.Background()

	// Create two keys with small delay to ensure ordering.
	if _, err := km.CreateKey(ctx, "int-list-a-"+t.Name(), store.KeyPermissions{Admin: false}, nil); err != nil {
		t.Fatalf("CreateKey a: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := km.CreateKey(ctx, "int-list-b-"+t.Name(), store.KeyPermissions{Admin: true}, nil); err != nil {
		t.Fatalf("CreateKey b: %v", err)
	}

	keys, err := km.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) < 2 {
		t.Fatalf("expected at least 2 keys, got %d", len(keys))
	}

	// Most recent should be first (ORDER BY created_at DESC).
	found := false
	for _, k := range keys {
		if k.Name == "int-list-b-"+t.Name() {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected to find key 'int-list-b' in list")
	}
}

// TestIntegration_CreateKey_WithExpiresIn verifies expiry is persisted.
func TestIntegration_CreateKey_WithExpiresIn(t *testing.T) {
	km, _ := newTestKeyManager(t)
	ctx := context.Background()
 expiresIn := 1 * time.Hour

	created, err := km.CreateKey(ctx, "int-expires-"+t.Name(), store.KeyPermissions{Admin: false}, &expiresIn)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if created.Meta.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set")
	}
	if created.Meta.ExpiresAt.Before(time.Now()) {
		t.Fatal("expected ExpiresAt to be in the future")
	}
}

// TestIntegration_GetKey_NotFound verifies missing key returns error.
func TestIntegration_GetKey_NotFound(t *testing.T) {
	km, _ := newTestKeyManager(t)
	ctx := context.Background()

	_, err := km.GetKey(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
}

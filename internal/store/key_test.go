//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateAPIKey(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	key := &APIKey{
		ID:        "550e8400-e29b-41d4-a716-446655440000",
		Name:      "test-key-1",
		KeyHash:   "abc123hash",
		KeyPrefix: "aek_xxxx",
		Permissions: KeyPermissions{
			Publish:   []string{"orders.*"},
			Subscribe: []string{"*"},
			Admin:     false,
		},
	}
	if err := s.CreateAPIKey(context.Background(), key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if key.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt after insert")
	}

	// Verify row exists in the database.
	var count int
	err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM api_keys WHERE id = $1`, key.ID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query api_keys: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestCreateAPIKey_DuplicateName(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	k1 := &APIKey{
		ID:        "k1-id", Name: "dup-name", KeyHash: "h1", KeyPrefix: "aek_a1",
	}
	if err := s.CreateAPIKey(context.Background(), k1); err != nil {
		t.Fatalf("first CreateAPIKey: %v", err)
	}

	k2 := &APIKey{
		ID: "k2-id", Name: "dup-name", KeyHash: "h2", KeyPrefix: "aek_b2",
	}
	err := s.CreateAPIKey(context.Background(), k2)
	if !errors.Is(err, ErrAPIKeyDuplicateName) {
		t.Fatalf("expected ErrAPIKeyDuplicateName, got %v", err)
	}
}

func TestGetAPIKey(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	expires := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key := &APIKey{
		ID:          "get-id",
		Name:        "get-test",
		KeyHash:     "gethash",
		KeyPrefix:   "aek_get1",
		Permissions: KeyPermissions{Publish: []string{"ch.*"}, Subscribe: []string{"ch.*"}, Admin: true},
		ExpiresAt:   &expires,
	}
	if err := s.CreateAPIKey(context.Background(), key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, err := s.GetAPIKey(context.Background(), key.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.Name != key.Name {
		t.Errorf("Name: expected %q, got %q", key.Name, got.Name)
	}
	if got.KeyHash != key.KeyHash {
		t.Errorf("KeyHash: expected %q, got %q", key.KeyHash, got.KeyHash)
	}
	if got.KeyPrefix != key.KeyPrefix {
		t.Errorf("KeyPrefix: expected %q, got %q", key.KeyPrefix, got.KeyPrefix)
	}
	if got.Permissions.Admin != true {
		t.Error("expected Admin=true")
	}
	if len(got.Permissions.Publish) != 1 || got.Permissions.Publish[0] != "ch.*" {
		t.Errorf("Publish: expected [ch.*], got %v", got.Permissions.Publish)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt: expected %v, got %v", expires, got.ExpiresAt)
	}
	if got.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
}

func TestGetAPIKey_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	_, err := s.GetAPIKey(context.Background(), "nonexistent-id")
	if !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

func TestListAPIKeys(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	for i := 0; i < 3; i++ {
		k := &APIKey{
			ID: "list-" + string(rune('0'+i)), Name: "list-" + string(rune('0'+i)),
			KeyHash: "h-" + string(rune('0'+i)), KeyPrefix: "aek_l" + string(rune('0'+i)),
		}
		if err := s.CreateAPIKey(context.Background(), k); err != nil {
			t.Fatalf("CreateAPIKey %d: %v", i, err)
		}
	}

	keys, err := s.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
	// Verify ordering: latest first.
	for i := 1; i < len(keys); i++ {
		if keys[i-1].CreatedAt.Before(keys[i].CreatedAt) {
			t.Errorf("keys not ordered by created_at DESC")
			break
		}
	}
}

func TestListAPIKeys_Empty(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	keys, err := s.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if keys == nil {
		t.Error("expected empty slice, got nil")
	}
	if len(keys) != 0 {
		t.Errorf("expected 0 keys, got %d", len(keys))
	}
}

func TestGetAPIKeyByHash(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	key := &APIKey{
		ID: "byhash-id", Name: "byhash-test", KeyHash: "byhash-hash", KeyPrefix: "aek_bh1",
	}
	if err := s.CreateAPIKey(context.Background(), key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, err := s.GetAPIKeyByHash(context.Background(), "byhash-hash")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("expected ID %q, got %q", key.ID, got.ID)
	}
}

func TestGetAPIKeyByHash_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	_, err := s.GetAPIKeyByHash(context.Background(), "no-such-hash")
	if !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

func TestRevokeAPIKey(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	key := &APIKey{
		ID: "revoke-id", Name: "revoke-test", KeyHash: "revoke-hash", KeyPrefix: "aek_rv1",
	}
	if err := s.CreateAPIKey(context.Background(), key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	if err := s.RevokeAPIKey(context.Background(), key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	got, err := s.GetAPIKey(context.Background(), key.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.RevokedAt == nil {
		t.Error("expected non-nil RevokedAt after revocation")
	}
}

func TestRevokeAPIKey_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	err := s.RevokeAPIKey(context.Background(), "no-such-id")
	if !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

func TestRotateAPIKey(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	key := &APIKey{
		ID: "rotate-id", Name: "rotate-test", KeyHash: "old-hash", KeyPrefix: "aek_old",
	}
	if err := s.CreateAPIKey(context.Background(), key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	if err := s.RotateAPIKey(context.Background(), key.ID, "new-hash", "aek_new"); err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}

	got, err := s.GetAPIKey(context.Background(), key.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.KeyHash != "new-hash" {
		t.Errorf("KeyHash: expected new-hash, got %q", got.KeyHash)
	}
	if got.KeyPrefix != "aek_new" {
		t.Errorf("KeyPrefix: expected aek_new, got %q", got.KeyPrefix)
	}
}

func TestRotateAPIKey_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	err := s.RotateAPIKey(context.Background(), "no-such-id", "h", "p")
	if !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

func TestAPIKey_NullableTimestamps(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	// Create key with ExpiresAt = nil.
	key := &APIKey{
		ID: "nullable-id", Name: "nullable-test", KeyHash: "nullable-hash", KeyPrefix: "aek_nl1",
	}
	if err := s.CreateAPIKey(context.Background(), key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, err := s.GetAPIKey(context.Background(), key.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expected nil ExpiresAt, got %v", *got.ExpiresAt)
	}
	if got.RevokedAt != nil {
		t.Errorf("expected nil RevokedAt, got %v", *got.RevokedAt)
	}
}

package keymgmt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

// mockKeyStore implements store.KeyStore with in-memory storage.
type mockKeyStore struct {
	mu   sync.Mutex
	keys map[string]*store.APIKey // keyed by ID
}

func newMockKeyStore() *mockKeyStore {
	return &mockKeyStore{keys: make(map[string]*store.APIKey)}
}

func (m *mockKeyStore) CreateAPIKey(ctx context.Context, key *store.APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check name uniqueness.
	for _, k := range m.keys {
		if k.Name == key.Name {
			return store.ErrAPIKeyDuplicateName
		}
	}

	key.CreatedAt = time.Now()
	clone := *key
	m.keys[key.ID] = &clone
	return nil
}

func (m *mockKeyStore) GetAPIKey(ctx context.Context, id string) (*store.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k, ok := m.keys[id]
	if !ok {
		return nil, store.ErrAPIKeyNotFound
	}
	clone := *k
	return &clone, nil
}

func (m *mockKeyStore) ListAPIKeys(ctx context.Context) ([]store.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]store.APIKey, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, *k)
	}
	return out, nil
}

func (m *mockKeyStore) GetAPIKeyByHash(ctx context.Context, hash string) (*store.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, k := range m.keys {
		if k.KeyHash == hash {
			clone := *k
			return &clone, nil
		}
	}
	return nil, store.ErrAPIKeyNotFound
}

func (m *mockKeyStore) RevokeAPIKey(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k, ok := m.keys[id]
	if !ok {
		return store.ErrAPIKeyNotFound
	}
	now := time.Now()
	k.RevokedAt = &now
	return nil
}

func (m *mockKeyStore) RotateAPIKey(ctx context.Context, id string, newHash, newPrefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k, ok := m.keys[id]
	if !ok {
		return store.ErrAPIKeyNotFound
	}
	k.KeyHash = newHash
	k.KeyPrefix = newPrefix
	return nil
}

func newTestManager(t *testing.T) KeyManager {
	t.Helper()
	return New(newMockKeyStore())
}

// --- Key format tests ---

func TestGenerateKey_Format(t *testing.T) {
	for i := 0; i < 10; i++ {
		k, err := generateKey()
		if err != nil {
			t.Fatalf("generateKey: %v", err)
		}
		if len(k) != 47 {
			t.Errorf("expected length 47, got %d: %q", len(k), k)
		}
		if k[:4] != "aek_" {
			t.Errorf("expected prefix aek_, got %q", k[:4])
		}
		for _, c := range k[4:] {
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				t.Errorf("invalid Base64url char %q in key suffix", c)
			}
		}
	}
}

func TestGenerateKey_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		k, err := generateKey()
		if err != nil {
			t.Fatalf("generateKey %d: %v", i, err)
		}
		if seen[k] {
			t.Errorf("duplicate key generated: %q", k)
		}
		seen[k] = true
	}
}

// --- Hash tests ---

func TestHashKey_Deterministic(t *testing.T) {
	h1 := hashKey("test-key")
	h2 := hashKey("test-key")
	if h1 != h2 {
		t.Errorf("same input should produce same hash: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(h1))
	}
}

func TestHashKey_Different(t *testing.T) {
	h1 := hashKey("key-a")
	h2 := hashKey("key-b")
	if h1 == h2 {
		t.Error("different inputs should produce different hashes")
	}
}

// --- CreateKey tests ---

func TestCreateKey(t *testing.T) {
	km := newTestManager(t)
	ctx := context.Background()

	perms := store.KeyPermissions{
		Publish: []string{"orders.*"}, Subscribe: []string{"*"}, Admin: false,
	}
	result, err := km.CreateKey(ctx, "test-key", perms, nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if result.Key == "" {
		t.Error("expected non-empty Key")
	}
	if len(result.Key) != 47 {
		t.Errorf("expected key length 47, got %d", len(result.Key))
	}
	if result.Meta.Name != "test-key" {
		t.Errorf("expected name test-key, got %q", result.Meta.Name)
	}
	if result.Meta.ID == "" {
		t.Error("expected non-empty ID")
	}
	if result.Meta.ExpiresAt != nil {
		t.Error("expected nil ExpiresAt")
	}
	if result.Meta.RevokedAt != nil {
		t.Error("expected nil RevokedAt")
	}
	if !result.Meta.Permissions.Admin == false {
		t.Error("expected Admin=false")
	}
	if result.Meta.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
	// Key is NOT in the Meta (only in the outer CreatedKey).
	if result.Meta.KeyPrefix != result.Key[:8] {
		t.Errorf("KeyPrefix mismatch: %q vs %q", result.Meta.KeyPrefix, result.Key[:8])
	}
}

func TestCreateKey_WithExpiresIn(t *testing.T) {
	km := newTestManager(t)
	ctx := context.Background()

	expiresIn := 1 * time.Hour
	result, err := km.CreateKey(ctx, "expiring-key", store.KeyPermissions{}, &expiresIn)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if result.Meta.ExpiresAt == nil {
		t.Fatal("expected non-nil ExpiresAt")
	}
	now := time.Now()
	if result.Meta.ExpiresAt.Before(now) {
		t.Error("ExpiresAt should be in the future")
	}
	maxExpected := now.Add(expiresIn + time.Minute)
	if result.Meta.ExpiresAt.After(maxExpected) {
		t.Error("ExpiresAt is too far in the future")
	}
}

func TestCreateKey_DuplicateName(t *testing.T) {
	km := newTestManager(t)
	ctx := context.Background()

	_, err := km.CreateKey(ctx, "dup", store.KeyPermissions{}, nil)
	if err != nil {
		t.Fatalf("first CreateKey: %v", err)
	}
	_, err = km.CreateKey(ctx, "dup", store.KeyPermissions{}, nil)
	if !errors.Is(err, store.ErrAPIKeyDuplicateName) {
		t.Fatalf("expected ErrAPIKeyDuplicateName, got %v", err)
	}
}

// --- GetKey tests ---

func TestGetKey(t *testing.T) {
	km := newTestManager(t)
	ctx := context.Background()

	created, err := km.CreateKey(ctx, "get-test", store.KeyPermissions{Admin: true}, nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	got, err := km.GetKey(ctx, created.Meta.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if got.Name != "get-test" {
		t.Errorf("expected name get-test, got %q", got.Name)
	}
	if !got.Permissions.Admin {
		t.Error("expected Admin=true")
	}
}

func TestGetKey_NotFound(t *testing.T) {
	km := newTestManager(t)

	_, err := km.GetKey(context.Background(), "no-such-id")
	if !errors.Is(err, store.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

// --- ListKeys tests ---

func TestListKeys(t *testing.T) {
	km := newTestManager(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := km.CreateKey(ctx, "list-"+string(rune('a'+i)), store.KeyPermissions{}, nil)
		if err != nil {
			t.Fatalf("CreateKey %d: %v", i, err)
		}
	}

	keys, err := km.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
}

func TestListKeys_Empty(t *testing.T) {
	km := newTestManager(t)

	keys, err := km.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected 0 keys, got %d", len(keys))
	}
}

// --- RotateKey tests ---

func TestRotateKey(t *testing.T) {
	km := newTestManager(t)
	ctx := context.Background()

	created, err := km.CreateKey(ctx, "rotate-me", store.KeyPermissions{}, nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	rotated, err := km.RotateKey(ctx, created.Meta.ID)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if rotated.Key == created.Key {
		t.Error("rotated key should differ from original")
	}
	if rotated.Meta.Name != created.Meta.Name {
		t.Error("name should not change after rotation")
	}
	if rotated.Meta.KeyPrefix != rotated.Key[:8] {
		t.Errorf("KeyPrefix mismatch after rotation: %q vs %q", rotated.Meta.KeyPrefix, rotated.Key[:8])
	}
	// Verify that the key stored in mock actually changed.
	got, err := km.GetKey(ctx, created.Meta.ID)
	if err != nil {
		t.Fatalf("GetKey after rotation: %v", err)
	}
	if got.KeyPrefix != rotated.Key[:8] {
		t.Error("stored KeyPrefix does not match rotated key")
	}
}

func TestRotateKey_NotFound(t *testing.T) {
	km := newTestManager(t)

	_, err := km.RotateKey(context.Background(), "no-such-id")
	if !errors.Is(err, store.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

// --- RevokeKey tests ---

func TestRevokeKey(t *testing.T) {
	km := newTestManager(t)
	ctx := context.Background()

	created, err := km.CreateKey(ctx, "revoke-me", store.KeyPermissions{}, nil)
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
		t.Error("expected non-nil RevokedAt after revocation")
	}
}

func TestRevokeKey_NotFound(t *testing.T) {
	km := newTestManager(t)

	err := km.RevokeKey(context.Background(), "no-such-id")
	if !errors.Is(err, store.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

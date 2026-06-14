//go:build bench

package auth

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/store"
	"github.com/golang-jwt/jwt/v5"
)

// benchMockKeyStore implements store.KeyStore for benchmarks that don't need DB.
type benchMockKeyStore struct{}

func (m *benchMockKeyStore) CreateAPIKey(context.Context, *store.APIKey) error  { return nil }
func (m *benchMockKeyStore) GetAPIKey(context.Context, string) (*store.APIKey, error) {
	return nil, store.ErrAPIKeyNotFound
}
func (m *benchMockKeyStore) ListAPIKeys(context.Context) ([]store.APIKey, error)   { return nil, nil }
func (m *benchMockKeyStore) GetAPIKeyByHash(context.Context, string) (*store.APIKey, error) {
	return nil, store.ErrAPIKeyNotFound
}
func (m *benchMockKeyStore) RevokeAPIKey(context.Context, string) error                 { return nil }
func (m *benchMockKeyStore) RotateAPIKey(context.Context, string, string, string) error { return nil }

// --- helpers reused across benchmarks ---

func newBenchAuthWithKeyStore(b *testing.B, ks store.KeyStore) Auth {
	b.Helper()
	return newTestAuthWithKeyStore(b, ks)
}

func sha256HexBench(s string) string { return sha256Hex(s) }

func makeTestAPIKey(id string, rawKey string) *store.APIKey {
	hash := sha256HexBench(rawKey)
	return &store.APIKey{
		ID:        id,
		Name:      id,
		KeyHash:   hash,
		KeyPrefix: rawKey[:8],
	}
}

// --- BenchmarkValidateAPIKey_CacheHit ---

func BenchmarkValidateAPIKey_CacheHit(b *testing.B) {
	st := newTestStore(b)
	ks := st.(store.KeyStore)
	a := newBenchAuthWithKeyStore(b, ks)
	ctx := context.Background()

	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	rawKey := "bench-cache-hit-" + uid + "-1234567890ab"
	k := makeTestAPIKey("bench-cachehit-"+uid, rawKey)
	if err := ks.CreateAPIKey(ctx, k); err != nil {
		b.Fatal(err)
	}

	// Warm cache.
	if _, err := a.ValidateAPIKey(ctx, rawKey); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Intentionally discard: key was validated during warm-up above.
		_, _ = a.ValidateAPIKey(ctx, rawKey)
	}
}

// --- BenchmarkValidateAPIKey_CacheMiss ---

func BenchmarkValidateAPIKey_CacheMiss(b *testing.B) {
	st := newTestStore(b)
	ks := st.(store.KeyStore)
	a := newBenchAuthWithKeyStore(b, ks)
	ctx := context.Background()

	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	const poolSize = 5000
	rawKeys := make([]string, poolSize)
	for i := 0; i < poolSize; i++ {
		rawKey := fmt.Sprintf("bench-cm-%s-%d-%s", uid, i, strings.Repeat("x", 30))
		rawKeys[i] = rawKey
		k := makeTestAPIKey(fmt.Sprintf("bench-cm-%s-%d", uid, i), rawKey)
		if err := ks.CreateAPIKey(ctx, k); err != nil {
			b.Fatal(err)
		}
	}

	var idx atomic.Int64

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		keyIdx := int(idx.Add(1) % poolSize)
		// Intentionally discard: the result is always valid (keys were pre-created above).
		_, _ = a.ValidateAPIKey(ctx, rawKeys[keyIdx])
	}
}

// --- BenchmarkParseAndValidateToken ---

func BenchmarkParseAndValidateToken(b *testing.B) {
	// Use a service with deterministic signing key, no DB needed.
	svc, ok := newBenchAuthWithKeyStore(b, &benchMockKeyStore{}).(*authService)
	if !ok {
		b.Fatal("expected *authService")
	}

	now := time.Now()
	claims := &jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "bench-user",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Channels: []string{"*"},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString(svc.signingKey)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := svc.ParseAndValidateToken(tokenStr)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// --- BenchmarkIsChannelAuthorized ---

func BenchmarkIsChannelAuthorized(b *testing.B) {
	svc, ok := newBenchAuthWithKeyStore(b, &benchMockKeyStore{}).(*authService)
	if !ok {
		b.Fatal("expected *authService")
	}

	claims := &Claims{
		Subject:  "bench",
		Channels: []string{"orders.*", "alerts.*", "system.*"},
	}
	channels := []string{
		"orders.1234",
		"alerts.cpu.high",
		"system.health",
		"orders.5678.detail",
		"unknown.channel",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = svc.IsChannelAuthorized(claims, channels[i%len(channels)])
	}
}

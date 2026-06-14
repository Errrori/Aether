package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/store"
)

var (
	ErrTokenExpired        = errors.New("token expired")
	ErrInvalidToken        = errors.New("invalid token")
	ErrInvalidAlgorithm    = errors.New("invalid algorithm")
	ErrUnauthorizedChannel = errors.New("unauthorized channel")
)

// Claims holds the identity and channel authorizations extracted from a JWT.
type Claims struct {
	Subject  string
	Channels []string
}

// KeyValidationResult is returned by ValidateAPIKey, replacing the previous bool.
type KeyValidationResult struct {
	Valid       bool
	KeyID       string
	Permissions store.KeyPermissions
}

// Auth is the authentication and authorization interface for Aether.
type Auth interface {
	ValidateAPIKey(ctx context.Context, key string) (KeyValidationResult, error)
	InvalidateCache(keyHash string)
	ParseAndValidateToken(tokenString string) (*Claims, error)
	IsChannelAuthorized(claims *Claims, channel string) bool
	CacheStats() (hits, misses int64)
}

type authService struct {
	signingKey []byte
	clockSkew  time.Duration
	keyStore   store.KeyStore
	cache      sync.Map // key_hash → *store.APIKey

	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
}

// New creates an Auth service backed by the given KeyStore.
func New(cfg *config.AuthConfig, ks store.KeyStore) (Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("auth config is nil")
	}
	if ks == nil {
		return nil, fmt.Errorf("keystore is nil")
	}

	return &authService{
		signingKey: []byte(cfg.JWTSigningKey),
		clockSkew:  cfg.JWTClockSkew,
		keyStore:   ks,
	}, nil
}

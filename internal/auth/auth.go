package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/aether-mq/aether/internal/config"
)

var (
	ErrTokenExpired        = errors.New("token expired")
	ErrInvalidToken        = errors.New("invalid token")
	ErrInvalidAlgorithm    = errors.New("invalid algorithm")
	ErrUnauthorizedChannel = errors.New("unauthorized channel")
)

type Claims struct {
	Subject  string
	Channels []string
}

type Auth interface {
	ValidateAPIKey(key string) bool
	ParseAndValidateToken(tokenString string) (*Claims, error)
	IsChannelAuthorized(claims *Claims, channel string) bool
}

type authService struct {
	signingKey []byte
	clockSkew  time.Duration
	apiKeys    [][]byte
}

func New(cfg *config.AuthConfig) (Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("auth config is nil")
	}

	keys := make([][]byte, len(cfg.APIKeys))
	for i, entry := range cfg.APIKeys {
		keys[i] = []byte(entry.Key)
	}

	return &authService{
		signingKey: []byte(cfg.JWTSigningKey),
		clockSkew:  cfg.JWTClockSkew,
		apiKeys:    keys,
	}, nil
}

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

func newTestAuth(t *testing.T) Auth {
	t.Helper()
	cfg := &config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  30 * time.Second,
		APIKeys: []config.APIKeyEntry{
			{Key: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY", Description: "test"},
			{Key: "QkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWjAxMjM0NTY3ODk", Description: "second"},
		},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func generateToken(t *testing.T, signingKey string, subject string, channels []string, exp time.Time) string {
	t.Helper()
	claims := &jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: subject,
		},
		Channels: channels,
	}
	if !exp.IsZero() {
		claims.ExpiresAt = jwt.NewNumericDate(exp)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := token.SignedString([]byte(signingKey))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func generateTokenWithAlg(t *testing.T, signingKey string, method jwt.SigningMethod, subject string, channels []string, exp time.Time) string {
	t.Helper()
	claims := &jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: subject,
		},
		Channels: channels,
	}
	if !exp.IsZero() {
		claims.ExpiresAt = jwt.NewNumericDate(exp)
	}
	token := jwt.NewWithClaims(method, claims)
	var key interface{}
	switch method.(type) {
	case *jwt.SigningMethodHMAC:
		key = []byte(signingKey)
	case *jwt.SigningMethodRSA:
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		key = k
	default:
		t.Fatalf("unsupported signing method: %v", method.Alg())
	}
	s, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token with %s: %v", method.Alg(), err)
	}
	return s
}

func generateNoneToken(t *testing.T, subject string, channels []string) string {
	t.Helper()
	claims := &jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: subject,
		},
		Channels: channels,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	s, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none token: %v", err)
	}
	return s
}

// ── A-1: API Key Validation ──

func TestValidateAPIKey(t *testing.T) {
	a := newTestAuth(t)

	tests := []struct {
		name   string
		key    string
		expect bool
	}{
		{"first valid key", "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY", true},
		{"second valid key", "QkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWjAxMjM0NTY3ODk", true},
		{"invalid key", "invalid_key_that_does_not_match_anything_XXXX", false},
		{"empty key", "", false},
		{"wrong case", "ywjjzgvmz2hpamtsbw5vchfyc3r1dnd4exoxmjm0nty", false},
		{"same length invalid", strings.Repeat("a", 44), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.ValidateAPIKey(tt.key)
			if got != tt.expect {
				t.Errorf("ValidateAPIKey(%q) = %v, want %v", tt.key, got, tt.expect)
			}
		})
	}
}

func TestValidateAPIKey_NoKeys(t *testing.T) {
	cfg := &config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  30 * time.Second,
		APIKeys:       nil,
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.ValidateAPIKey("anything") {
		t.Error("ValidateAPIKey should return false when no keys are configured")
	}
}

// ── A-2: JWT Algorithm Validation ──

func TestParseAndValidateToken_Algorithm(t *testing.T) {
	a := newTestAuth(t)
	key := strings.Repeat("a", 32)

	t.Run("HS256 valid", func(t *testing.T) {
		token := generateToken(t, key, "sub1", []string{"ch1"}, time.Now().Add(time.Hour))
		claims, err := a.ParseAndValidateToken(token)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if claims.Subject != "sub1" {
			t.Errorf("subject = %q, want %q", claims.Subject, "sub1")
		}
	})

	t.Run("none algorithm rejected", func(t *testing.T) {
		token := generateNoneToken(t, "sub1", []string{"ch1"})
		_, err := a.ParseAndValidateToken(token)
		if err == nil {
			t.Error("expected error for none algorithm")
		}
	})

	t.Run("HS384 rejected", func(t *testing.T) {
		token := generateTokenWithAlg(t, key, jwt.SigningMethodHS384, "sub1", []string{"ch1"}, time.Now().Add(time.Hour))
		_, err := a.ParseAndValidateToken(token)
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("HS512 rejected", func(t *testing.T) {
		token := generateTokenWithAlg(t, key, jwt.SigningMethodHS512, "sub1", []string{"ch1"}, time.Now().Add(time.Hour))
		_, err := a.ParseAndValidateToken(token)
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("RS256 rejected", func(t *testing.T) {
		token := generateTokenWithAlg(t, key, jwt.SigningMethodRS256, "sub1", []string{"ch1"}, time.Now().Add(time.Hour))
		_, err := a.ParseAndValidateToken(token)
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})
}

func TestParseAndValidateToken_Malformed(t *testing.T) {
	a := newTestAuth(t)

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{"empty string", "", ErrInvalidToken},
		{"garbage", "not.a.jwt", ErrInvalidToken},
		{"wrong signature", generateToken(t, strings.Repeat("b", 32), "sub", nil, time.Now().Add(time.Hour)), ErrInvalidToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.ParseAndValidateToken(tt.input)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

// ── A-3: JWT Expiry Validation ──

func TestParseAndValidateToken_Expiry(t *testing.T) {
	a := newTestAuth(t)
	key := strings.Repeat("a", 32)

	tests := []struct {
		name    string
		exp     time.Time
		wantErr error
	}{
		{"valid unexpired", time.Now().Add(time.Hour), nil},
		{"no expiry", time.Time{}, nil},
		{"expired outside skew", time.Now().Add(-60 * time.Second), ErrTokenExpired},
		{"expired within skew", time.Now().Add(-20 * time.Second), nil},
		{"expired at skew boundary", time.Now().Add(-29 * time.Second), nil},
		{"expired at skew boundary +1s", time.Now().Add(-35 * time.Second), ErrTokenExpired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := generateToken(t, key, "sub", nil, tt.exp)
			_, err := a.ParseAndValidateToken(token)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("expected %v, got %v", tt.wantErr, err)
				}
			}
		})
	}
}

func TestParseAndValidateToken_LargeClockSkew(t *testing.T) {
	cfg := &config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  5 * time.Minute,
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token := generateToken(t, cfg.JWTSigningKey, "sub", nil, time.Now().Add(-4*time.Minute))
	_, err = a.ParseAndValidateToken(token)
	if err != nil {
		t.Errorf("token expired 4min ago should be accepted with 5min skew, got: %v", err)
	}
}

// ── A-4: Channel Authorization ──

func TestIsChannelAuthorized(t *testing.T) {
	a := newTestAuth(t)

	tests := []struct {
		name     string
		claims   *Claims
		channel  string
		expected bool
	}{
		{"exact match", &Claims{Channels: []string{"order.1234"}}, "order.1234", true},
		{"exact no match", &Claims{Channels: []string{"order.1234"}}, "order.5678", false},
		{"wildcard child", &Claims{Channels: []string{"order.*"}}, "order.1234", true},
		{"wildcard nested", &Claims{Channels: []string{"order.*"}}, "order.1234.detail", true},
		{"wildcard deep nested", &Claims{Channels: []string{"order.*"}}, "order.a.b.c.d", true},
		{"wildcard no match different prefix", &Claims{Channels: []string{"order.*"}}, "orders.1234", false},
		{"wildcard no match bare prefix", &Claims{Channels: []string{"order.*"}}, "order", false},
		{"global wildcard any", &Claims{Channels: []string{"*"}}, "anything.at.all", true},
		{"global wildcard specific", &Claims{Channels: []string{"*"}}, "foo.bar.baz", true},
		{"mixed patterns match", &Claims{Channels: []string{"admin", "user.*"}}, "user.1234", true},
		{"mixed patterns no match", &Claims{Channels: []string{"admin", "system.*"}}, "user.1234", false},
		{"multiple wildcards match", &Claims{Channels: []string{"a.*", "b.*"}}, "b.child", true},
		{"wildcard bare dot", &Claims{Channels: []string{".*"}}, "anything", false},
			{"nil claims", nil, "anything", false},
		{"empty channels", &Claims{Channels: []string{}}, "anything", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.IsChannelAuthorized(tt.claims, tt.channel)
			if got != tt.expected {
				t.Errorf("IsChannelAuthorized(%v, %q) = %v, want %v", tt.claims, tt.channel, got, tt.expected)
			}
		})
	}
}

// ── Claims Extraction ──

func TestParseAndValidateToken_ClaimsExtraction(t *testing.T) {
	a := newTestAuth(t)
	key := strings.Repeat("a", 32)
	token := generateToken(t, key, "user-42", []string{"order.*", "system.alerts"}, time.Now().Add(time.Hour))

	claims, err := a.ParseAndValidateToken(token)
	if err != nil {
		t.Fatalf("ParseAndValidateToken: %v", err)
	}
	if claims.Subject != "user-42" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-42")
	}
	if len(claims.Channels) != 2 {
		t.Fatalf("len(Channels) = %d, want 2", len(claims.Channels))
	}
	if claims.Channels[0] != "order.*" || claims.Channels[1] != "system.alerts" {
		t.Errorf("Channels = %v, want [order.*, system.alerts]", claims.Channels)
	}
}

func TestParseAndValidateToken_MissingSub(t *testing.T) {
	a := newTestAuth(t)
	key := strings.Repeat("a", 32)
	token := generateToken(t, key, "", []string{"ch1"}, time.Now().Add(time.Hour))

	claims, err := a.ParseAndValidateToken(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Subject != "" {
		t.Errorf("Subject = %q, want empty", claims.Subject)
	}
}

// ── Constructor ──

func TestNew(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		cfg := &config.AuthConfig{
			JWTSigningKey: strings.Repeat("a", 32),
			JWTClockSkew:  30 * time.Second,
		}
		a, err := New(cfg)
		if err != nil {
			t.Errorf("New with valid config: %v", err)
		}
		if a == nil {
			t.Error("New returned nil Auth")
		}
	})

	t.Run("nil config", func(t *testing.T) {
		_, err := New(nil)
		if err == nil {
			t.Error("New(nil) should return error")
		}
	})
}

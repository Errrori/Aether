//go:build integration || bench

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

func newTestStore(tb testing.TB) store.Store {
	tb.Helper()
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
		tb.Fatalf("connect to test db: %v", err)
	}
	tb.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		tb.Fatalf("run migrations: %v", err)
	}

	return st
}

func newTestAuthWithKeyStore(tb testing.TB, ks store.KeyStore) Auth {
	tb.Helper()
	cfg := &config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  30 * time.Second,
	}
	a, err := New(cfg, ks)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return a
}

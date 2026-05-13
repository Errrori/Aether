package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aether-mq/aether/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgStore struct {
	pool      *pgxpool.Pool
	retention *config.RetentionConfig
	ruleMatch func(channel string) (ttl time.Duration, maxCount int)
}

// New creates a pgStore backed by a pgx connection pool.
func New(ctx context.Context, dbCfg *config.DatabaseConfig, retCfg *config.RetentionConfig) (*pgStore, error) {
	poolCfg, err := pgxpool.ParseConfig(dbCfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}

	poolCfg.MaxConns = int32(dbCfg.MaxOpenConns)
	poolCfg.MinConns = int32(dbCfg.MaxIdleConns)
	poolCfg.MaxConnIdleTime = dbCfg.ConnMaxIdleTime
	poolCfg.MaxConnLifetime = dbCfg.ConnMaxLifetime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &pgStore{
		pool:      pool,
		retention: retCfg,
	}
	s.ruleMatch = s.defaultRuleMatch

	return s, nil
}

func (s *pgStore) defaultRuleMatch(channel string) (time.Duration, int) {
	cfg := &config.Config{Retention: *s.retention}
	return cfg.MatchRetentionRule(channel)
}

// Ping validates that the PostgreSQL connection is usable.
func (s *pgStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close releases all connection pool resources.
func (s *pgStore) Close() {
	s.pool.Close()
}

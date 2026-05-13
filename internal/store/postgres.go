package store

import (
	"context"
	"fmt"
	"strings"
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
func New(ctx context.Context, dbCfg *config.DatabaseConfig, retCfg *config.RetentionConfig) (Store, error) {
	poolCfg, err := pgxpool.ParseConfig(dbCfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}

	poolCfg.MaxConns = int32(dbCfg.MaxOpenConns)
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
	s.ruleMatch = s.matchRetentionRule

	return s, nil
}

// matchRetentionRule returns the TTL and MaxCount for a channel by matching
// against the configured retention rules. The first rule whose pattern matches
// the channel name wins; unmatched channels receive defaults.
func (s *pgStore) matchRetentionRule(channel string) (time.Duration, int) {
	for _, rule := range s.retention.Rules {
		if strings.HasSuffix(rule.Pattern, ".*") {
			prefix := strings.TrimSuffix(rule.Pattern, ".*")
			if strings.HasPrefix(channel, prefix+".") {
				return rule.TTL, rule.MaxCount
			}
		} else if channel == rule.Pattern {
			return rule.TTL, rule.MaxCount
		}
	}
	return s.retention.DefaultTTL, s.retention.DefaultMaxCount
}

// Ping validates that the PostgreSQL connection is usable.
func (s *pgStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close releases all connection pool resources.
func (s *pgStore) Close() {
	s.pool.Close()
}

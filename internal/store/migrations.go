package store

import (
	"context"
	"fmt"
)

type migration struct {
	Version int
	SQL     string
}

var migrations = []migration{
	{
		Version: 1,
		SQL: `CREATE TABLE IF NOT EXISTS channels (
    name        TEXT PRIMARY KEY,
    current_seq BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);`,
	},
	{
		Version: 2,
		SQL: `CREATE TABLE IF NOT EXISTS messages (
    id              BIGSERIAL PRIMARY KEY,
    channel         TEXT NOT NULL REFERENCES channels(name),
    seq_id          BIGINT NOT NULL,
    payload         JSONB NOT NULL,
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (channel, seq_id),
    UNIQUE (channel, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_messages_channel_seq ON messages (channel, seq_id);
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages (created_at);`,
	},
}

// RunMigrations creates the database schema if it does not exist.
// Each migration runs in its own transaction. Repeated calls are idempotent.
func (s *pgStore) RunMigrations(ctx context.Context) error {
	// Bootstrap: ensure schema_migrations table exists before we can track versions.
	_, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var applied bool
		err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, m.Version,
		).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration v%d: %w", m.Version, err)
		}
		if applied {
			continue
		}

		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", m.Version, err)
		}
		defer tx.Rollback(ctx)

		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("apply migration v%d: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version,
		); err != nil {
			return fmt.Errorf("record migration v%d: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration v%d: %w", m.Version, err)
		}
	}

	return nil
}

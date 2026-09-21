// Package storetest provides helpers for integration tests that exercise the
// store schema.
package storetest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TruncateStmt removes all rows from every store table and resets identity
// sequences. schema_migrations is left intact so migration state is kept.
// Keep the table list in sync with migrations: a table missing here leaks
// rows across test cases without any assertion noticing.
const TruncateStmt = `TRUNCATE subscriber_cursors, webhook_deliveries, webhooks, api_keys, messages, channels RESTART IDENTITY CASCADE`

// TruncateAll executes TruncateStmt on the database at dsn. Integration test
// suites call it so every test starts from a clean schema, independent of
// leftover rows from previous tests or runs.
func TruncateAll(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("storetest: connect: %w", err)
	}
	defer func() {
		// Close error is not actionable in test teardown.
		_ = conn.Close(ctx)
	}()
	if _, err := conn.Exec(ctx, TruncateStmt); err != nil {
		return fmt.Errorf("storetest: truncate: %w", err)
	}
	return nil
}

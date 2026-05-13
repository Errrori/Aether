package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WriteMessage atomically writes a message to a channel.
// If idempotencyKey is non-nil and a message with the same (channel, idempotencyKey) already exists,
// the original seq_id and timestamp are returned without re-writing.
func (s *pgStore) WriteMessage(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (int64, time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Step 1: Ensure channel exists.
	_, err = tx.Exec(ctx,
		`INSERT INTO channels (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		channel,
	)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("ensure channel: %w", err)
	}

	// Step 2: Lock channel row and read current_seq.
	var currentSeq int64
	err = tx.QueryRow(ctx,
		`SELECT current_seq FROM channels WHERE name = $1 FOR UPDATE`,
		channel,
	).Scan(&currentSeq)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("lock channel: %w", err)
	}

	// Step 3: Compute new seq.
	newSeq := currentSeq + 1

	// Step 4: Insert message.
	// When idempotency_key is NULL, the UNIQUE(channel, idempotency_key) constraint
	// does not fire (NULL != NULL in SQL), so ON CONFLICT is a no-op and INSERT always succeeds.
	var seqID int64
	var createdAt time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO messages (channel, seq_id, payload, idempotency_key)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (channel, idempotency_key) DO NOTHING
		 RETURNING seq_id, created_at`,
		channel, newSeq, payload, idempotencyKey,
	).Scan(&seqID, &createdAt)

	if err != nil {
		if err == pgx.ErrNoRows {
			// Step 5: Idempotency conflict — query the existing message.
			err = tx.QueryRow(ctx,
				`SELECT seq_id, created_at FROM messages WHERE channel = $1 AND idempotency_key = $2`,
				channel, *idempotencyKey,
			).Scan(&seqID, &createdAt)
			if err != nil {
				return 0, time.Time{}, fmt.Errorf("query idempotent message: %w", err)
			}
			// Skip step 6: do not increment seq.
			if err := tx.Commit(ctx); err != nil {
				return 0, time.Time{}, fmt.Errorf("commit idempotent read: %w", err)
			}
			return seqID, createdAt, nil
		}
		return 0, time.Time{}, fmt.Errorf("insert message: %w", err)
	}

	// Step 6: Advance channel seq.
	_, err = tx.Exec(ctx,
		`UPDATE channels SET current_seq = current_seq + 1, updated_at = now() WHERE name = $1`,
		channel,
	)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("advance seq: %w", err)
	}

	// Step 7: Commit.
	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, fmt.Errorf("commit: %w", err)
	}

	return seqID, createdAt, nil
}

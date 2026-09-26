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
	if err := ValidateChannelName(channel); err != nil {
		return 0, time.Time{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful Commit is a no-op; the error is not actionable.
		_ = tx.Rollback(ctx)
	}()

	// Step 1: Ensure channel exists, lock its row and read current_seq in one
	// statement. A separate INSERT followed by SELECT ... FOR UPDATE leaves a
	// window where the eviction loop's empty-channel cleanup deletes the row in
	// between, failing the publish (SPEC 7.4.6).
	var currentSeq int64
	err = tx.QueryRow(ctx,
		`INSERT INTO channels (name) VALUES ($1)
		 ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		 RETURNING current_seq`,
		channel,
	).Scan(&currentSeq)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("ensure and lock channel: %w", err)
	}

	// Step 2: Compute new seq.
	newSeq := currentSeq + 1

	// Step 3: Insert message.
	// When idempotency_key is NULL, the UNIQUE(channel, idempotency_key) constraint
	// does not fire (NULL != NULL in SQL), so ON CONFLICT is a no-op and INSERT always succeeds.
	// Messages written in cluster mode are stamped with this node's id so that
	// catch-up can skip rows already delivered inline by the publisher.
	var origin *string
	if s.nodeID != "" {
		origin = &s.nodeID
	}
	var seqID int64
	var createdAt time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO messages (channel, seq_id, payload, idempotency_key, origin_node)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (channel, idempotency_key) DO NOTHING
		 RETURNING seq_id, created_at`,
		channel, newSeq, payload, idempotencyKey, origin,
	).Scan(&seqID, &createdAt)

	if err != nil {
		if err == pgx.ErrNoRows {
			// Step 3b: Idempotency conflict — query the existing message.
			err = tx.QueryRow(ctx,
				`SELECT seq_id, created_at FROM messages WHERE channel = $1 AND idempotency_key = $2`,
				channel, *idempotencyKey,
			).Scan(&seqID, &createdAt)
			if err != nil {
				return 0, time.Time{}, fmt.Errorf("query idempotent message: %w", err)
			}
			// Skip step 5: do not increment seq.
			if err := tx.Commit(ctx); err != nil {
				return 0, time.Time{}, fmt.Errorf("commit idempotent read: %w", err)
			}
			return seqID, createdAt, nil
		}
		return 0, time.Time{}, fmt.Errorf("insert message: %w", err)
	}

	// Step 4: Notify other nodes from inside the transaction. PostgreSQL only
	// delivers the notification if this transaction commits, so a rolled-back
	// publish never produces an event. The idempotent-conflict branch returned
	// above and emits nothing (no new message).
	if s.nodeID != "" {
		event, err := EncodeMessageEvent(MessageEvent{NodeID: s.nodeID, Channel: channel, SeqID: seqID})
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("encode notify event: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, event); err != nil {
			return 0, time.Time{}, fmt.Errorf("notify message event: %w", err)
		}
	}

	// Step 5: Advance channel seq.
	_, err = tx.Exec(ctx,
		`UPDATE channels SET current_seq = current_seq + 1, updated_at = now() WHERE name = $1`,
		channel,
	)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("advance seq: %w", err)
	}

	// Step 6: Commit.
	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, fmt.Errorf("commit: %w", err)
	}

	return seqID, createdAt, nil
}

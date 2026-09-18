package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const maxHistoryLimit = 1000

// ReadHistory returns messages with seq_id > afterSeq for the given channel,
// ordered by seq_id ascending. Limit is capped at 1000.
// If the channel does not exist, returns an empty slice (not an error).
func (s *pgStore) ReadHistory(ctx context.Context, channel string, afterSeq int64, limit int) (*HistoryResult, error) {
	if err := ValidateChannelName(channel); err != nil {
		return nil, err
	}

	if limit <= 0 || limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	rows, err := s.pool.Query(ctx,
		`SELECT seq_id, payload, created_at, COALESCE(origin_node, '') FROM messages
		 WHERE channel = $1 AND seq_id > $2
		 ORDER BY seq_id ASC
		 LIMIT $3`,
		channel, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	result := &HistoryResult{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.SeqID, &m.Payload, &m.CreatedAt, &m.Origin); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		result.Messages = append(result.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}

	// MinSeq reflects the earliest available seq_id in the channel.
	// Always computed so Hub can perform gap detection even when no messages
	// are returned (e.g. afterSeq exceeds the channel's max seq_id).
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(MIN(seq_id), 0) FROM messages WHERE channel = $1`,
		channel,
	).Scan(&result.MinSeq)
	if err != nil {
		return nil, fmt.Errorf("query min_seq: %w", err)
	}

	return result, nil
}

// ReadMessage loads a single message by seq_id. It returns ErrMessageNotFound
// when the message does not exist (e.g. it was removed by the retention loop).
func (s *pgStore) ReadMessage(ctx context.Context, channel string, seqID int64) (*Message, error) {
	if err := ValidateChannelName(channel); err != nil {
		return nil, err
	}

	var m Message
	err := s.pool.QueryRow(ctx,
		`SELECT seq_id, payload, created_at, COALESCE(origin_node, '') FROM messages WHERE channel = $1 AND seq_id = $2`,
		channel, seqID,
	).Scan(&m.SeqID, &m.Payload, &m.CreatedAt, &m.Origin)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, fmt.Errorf("read message: %w", err)
	}
	return &m, nil
}

// LatestSeq returns the channel's current (highest allocated) seq_id, or 0 for
// a channel that does not exist.
func (s *pgStore) LatestSeq(ctx context.Context, channel string) (int64, error) {
	if err := ValidateChannelName(channel); err != nil {
		return 0, err
	}

	var seq int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT current_seq FROM channels WHERE name = $1), 0)`,
		channel,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("latest seq: %w", err)
	}
	return seq, nil
}

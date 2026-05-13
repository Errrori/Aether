package store

import (
	"context"
	"fmt"
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
		`SELECT seq_id, payload, created_at FROM messages
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
		if err := rows.Scan(&m.SeqID, &m.Payload, &m.CreatedAt); err != nil {
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

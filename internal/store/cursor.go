package store

import (
	"context"
	"fmt"
	"time"
)

// CursorStore defines persistence operations for subscriber acknowledgment
// cursors. Implemented by *pgStore alongside the Store interface.
type CursorStore interface {
	// LoadCursors returns persisted cursors for the subscriber on the given
	// channels. Channels without a row are omitted from the result.
	LoadCursors(ctx context.Context, subscriberID string, channels []string) (map[string]int64, error)
	// SaveCursors upserts batched cursors. The stored seq_id never moves
	// backwards (GREATEST), and updated_at is only refreshed when the cursor
	// actually advances so stale replays do not extend the cleanup TTL.
	SaveCursors(ctx context.Context, subscriberID string, cursors map[string]int64) error
	// DeleteStaleCursors removes rows whose updated_at is older than ttl and
	// returns the number of deleted rows.
	DeleteStaleCursors(ctx context.Context, ttl time.Duration) (int64, error)
}

// LoadCursors returns persisted cursors for the subscriber on the given
// channels. Channels without a row are omitted from the result.
func (s *pgStore) LoadCursors(ctx context.Context, subscriberID string, channels []string) (map[string]int64, error) {
	if len(channels) == 0 {
		return map[string]int64{}, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT channel, seq_id FROM subscriber_cursors
		 WHERE subscriber_id = $1 AND channel = ANY($2)`,
		subscriberID, channels)
	if err != nil {
		return nil, fmt.Errorf("load cursors: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int64, len(channels))
	for rows.Next() {
		var channel string
		var seq int64
		if err := rows.Scan(&channel, &seq); err != nil {
			return nil, fmt.Errorf("scan cursor: %w", err)
		}
		result[channel] = seq
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cursors: %w", err)
	}
	return result, nil
}

// SaveCursors upserts the given cursors in a single statement. seq_id is
// monotonically maxed; updated_at only advances when the cursor advances.
func (s *pgStore) SaveCursors(ctx context.Context, subscriberID string, cursors map[string]int64) error {
	if len(cursors) == 0 {
		return nil
	}

	channels := make([]string, 0, len(cursors))
	seqs := make([]int64, 0, len(cursors))
	for ch, seq := range cursors {
		channels = append(channels, ch)
		seqs = append(seqs, seq)
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO subscriber_cursors (subscriber_id, channel, seq_id)
		 SELECT $1, t.ch, t.seq
		 FROM unnest($2::text[], $3::bigint[]) AS t(ch, seq)
		 ON CONFLICT (subscriber_id, channel) DO UPDATE
		 SET seq_id = GREATEST(subscriber_cursors.seq_id, EXCLUDED.seq_id),
		     updated_at = CASE WHEN EXCLUDED.seq_id > subscriber_cursors.seq_id
		                       THEN now() ELSE subscriber_cursors.updated_at END`,
		subscriberID, channels, seqs)
	if err != nil {
		return fmt.Errorf("save cursors: %w", err)
	}
	return nil
}

// DeleteStaleCursors removes cursor rows that have not advanced for ttl.
func (s *pgStore) DeleteStaleCursors(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM subscriber_cursors WHERE updated_at < now() - make_interval(secs => $1)`,
		ttl.Seconds())
	if err != nil {
		return 0, fmt.Errorf("delete stale cursors: %w", err)
	}
	return tag.RowsAffected(), nil
}

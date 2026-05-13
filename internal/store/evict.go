package store

import (
	"context"
	"fmt"
)

// EvictExpiredMessages cleans up messages per channel based on retention rules.
// channelsCleaned counts channels that had at least one message evicted.
// Each DELETE is auto-committed (no transaction) so partial progress is preserved on error.
func (s *pgStore) EvictExpiredMessages(ctx context.Context) (int, int, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, current_seq FROM channels`)
	if err != nil {
		return 0, 0, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	type channelInfo struct {
		Name       string
		CurrentSeq int64
	}
	var channels []channelInfo
	for rows.Next() {
		var ch channelInfo
		if err := rows.Scan(&ch.Name, &ch.CurrentSeq); err != nil {
			return 0, 0, fmt.Errorf("scan channel: %w", err)
		}
		channels = append(channels, ch)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate channels: %w", err)
	}

	totalEvicted := 0
	totalCleaned := 0

	for _, ch := range channels {
		if err := ctx.Err(); err != nil {
			return totalCleaned, totalEvicted, fmt.Errorf("eviction cancelled: %w", err)
		}

		ttl, maxCount := s.ruleMatch(ch.Name)

		// TTL eviction.
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM messages WHERE channel = $1 AND created_at < now() - make_interval(secs => $2)`,
			ch.Name, ttl.Seconds(),
		)
		if err != nil {
			return totalCleaned, totalEvicted, fmt.Errorf("ttl evict channel %q: %w", ch.Name, err)
		}
		evicted := int(tag.RowsAffected())

		// Max-count eviction.
		threshold := ch.CurrentSeq - int64(maxCount)
		if threshold > 0 {
			tag, err = s.pool.Exec(ctx,
				`DELETE FROM messages WHERE channel = $1 AND seq_id <= $2`,
				ch.Name, threshold,
			)
			if err != nil {
				return totalCleaned, totalEvicted, fmt.Errorf("max_count evict channel %q: %w", ch.Name, err)
			}
			evicted += int(tag.RowsAffected())
		}

		if evicted > 0 {
			totalCleaned++
			totalEvicted += evicted
		}
	}

	// Clean up empty channels.
	_, err = s.pool.Exec(ctx,
		`DELETE FROM channels WHERE NOT EXISTS (SELECT 1 FROM messages WHERE messages.channel = channels.name)`,
	)
	if err != nil {
		return totalCleaned, totalEvicted, fmt.Errorf("clean empty channels: %w", err)
	}

	return totalCleaned, totalEvicted, nil
}

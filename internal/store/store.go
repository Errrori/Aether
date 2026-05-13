package store

import (
	"context"
	"encoding/json"
	"time"
)

// Message represents a single persisted message.
type Message struct {
	SeqID     int64
	Payload   json.RawMessage
	CreatedAt time.Time
}

// HistoryResult is returned by ReadHistory.
// MinSeq is the earliest available seq_id in the channel, used by Hub for gap detection.
type HistoryResult struct {
	Messages []Message
	MinSeq   int64
}

// Store is the persistence interface for Aether.
type Store interface {
	RunMigrations(ctx context.Context) error
	Ping(ctx context.Context) error
	WriteMessage(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (seqID int64, timestamp time.Time, err error)
	ReadHistory(ctx context.Context, channel string, afterSeq int64, limit int) (*HistoryResult, error)
	EvictExpiredMessages(ctx context.Context) (channelsCleaned int, messagesEvicted int, err error)
}

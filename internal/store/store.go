package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aether-mq/aether/internal/config"
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
	Close()
}

// ValidateChannelName checks that a channel name conforms to the Aether naming rules
// (1-128 chars, alphanumeric plus _ . / -, no consecutive dots).
func ValidateChannelName(name string) error {
	if len(name) < 1 || len(name) > 128 {
		return fmt.Errorf("channel name must be 1-128 characters")
	}
	if !config.ChannelNameRegex.MatchString(name) {
		return fmt.Errorf("invalid channel name: %q", name)
	}
	return nil
}

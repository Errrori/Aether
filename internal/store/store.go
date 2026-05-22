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

// KeyPermissions defines the channel-level access control for an API key.
// It is stored as JSONB in the api_keys table.
type KeyPermissions struct {
	Publish   []string `json:"publish"`
	Subscribe []string `json:"subscribe"`
	Admin     bool     `json:"admin"`
}

// APIKey represents a row in the api_keys table.
type APIKey struct {
	ID          string
	Name        string
	KeyHash     string
	KeyPrefix   string
	Permissions KeyPermissions
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

// KeyStore defines persistence operations for API keys.
// Implemented by *pgStore alongside the Store interface.
type KeyStore interface {
	CreateAPIKey(ctx context.Context, key *APIKey) error
	GetAPIKey(ctx context.Context, id string) (*APIKey, error)
	ListAPIKeys(ctx context.Context) ([]APIKey, error)
	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error
	RotateAPIKey(ctx context.Context, id string, newHash, newPrefix string) error
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

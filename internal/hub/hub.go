package hub

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/store"
)

var (
	ErrStoreNotSet   = errors.New("store not set")
	ErrNilConnection = errors.New("connection is nil")
)

// HubConfig holds Hub-specific configuration derived from config.Config.
type HubConfig struct {
	OutboundBufferSize      int
	MaxChannelsPerSubscribe int
	MaxChannelsPerConn      int
	HistoryLimit            int
}

// Hub is the core runtime component that manages channels, subscriptions,
// and message distribution.
type Hub interface {
	Publish(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (seqID int64, timestamp time.Time, err error)
	Subscribe(conn *Connection, channels []string, afterSeq map[string]int64) error
	Unsubscribe(conn *Connection, channels []string)
	RemoveConnection(conn *Connection)
}

type hubImpl struct {
	store   store.Store
	auth    auth.Auth
	metrics Metrics
	config  HubConfig

	mu       sync.RWMutex
	channels map[string]map[string]*Connection // channel -> connID -> *Connection

	connsMu sync.RWMutex
	conns   map[string]*Connection // connID -> *Connection

	activeChans atomic.Int64
}

// New creates a new Hub instance. cfg.OutboundBufferSize controls the capacity
// of each connection's Send channel. cfg.HistoryLimit caps messages per
// ReadHistory call. metrics may be NopMetrics() when instrumentation is
// not needed.
func New(s store.Store, a auth.Auth, cfg HubConfig, m Metrics) Hub {
	if cfg.OutboundBufferSize <= 0 {
		cfg.OutboundBufferSize = 256
	}
	if cfg.MaxChannelsPerSubscribe <= 0 {
		cfg.MaxChannelsPerSubscribe = 100
	}
	if cfg.MaxChannelsPerConn <= 0 {
		cfg.MaxChannelsPerConn = 1000
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 1000
	}
	return &hubImpl{
		store:    s,
		auth:     a,
		metrics:  m,
		config:   cfg,
		channels: make(map[string]map[string]*Connection),
		conns:    make(map[string]*Connection),
	}
}

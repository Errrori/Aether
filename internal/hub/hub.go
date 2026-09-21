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
	// NodeID is non-empty in cluster mode. It enables the node-level delivery
	// cursor and the cluster delivery methods (DeliverRemote / CatchUp), and
	// identifies messages published by this node (origin_node).
	NodeID string
	// AckFlushInterval is how often pending acknowledgment cursors are
	// persisted. Defaults to 1s when not positive.
	AckFlushInterval time.Duration
}

// SubscribeOptions carries subscribe-time options.
// AfterSeq takes precedence over Resume per channel.
type SubscribeOptions struct {
	// AfterSeq is the explicit replay anchor per channel: a value of zero
	// replays from the earliest available message. A missing entry means no
	// catch-up unless Resume is set.
	AfterSeq map[string]int64
	// Resume replays from the subscriber's persisted acknowledgment cursor
	// for channels without an explicit AfterSeq entry. Channels without a
	// cursor behave as if Resume were false (live only).
	Resume bool
}

// Hub is the core runtime component that manages channels, subscriptions,
// and message distribution.
type Hub interface {
	Publish(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (seqID int64, timestamp time.Time, err error)
	Subscribe(conn *Connection, channels []string, opts SubscribeOptions) error
	Unsubscribe(conn *Connection, channels []string)
	RemoveConnection(conn *Connection)
	Ack(conn *Connection, acks map[string]int64)
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

	// cursorMu guards nodeCursors: per-channel high-water mark of seqs
	// accounted for by the cluster listener path (delivered or skipped as
	// evicted). Lock order: h.mu may be held while acquiring cursorMu, never
	// the other way around.
	cursorMu    sync.Mutex
	nodeCursors map[string]int64

	// cursorStore is the optional persistence layer for subscriber
	// acknowledgment cursors (nil = in-memory only, e.g. unit tests).
	cursorStore store.CursorStore

	// pendingMu guards pending: unflushed acknowledgment cursors keyed by
	// subscriber and channel. Ack writers only raise the value (max); the
	// flusher goroutine is the single writer to the store.
	pendingMu   sync.Mutex
	pending     map[pendingCursorKey]int64
	flushSignal chan struct{}
	flushWG     sync.WaitGroup

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
	if cfg.AckFlushInterval <= 0 {
		cfg.AckFlushInterval = time.Second
	}
	h := &hubImpl{
		store:       s,
		auth:        a,
		metrics:     m,
		config:      cfg,
		channels:    make(map[string]map[string]*Connection),
		conns:       make(map[string]*Connection),
		nodeCursors: make(map[string]int64),
		pending:     make(map[pendingCursorKey]int64),
		flushSignal: make(chan struct{}, 1),
	}
	if cs, ok := s.(store.CursorStore); ok {
		h.cursorStore = cs
	}
	return h
}

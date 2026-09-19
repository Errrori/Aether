// Package cluster implements the cross-node fan-out receiver: a dedicated
// PostgreSQL LISTEN connection that feeds notifications into the local hub.
package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aether-mq/aether/internal/store"
	"github.com/jackc/pgx/v5"
)

// Deliverer is implemented by the hub. The cluster package never imports the
// hub: deliveries go through this interface.
type Deliverer interface {
	// HasSubscribers reports whether any local connection is subscribed to the
	// channel. False lets the listener skip the read-back entirely.
	HasSubscribers(channel string) bool
	// DeliverRemote reads the message back from the store and fans it out to
	// local subscribers.
	DeliverRemote(ctx context.Context, channel string, seqID int64) error
	// CatchUp re-delivers messages published while the LISTEN connection was
	// down. It runs before any queued notification is consumed.
	CatchUp(ctx context.Context) error
}

// Config configures a Listener.
type Config struct {
	// DSN of the shared database. The listener dials its own dedicated
	// connection: LISTEN is session state and must not come from a pool.
	DSN string
	// NodeID identifies this node; notifications carrying it are skipped.
	NodeID string
	// ReconnectBase and ReconnectMax bound the exponential reconnect backoff.
	ReconnectBase time.Duration
	ReconnectMax  time.Duration
}

// Listener maintains the LISTEN connection and dispatches notifications.
type Listener struct {
	cfg    Config
	d      Deliverer
	logger *slog.Logger
}

// New creates a Listener. Zero backoff values default to 1s/30s.
func New(cfg Config, d Deliverer, logger *slog.Logger) *Listener {
	if cfg.ReconnectBase <= 0 {
		cfg.ReconnectBase = time.Second
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Listener{cfg: cfg, d: d, logger: logger}
}

// Run blocks until ctx is cancelled, keeping the LISTEN connection alive and
// reconnecting with exponential backoff on failure. It returns nil on
// cancellation; failures are logged and retried, never returned, so a
// transient database outage cannot take the node down.
func (l *Listener) Run(ctx context.Context) error {
	backoff := l.cfg.ReconnectBase

	for {
		if ctx.Err() != nil {
			return nil
		}

		established, err := l.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if established {
			// The connection worked; a fresh disconnect starts the backoff
			// over instead of inheriting the previous outage's growth.
			backoff = l.cfg.ReconnectBase
		}

		l.logger.Warn("cluster listener disconnected, reconnecting",
			"node_id", l.cfg.NodeID, "backoff", backoff, "err", err)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff, l.cfg.ReconnectMax)
	}
}

// runOnce establishes the connection and consumes notifications until an
// error occurs. It reports whether the connection was fully established
// (connected, LISTENed and caught up) before failing.
func (l *Listener) runOnce(ctx context.Context) (bool, error) {
	connCfg, err := pgx.ParseConfig(l.cfg.DSN)
	if err != nil {
		return false, fmt.Errorf("parse dsn: %w", err)
	}
	if connCfg.RuntimeParams == nil {
		connCfg.RuntimeParams = make(map[string]string)
	}
	connCfg.RuntimeParams["application_name"] = truncateAppName("aether-cluster-" + l.cfg.NodeID)

	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer func() {
		// A cancelled context poisons the connection, so Close must not reuse
		// it; the session (and its LISTEN state) ends when it is closed.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := conn.Close(closeCtx); err != nil {
			l.logger.Debug("close cluster connection", "err", err)
		}
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+store.NotifyChannel); err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}

	// Catch up before consuming notifications. LISTEN is already active, so
	// messages published during catch-up queue up on this connection and are
	// processed afterwards; the hub's cursor dedups any overlap.
	if err := l.d.CatchUp(ctx); err != nil {
		return false, fmt.Errorf("catch-up: %w", err)
	}

	l.logger.Info("cluster listener connected", "node_id", l.cfg.NodeID, "channel", store.NotifyChannel)

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("wait for notification: %w", err)
		}
		if n == nil {
			continue
		}
		l.handleNotification(ctx, n.Payload)
	}
}

// handleNotification processes one notification payload. Failures are logged
// and never abort the listener loop.
func (l *Listener) handleNotification(ctx context.Context, payload string) {
	ev, err := store.DecodeMessageEvent(payload)
	if err != nil {
		l.logger.Warn("discarding malformed cluster notification", "err", err)
		return
	}
	if ev.NodeID == l.cfg.NodeID {
		return // our own publish, already fanned out inline
	}
	if !l.d.HasSubscribers(ev.Channel) {
		return // no local subscribers: skip the read-back entirely
	}
	if err := l.d.DeliverRemote(ctx, ev.Channel, ev.SeqID); err != nil {
		l.logger.Warn("remote delivery failed", "channel", ev.Channel, "seq", ev.SeqID, "err", err)
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// truncateAppName keeps application_name within PostgreSQL's 63-byte limit.
func truncateAppName(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}

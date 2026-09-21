package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aether-mq/aether/internal/api"
	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/cluster"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/keymgmt"
	"github.com/aether-mq/aether/internal/metrics"
	"github.com/aether-mq/aether/internal/ratelimit"
	"github.com/aether-mq/aether/internal/store"
	"github.com/aether-mq/aether/internal/webhook"
	"github.com/aether-mq/aether/internal/ws"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	// 1. Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// 2. Set up structured logging before any component initializes.
	setupLogging(cfg.Log)
	slog.Info("configuration loaded", "file", *configPath)

	// 3. Signal handling early — allows interrupting a hung DB connection.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// 3a. Cluster mode: resolve the node identity before the store is created
	// so the store (origin stamping + notifications) and the listener share it.
	// Empty nodeID keeps every cluster path disabled.
	var nodeID string
	if cfg.Cluster.Enabled {
		nodeID = cfg.Cluster.NodeID
		if nodeID == "" {
			generated, err := generateNodeID()
			if err != nil {
				return fmt.Errorf("generate node id: %w", err)
			}
			nodeID = generated
		}
		slog.Info("cluster mode enabled", "node_id", nodeID)
	}

	// 4. Storage engine.
	slog.Info("connecting to database")
	var storeOpts []store.Options
	if nodeID != "" {
		storeOpts = append(storeOpts, store.Options{NodeID: nodeID})
	}
	st, err := store.New(ctx, &cfg.Database, &cfg.Retention, storeOpts...)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer st.Close()

	slog.Info("running database migrations")
	if err := st.RunMigrations(ctx); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	// 5. Auth.
	ks, ok := st.(store.KeyStore)
	if !ok {
		return fmt.Errorf("store does not implement KeyStore")
	}
	if err := auth.BootstrapConfigKeys(ctx, ks, cfg.Auth.APIKeys); err != nil {
		return fmt.Errorf("bootstrap config keys: %w", err)
	}
	au, err := auth.New(&cfg.Auth, ks)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	slog.Info("auth module ready")

	// 6. Metrics (must be before Hub so callbacks are wired).
	m := metrics.New()

	// 7. Hub.
	hubCfg := hub.HubConfig{
		OutboundBufferSize:      cfg.WebSocket.OutboundBuffer,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
		NodeID:                  nodeID,
	}
	h := hub.New(st, au, hubCfg, m)
	slog.Info("hub ready")

	// 7a. Cluster listener (v2 layer 3): consumes cross-node notifications and
	// feeds them into the hub. Stopped before the HTTP server drains.
	clusterCtx, clusterCancel := context.WithCancel(context.Background())
	defer clusterCancel()

	var clusterDone sync.WaitGroup
	if cfg.Cluster.Enabled {
		deliverer, ok := h.(cluster.Deliverer)
		if !ok {
			return fmt.Errorf("hub does not implement cluster.Deliverer")
		}
		listener := cluster.New(cluster.Config{
			DSN:           cfg.Database.DSN,
			NodeID:        nodeID,
			ReconnectBase: cfg.Cluster.ReconnectBase,
			ReconnectMax:  cfg.Cluster.ReconnectMax,
		}, deliverer, slog.Default())

		clusterDone.Add(1)
		go func() {
			defer clusterDone.Done()
			if err := listener.Run(clusterCtx); err != nil {
				slog.Warn("cluster listener stopped", "err", err)
			}
		}()
		slog.Info("cluster listener started")
	}

	// 7b. Acknowledgment cursor flusher: aggregates acks in memory and
	// persists them in batches. Stopped after the WebSocket drain so the
	// final flush runs before the deferred store close.
	hubFlusher, ok := h.(interface {
		Start(context.Context)
		Wait()
	})
	if !ok {
		return fmt.Errorf("hub does not implement the cursor flusher lifecycle")
	}
	hubFlushCtx, hubFlushCancel := context.WithCancel(context.Background())
	defer hubFlushCancel()
	hubFlusher.Start(hubFlushCtx)

	// 8. WebSocket manager.
	wsm := ws.NewManager(h, au, cfg.WebSocket)
	slog.Info("websocket manager ready")

	// 9. Key management.
	km := keymgmt.New(ks)

	// 9a. Webhook manager (v2 layer 2).
	whStore, ok := st.(store.WebhookStore)
	if !ok {
		return fmt.Errorf("store does not implement WebhookStore")
	}
	whm := webhook.New(whStore, h, slog.Default())
	slog.Info("webhook manager ready")

	// 9b. Rate limiter (v2 layer 4): in-process token buckets for the HTTP
	// publish entrypoints. Disabled by default — no buckets, no sweeper.
	var limiter *ratelimit.Limiter
	limiterCtx, limiterCancel := context.WithCancel(context.Background())
	defer limiterCancel()
	if cfg.RateLimit.Enabled {
		limiter = ratelimit.New(ratelimit.Config{
			Publisher: ratelimit.RateConfig{
				Rate:  cfg.RateLimit.Publisher.Rate,
				Burst: cfg.RateLimit.Publisher.Burst,
			},
			Channel: ratelimit.RateConfig{
				Rate:  cfg.RateLimit.Channel.Rate,
				Burst: cfg.RateLimit.Channel.Burst,
			},
			OnRejected: metrics.NewRateLimitRejected(),
		}, slog.Default())
		limiter.Start(limiterCtx)
		slog.Info("rate limiting enabled",
			"publisher_rate", cfg.RateLimit.Publisher.Rate,
			"publisher_burst", cfg.RateLimit.Publisher.Burst,
			"channel_rate", cfg.RateLimit.Channel.Rate,
			"channel_burst", cfg.RateLimit.Channel.Burst)
	}

	// 10. HTTP API server.
	apiCfg := api.ServerConfig{
		MaxPayloadSize: cfg.Server.MaxPayloadSize,
	}
	if limiter != nil {
		apiCfg.RateLimiter = limiter
	}
	srv := api.New(h, au, st, km, ks, whm, wsm, apiCfg)

	// 10. Background tasks: eviction loop. In cluster mode the loop first
	// acquires the leader lock so exactly one node evicts per cycle. Cursor
	// cleanup runs inside the same leader-gated cycle.
	var evictLeader store.LeaderStore
	if cfg.Cluster.Enabled {
		ls, ok := st.(store.LeaderStore)
		if !ok {
			return fmt.Errorf("store does not implement LeaderStore")
		}
		evictLeader = ls
	}

	var cursorStore store.CursorStore
	if cs, ok := st.(store.CursorStore); ok {
		cursorStore = cs
	}

	evictCtx, evictCancel := context.WithCancel(context.Background())
	defer evictCancel()

	var evictDone sync.WaitGroup
	evictDone.Add(1)
	go runEvictionLoop(evictCtx, st, evictLeader, cursorStore, cfg.Ack.CursorTTL, cfg.Retention.EvictionInterval, &evictDone)

	// 11. Start the HTTP server in a goroutine.
	serverErr := make(chan error, 1)
	go func() {
		if cfg.Server.TLSCert != "" {
			slog.Info("starting TLS server", "addr", cfg.Server.Addr)
			serverErr <- srv.ListenAndServeTLS(cfg.Server.Addr, cfg.Server.TLSCert, cfg.Server.TLSKey)
		} else {
			slog.Info("starting server", "addr", cfg.Server.Addr)
			serverErr <- srv.ListenAndServe(cfg.Server.Addr)
		}
	}()

	// 12. Wait for a signal or a server startup error.
	select {
	case err = <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: %w", err)
		}
		slog.Info("server stopped")
	case <-ctx.Done():
		slog.Info("received signal, initiating graceful shutdown")
	}

	// 13. Shutdown sequence: background tasks finish before the server drain
	// and before the deferred store close.
	evictCancel()
	evictDone.Wait()

	clusterCancel()
	clusterDone.Wait()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
	defer shutdownCancel()

	slog.Info("shutting down", "timeout", cfg.Shutdown.Timeout)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown incomplete", "err", err)
	}

	// Stop the rate limiter sweeper after the HTTP drain; Allow stays usable
	// during the drain and does not depend on the sweeper.
	limiterCancel()
	if limiter != nil {
		limiter.Wait()
	}

	// Stop the cursor flusher after the WebSocket drain (its last acks) and
	// before the deferred store close so the final flush can persist.
	hubFlushCancel()
	hubFlusher.Wait()
	slog.Info("shutdown complete")
	return nil
}

func setupLogging(cfg config.LogConfig) {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(h))
}

func runEvictionLoop(ctx context.Context, s store.Store, leader store.LeaderStore, cursorStore store.CursorStore, cursorTTL, interval time.Duration, done *sync.WaitGroup) {
	defer done.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("eviction loop stopped")
			return
		case <-ticker.C:
			runEvictionCycle(ctx, s, leader, cursorStore, cursorTTL)
		}
	}
}

// runEvictionCycle runs one eviction round. In cluster mode (leader != nil) it
// first acquires the leader lock, so nodes that lose the race skip the round
// instead of evicting concurrently.
func runEvictionCycle(ctx context.Context, s store.Store, leader store.LeaderStore, cursorStore store.CursorStore, cursorTTL time.Duration) {
	if leader != nil {
		lock, err := leader.TryEvictionLock(ctx)
		if err != nil {
			slog.Warn("eviction leader lock failed", "err", err)
			return
		}
		if lock == nil {
			slog.Debug("eviction skipped: another node holds the leader lock")
			return
		}
		defer func() {
			if err := lock.Release(context.Background()); err != nil {
				slog.Warn("eviction leader lock release failed", "err", err)
			}
		}()
	}

	channels, msgs, err := s.EvictExpiredMessages(ctx)
	if err != nil {
		slog.Warn("eviction cycle failed", "err", err)
	} else if channels > 0 || msgs > 0 {
		slog.Info("eviction completed", "channels_cleaned", channels, "messages_evicted", msgs)
	}

	// Stale cursor cleanup runs regardless of the message eviction outcome:
	// both steps are independent and a failure in one must not skip the other.
	if cursorStore != nil {
		removed, err := cursorStore.DeleteStaleCursors(ctx, cursorTTL)
		if err != nil {
			slog.Warn("cursor cleanup failed", "err", err)
		} else if removed > 0 {
			slog.Info("cursor cleanup completed", "cursors_removed", removed)
		}
	}
}

func generateNodeID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

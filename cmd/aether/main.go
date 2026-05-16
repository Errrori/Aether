package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aether-mq/aether/internal/api"
	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/metrics"
	"github.com/aether-mq/aether/internal/store"
	"github.com/aether-mq/aether/internal/ws"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	// 1. Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load error: %v\n", err)
		os.Exit(1)
	}

	// 2. Set up structured logging before any component initializes.
	setupLogging(cfg.Log)
	slog.Info("configuration loaded", "file", *configPath)

	// 3. Signal handling early — allows interrupting a hung DB connection.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// 4. Storage engine.
	slog.Info("connecting to database")
	st, err := store.New(ctx, &cfg.Database, &cfg.Retention)
	if err != nil {
		slog.Error("database connection failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	slog.Info("running database migrations")
	if err := st.RunMigrations(ctx); err != nil {
		slog.Error("migrations failed", "err", err)
		os.Exit(1)
	}

	// 5. Auth.
	au, err := auth.New(&cfg.Auth)
	if err != nil {
		slog.Error("auth init failed", "err", err)
		os.Exit(1)
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
	}
	h := hub.New(st, au, hubCfg, m)
	slog.Info("hub ready")

	// 8. WebSocket manager.
	wsm := ws.NewManager(h, au, cfg.WebSocket)
	slog.Info("websocket manager ready")

	// 9. HTTP API server.
	apiCfg := api.ServerConfig{
		MaxPayloadSize: cfg.Server.MaxPayloadSize,
	}
	srv := api.New(h, au, st, wsm, apiCfg)

	// 10. Background tasks: eviction loop.
	evictCtx, evictCancel := context.WithCancel(context.Background())
	defer evictCancel()
	go runEvictionLoop(evictCtx, st, cfg.Retention.EvictionInterval)

	// 11. Start the HTTP server (blocks until shutdown).
	if cfg.Server.TLSCert != "" {
		slog.Info("starting TLS server", "addr", cfg.Server.Addr)
		err = srv.ListenAndServeTLS(cfg.Server.Addr, cfg.Server.TLSCert, cfg.Server.TLSKey)
	} else {
		slog.Info("starting server", "addr", cfg.Server.Addr)
		err = srv.ListenAndServe(cfg.Server.Addr)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped unexpectedly", "err", err)
	}
	slog.Info("server stopped accepting connections")

	// 12. Shutdown sequence.
	evictCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
	defer shutdownCancel()

	slog.Info("shutting down", "timeout", cfg.Shutdown.Timeout)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown incomplete", "err", err)
	}
	slog.Info("shutdown complete")
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

func runEvictionLoop(ctx context.Context, s store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("eviction loop stopped")
			return
		case <-ticker.C:
			channels, msgs, err := s.EvictExpiredMessages(ctx)
			if err != nil {
				slog.Warn("eviction cycle failed", "err", err)
			} else if channels > 0 || msgs > 0 {
				slog.Info("eviction completed", "channels_cleaned", channels, "messages_evicted", msgs)
			}
		}
	}
}

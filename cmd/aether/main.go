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
	"sync"
	"syscall"
	"time"

	"github.com/aether-mq/aether/internal/api"
	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/keymgmt"
	"github.com/aether-mq/aether/internal/metrics"
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

	// 4. Storage engine.
	slog.Info("connecting to database")
	st, err := store.New(ctx, &cfg.Database, &cfg.Retention)
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
	}
	h := hub.New(st, au, hubCfg, m)
	slog.Info("hub ready")

	// 8. WebSocket manager.
	wsm := ws.NewManager(h, au, cfg.WebSocket)
	slog.Info("websocket manager ready")

	// 9. Key management (v2).
	km := keymgmt.New(ks)

	// 9a. Webhook manager (v2 layer 2).
	whStore, ok := st.(store.WebhookStore)
	if !ok {
		return fmt.Errorf("store does not implement WebhookStore")
	}
	whm := webhook.New(whStore, h, slog.Default())
	slog.Info("webhook manager ready")

	// 10. HTTP API server.
	apiCfg := api.ServerConfig{
		MaxPayloadSize: cfg.Server.MaxPayloadSize,
	}
	srv := api.New(h, au, st, km, ks, whm, wsm, apiCfg)

	// 11. Background tasks: eviction loop.
	evictCtx, evictCancel := context.WithCancel(context.Background())
	defer evictCancel()

	var evictDone sync.WaitGroup
	evictDone.Add(1)
	go runEvictionLoop(evictCtx, st, cfg.Retention.EvictionInterval, &evictDone)

	// 12. Start the HTTP server in a goroutine.
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

	// 13. Shutdown sequence.
	evictCancel()
	evictDone.Wait()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
	defer shutdownCancel()

	slog.Info("shutting down", "timeout", cfg.Shutdown.Timeout)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown incomplete", "err", err)
	}
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

func runEvictionLoop(ctx context.Context, s store.Store, interval time.Duration, done *sync.WaitGroup) {
	defer done.Done()

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

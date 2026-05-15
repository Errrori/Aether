package api

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type ServerConfig struct {
	MaxPayloadSize int
}

type Server struct {
	hub   hub.Hub
	auth  auth.Auth
	store store.Store
	cfg   ServerConfig
	ready atomic.Bool
	srv   *http.Server
}

func New(h hub.Hub, a auth.Auth, s store.Store, cfg ServerConfig) *Server {
	if cfg.MaxPayloadSize <= 0 {
		cfg.MaxPayloadSize = 65536
	}
	return &Server{
		hub:   h,
		auth:  a,
		store: s,
		cfg:   cfg,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/publish", s.authMiddleware(s.handlePublish))
	mux.HandleFunc("GET /api/v1/history", s.authMiddleware(s.handleHistory))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metricsz", promhttp.Handler().ServeHTTP)

	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	s.srv = &http.Server{Addr: addr, Handler: s.Handler()}
	s.ready.Store(true)
	defer s.ready.Store(false)
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.ready.Store(false)
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

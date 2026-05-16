package ws

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/coder/websocket"
)

// Manager handles WebSocket upgrade, connection lifecycle, and graceful shutdown.
type Manager struct {
	hub  hub.Hub
	auth auth.Auth
	cfg  config.WebSocketConfig

	mu       sync.Mutex
	conns    map[string]*activeConn
	draining bool
	wg       sync.WaitGroup
}

type activeConn struct {
	id     string
	wsConn *websocket.Conn
	hubCnn *hub.Connection
}

// NewManager creates a Manager.
func NewManager(h hub.Hub, a auth.Auth, cfg config.WebSocketConfig) *Manager {
	return &Manager{
		hub:   h,
		auth:  a,
		cfg:   cfg,
		conns: make(map[string]*activeConn),
	}
}

// ServeHTTP handles a WebSocket upgrade request.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()

	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	claims, err := m.auth.ParseAndValidateToken(token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	if !m.checkOrigin(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Default().Info("websocket accept failed", "err", err)
		return
	}

	connID := generateConnID()
	hubCnn := hub.NewConnection(connID, claims.Subject, claims, m.cfg.OutboundBuffer)
	hubCnn.Overflow = func() {
		wsConn.Close(websocket.StatusServiceRestart, "buffer full")
	}

	ac := &activeConn{
		id:     connID,
		wsConn: wsConn,
		hubCnn: hubCnn,
	}

	m.mu.Lock()
	m.conns[connID] = ac
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.conns, connID)
		m.mu.Unlock()
		m.hub.RemoveConnection(hubCnn)
	}()

	var loopsWg sync.WaitGroup
	loopsWg.Add(3)

	go readLoop(ac, m.hub, m.cfg.MaxMessageSize, &loopsWg)
	go writeLoop(ac, &loopsWg)
	go heartbeatLoop(ac, m.cfg.PingInterval, m.cfg.PongTimeout, &loopsWg)

	loopsWg.Wait()
}

// Shutdown gracefully shuts down all WebSocket connections.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.draining = true
	m.mu.Unlock()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Close any connections currently in the map. This runs in a loop
		// because ServeHTTP may register new connections (that passed the
		// draining check before it was set) in the time between the snapshot
		// and these connections completing their upgrade.
		m.mu.Lock()
		for _, ac := range m.conns {
			ac.wsConn.Close(websocket.StatusGoingAway, "server shutting down")
		}
		m.mu.Unlock()

		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			return nil
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) checkOrigin(origin string) bool {
	if len(m.cfg.AllowedOrigins) == 0 {
		return false
	}
	if origin == "" {
		return true
	}
	for _, allowed := range m.cfg.AllowedOrigins {
		if allowed == "*" {
			return true
		}
		if allowed == origin {
			return true
		}
	}
	return false
}

func generateConnID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

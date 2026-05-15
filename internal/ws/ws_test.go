package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/coder/websocket"
)

// --- mock auth ---

type mockAuth struct {
	validToken string
}

func (a *mockAuth) ValidateAPIKey(key string) bool { return false }

func (a *mockAuth) ParseAndValidateToken(tokenString string) (*auth.Claims, error) {
	if tokenString == a.validToken {
		return &auth.Claims{Subject: "test-user", Channels: []string{"*"}}, nil
	}
	if tokenString == "expired-token" {
		return nil, auth.ErrTokenExpired
	}
	return nil, auth.ErrInvalidToken
}

func (a *mockAuth) IsChannelAuthorized(claims *auth.Claims, channel string) bool { return true }

// --- mock hub ---

type mockHub struct {
	mu          sync.Mutex
	subscribers map[string][]string // connID -> subscribed channels
	removed     chan string         // optional: receives connID on RemoveConnection
}

func newMockHub() *mockHub {
	return &mockHub{subscribers: make(map[string][]string)}
}

func newMockHubWithRemoved() *mockHub {
	return &mockHub{
		subscribers: make(map[string][]string),
		removed:     make(chan string, 1),
	}
}

func (h *mockHub) Publish(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (int64, time.Time, error) {
	return 0, time.Time{}, nil
}

func (h *mockHub) Subscribe(conn *hub.Connection, channels []string, afterSeq map[string]int64) error {
	h.mu.Lock()
	h.subscribers[conn.ID] = channels
	h.mu.Unlock()

	frame, _ := hub.MarshalFrame(hub.SubscribedFrame{
		Type:     hub.FrameTypeSubscribed,
		Channels: channels,
	})
	select {
	case conn.Send <- frame:
	default:
	}
	return nil
}

func (h *mockHub) Unsubscribe(conn *hub.Connection, channels []string) {
	h.mu.Lock()
	delete(h.subscribers, conn.ID)
	h.mu.Unlock()

	frame, _ := hub.MarshalFrame(hub.UnsubscribedFrame{
		Type:     hub.FrameTypeUnsubscribed,
		Channels: channels,
	})
	select {
	case conn.Send <- frame:
	default:
	}
}

func (h *mockHub) RemoveConnection(conn *hub.Connection) {
	h.mu.Lock()
	delete(h.subscribers, conn.ID)
	h.mu.Unlock()
	conn.Close()
	if h.removed != nil {
		h.removed <- conn.ID
	}
}

// --- helpers ---

func testConfig() config.WebSocketConfig {
	return config.WebSocketConfig{
		PingInterval:   30 * time.Second,
		PongTimeout:    60 * time.Second,
		OutboundBuffer: 256,
		MaxMessageSize: 65536,
		AllowedOrigins: []string{"*"},
	}
}

func wsURL(httpURL, token string) string {
	return strings.Replace(httpURL, "http://", "ws://", 1) + "?token=" + token
}

func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return v
}

func writeJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// --- tests: HTTP auth / origin ---

func TestServeHTTP_ValidToken(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	if resp.StatusCode != 101 {
		t.Fatalf("expected status 101, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_MissingToken(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	url := strings.Replace(ts.URL, "http://", "ws://", 1)
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("expected dial error for missing token")
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_InvalidToken(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	_, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "bad-token"), nil)
	if err == nil {
		t.Fatal("expected dial error for invalid token")
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_ExpiredToken(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	_, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "expired-token"), nil)
	if err == nil {
		t.Fatal("expected dial error for expired token")
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_OriginNotAllowed(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{"https://example.com"}
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	_, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Origin": {"https://evil.com"},
		},
	})
	if err == nil {
		t.Fatal("expected dial error for disallowed origin")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 error, got: %v", err)
	}
}

func TestServeHTTP_OriginAllowed_ExactMatch(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{"https://example.com"}
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Origin": {"https://example.com"},
		},
	})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()
	if resp.StatusCode != 101 {
		t.Fatalf("expected status 101, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_OriginWildcard(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{"*"}
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Origin": {"https://any-origin.com"},
		},
	})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()
	if resp.StatusCode != 101 {
		t.Fatalf("expected status 101, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_NoOrigin_NonBrowser(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{"https://example.com"}
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	// No Origin header → allow (non-browser client)
	conn, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()
	if resp.StatusCode != 101 {
		t.Fatalf("expected status 101, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_EmptyAllowedOrigins_DeniesAll(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{}
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	// Even without Origin header, empty AllowedOrigins denies all
	_, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err == nil {
		t.Fatal("expected dial error with empty AllowedOrigins")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 error, got: %v", err)
	}
}

// --- tests: draining ---

func TestServeHTTP_Draining(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	mgr.mu.Lock()
	mgr.draining = true
	mgr.mu.Unlock()

	_, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err == nil {
		t.Fatal("expected dial error when draining")
	}
	if resp.StatusCode != 503 {
		t.Fatalf("expected status 503, got %d", resp.StatusCode)
	}
}

// --- tests: client frames ---

func TestReadLoop_Subscribe(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	writeJSON(t, conn, map[string]any{
		"type":     "subscribe",
		"channels": []string{"ch1", "ch2"},
	})

	frame := readFrame(t, conn)
	if frame["type"] != "subscribed" {
		t.Fatalf("expected subscribed, got %v", frame["type"])
	}
	channels, ok := frame["channels"].([]any)
	if !ok {
		t.Fatal("expected channels array in subscribed frame")
	}
	if len(channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(channels))
	}
}

func TestReadLoop_SubscribeWithAfterSeq(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	writeJSON(t, conn, map[string]any{
		"type":      "subscribe",
		"channels":  []string{"ch1"},
		"after_seq": map[string]int64{"ch1": 42},
	})

	frame := readFrame(t, conn)
	if frame["type"] != "subscribed" {
		t.Fatalf("expected subscribed, got %v", frame["type"])
	}
}

func TestReadLoop_Unsubscribe(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Subscribe first
	writeJSON(t, conn, map[string]any{
		"type":     "subscribe",
		"channels": []string{"ch1"},
	})
	readFrame(t, conn) // consume subscribed ack

	// Then unsubscribe
	writeJSON(t, conn, map[string]any{
		"type":     "unsubscribe",
		"channels": []string{"ch1"},
	})

	frame := readFrame(t, conn)
	if frame["type"] != "unsubscribed" {
		t.Fatalf("expected unsubscribed, got %v", frame["type"])
	}
}

func TestReadLoop_UnknownFrame(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	writeJSON(t, conn, map[string]any{
		"type": "unknown_type",
	})

	frame := readFrame(t, conn)
	if frame["type"] != "error" {
		t.Fatalf("expected error frame, got %v", frame["type"])
	}
	code := frame["code"].(float64)
	if code != float64(hub.ErrCodeUnknownFrame) {
		t.Fatalf("expected code %d, got %v", hub.ErrCodeUnknownFrame, code)
	}
}

func TestReadLoop_InvalidJSON(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Send non-JSON data
	if err := conn.Write(context.Background(), websocket.MessageText, []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}

	frame := readFrame(t, conn)
	if frame["type"] != "error" {
		t.Fatalf("expected error frame, got %v", frame["type"])
	}
	code := frame["code"].(float64)
	if code != float64(hub.ErrCodeInvalidJSON) {
		t.Fatalf("expected code %d, got %v", hub.ErrCodeInvalidJSON, code)
	}
}

func TestReadLoop_MalformedSubscribeFrame(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Send subscribe with channels as string instead of array
	writeJSON(t, conn, map[string]any{
		"type":     "subscribe",
		"channels": "not-an-array",
	})

	frame := readFrame(t, conn)
	if frame["type"] != "error" {
		t.Fatalf("expected error frame, got %v", frame["type"])
	}
	code := frame["code"].(float64)
	if code != float64(hub.ErrCodeInvalidJSON) {
		t.Fatalf("expected code %d, got %v", hub.ErrCodeInvalidJSON, code)
	}
}

// --- tests: connection lifecycle ---

func TestConnection_ClientClose(t *testing.T) {
	cfg := testConfig()
	mh := newMockHubWithRemoved()
	mgr := NewManager(mh, &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	writeJSON(t, conn, map[string]any{
		"type":     "subscribe",
		"channels": []string{"ch1"},
	})
	readFrame(t, conn) // consume subscribed ack

	// Close client connection
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Wait for server-side cleanup via RemoveConnection
	select {
	case connID := <-mh.removed:
		if connID == "" {
			t.Fatal("expected non-empty connID from RemoveConnection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveConnection was not called within 2s")
	}
}

func TestConnection_OversizedFrame(t *testing.T) {
	cfg := testConfig()
	cfg.MaxMessageSize = 100
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Send a frame larger than the limit
	bigPayload := strings.Repeat("x", 200)
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(bigPayload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Server should close the connection; subsequent read should error
	_, _, err = conn.Read(context.Background())
	if err == nil {
		t.Fatal("expected read error after oversized frame")
	}
}

// --- tests: subscribe with empty channels ---

func TestReadLoop_SubscribeEmptyChannels(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	writeJSON(t, conn, map[string]any{
		"type":     "subscribe",
		"channels": []string{},
	})

	// Hub should still send subscribed ack with empty list
	frame := readFrame(t, conn)
	if frame["type"] != "subscribed" {
		t.Fatalf("expected subscribed, got %v", frame["type"])
	}
}

// --- tests: shutdown ---

func TestShutdown_CloseCode(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	var shutdownErr error
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr = mgr.Shutdown(ctx)
		close(done)
	}()

	// Client reads the close frame, which completes the close handshake.
	_, _, err = conn.Read(context.Background())
	if err == nil {
		t.Fatal("expected read error after shutdown close")
	}
	status := websocket.CloseStatus(err)
	if status != websocket.StatusGoingAway {
		t.Fatalf("expected close status 1001, got %d", status)
	}

	<-done
	if shutdownErr != nil {
		t.Fatalf("shutdown: %v", shutdownErr)
	}
}

func TestShutdown_RejectNewConnections(t *testing.T) {
	cfg := testConfig()
	mgr := NewManager(newMockHub(), &mockAuth{validToken: "valid-token"}, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	_, resp, err := websocket.Dial(context.Background(), wsURL(ts.URL, "valid-token"), nil)
	if err == nil {
		t.Fatal("expected dial error after shutdown")
	}
	if resp.StatusCode != 503 {
		t.Fatalf("expected status 503, got %d", resp.StatusCode)
	}
}

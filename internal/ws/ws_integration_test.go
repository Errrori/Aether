//go:build integration

package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/store"
	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
)

// --- helpers ---

func testDSN() string {
	if dsn := os.Getenv("AETHER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://aether:aether@localhost:5433/aether_test?sslmode=disable"
}

func integNewTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbCfg := &config.DatabaseConfig{
		DSN:             testDSN(),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxIdleTime: time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
	}
	retCfg := &config.RetentionConfig{
		DefaultTTL:       720 * time.Hour,
		DefaultMaxCount:  10000,
		EvictionInterval: 5 * time.Minute,
	}

	st, err := store.New(ctx, dbCfg, retCfg)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return st
}

func integSigningKey() string {
	return strings.Repeat("a", 32)
}

func integNewTestAuth(t *testing.T, ks store.KeyStore) auth.Auth {
	t.Helper()
	cfg := &config.AuthConfig{
		JWTSigningKey: integSigningKey(),
		JWTClockSkew:  30 * time.Second,
	}
	a, err := auth.New(cfg, ks)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return a
}

func integNewTestHub(t *testing.T, st store.Store, a auth.Auth) hub.Hub {
	t.Helper()
	cfg := hub.HubConfig{
		OutboundBufferSize:      256,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
	}
	return hub.New(st, a, cfg, hub.NopMetrics())
}

func integGenerateToken(t *testing.T, subject string, channels []string, expiry time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":      subject,
		"channels": channels,
		"iat":      time.Now().Unix(),
	}
	if expiry != 0 {
		claims["exp"] = time.Now().Add(expiry).Unix()
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(integSigningKey()))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func integNewTestServer(t *testing.T) (*Manager, *httptest.Server, hub.Hub, store.Store) {
	t.Helper()
	st := integNewTestStore(t)
	a := integNewTestAuth(t, st.(store.KeyStore))
	h := integNewTestHub(t, st, a)
	cfg := config.WebSocketConfig{
		PingInterval:   30 * time.Second,
		PongTimeout:    60 * time.Second,
		OutboundBuffer: 256,
		MaxMessageSize: 65536,
		AllowedOrigins: []string{"*"},
	}
	mgr := NewManager(h, a, cfg)
	ts := httptest.NewServer(mgr)
	t.Cleanup(func() { ts.Close() })
	return mgr, ts, h, st
}

func integWSURL(httpURL, token string) string {
	return strings.Replace(httpURL, "http://", "ws://", 1) + "?token=" + token
}

func integReadFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return v
}

func integAssertNoFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("expected no frame but received one")
	}
}

func integWriteJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func integDial(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.Dial(context.Background(), integWSURL(ts.URL, token), nil)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, resp.StatusCode)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("expected status 101, got %d", resp.StatusCode)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func integSubscribe(t *testing.T, conn *websocket.Conn, channels []string, afterSeq map[string]int64) map[string]any {
	t.Helper()
	req := map[string]any{
		"type":     "subscribe",
		"channels": channels,
	}
	if afterSeq != nil {
		req["after_seq"] = afterSeq
	}
	integWriteJSON(t, conn, req)
	return integReadFrame(t, conn, 5*time.Second)
}

func integUnsubscribe(t *testing.T, conn *websocket.Conn, channels []string) map[string]any {
	t.Helper()
	integWriteJSON(t, conn, map[string]any{
		"type":     "unsubscribe",
		"channels": channels,
	})
	return integReadFrame(t, conn, 5*time.Second)
}

func integPublish(t *testing.T, h hub.Hub, channel string, payload json.RawMessage) int64 {
	t.Helper()
	seqID, _, err := h.Publish(context.Background(), channel, payload, nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return seqID
}

// --- auth tests ---

func TestIntegration_Auth_ValidJWT(t *testing.T) {
	_, ts, _, _ := integNewTestServer(t)
	token := integGenerateToken(t, "test-sub", []string{"*"}, time.Hour)

	conn, resp, err := websocket.Dial(context.Background(), integWSURL(ts.URL, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if resp.StatusCode != 101 {
		t.Fatalf("expected status 101, got %d", resp.StatusCode)
	}
}

func TestIntegration_Auth_InvalidSignature(t *testing.T) {
	_, ts, _, _ := integNewTestServer(t)
	// Sign with a different key than integSigningKey()
	claims := jwt.MapClaims{
		"sub":      "test-sub",
		"channels": []string{"*"},
		"exp":      time.Now().Add(time.Hour).Unix(),
		"iat":      time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(strings.Repeat("b", 32)))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	_, resp, err := websocket.Dial(context.Background(), integWSURL(ts.URL, signed), nil)
	if err == nil {
		t.Fatal("expected dial error for invalid signature")
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestIntegration_Auth_ExpiredToken(t *testing.T) {
	_, ts, _, _ := integNewTestServer(t)
	token := integGenerateToken(t, "test-sub", []string{"*"}, -time.Hour)

	_, resp, err := websocket.Dial(context.Background(), integWSURL(ts.URL, token), nil)
	if err == nil {
		t.Fatal("expected dial error for expired token")
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestIntegration_Auth_TokenWithoutChannels(t *testing.T) {
	_, ts, _, _ := integNewTestServer(t)
	token := integGenerateToken(t, "test-sub", []string{}, time.Hour)

	conn, resp, err := websocket.Dial(context.Background(), integWSURL(ts.URL, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if resp.StatusCode != 101 {
		t.Fatalf("expected status 101, got %d", resp.StatusCode)
	}
}

// --- subscribe & messaging tests ---

func TestIntegration_Subscribe_RealTimeMessage(t *testing.T) {
	_, ts, h, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)
	conn := integDial(t, ts, token)

	frame := integSubscribe(t, conn, []string{ch}, nil)
	if frame["type"] != "subscribed" {
		t.Fatalf("expected subscribed, got %v", frame["type"])
	}

	integPublish(t, h, ch, json.RawMessage(`"hello"`))

	frame = integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "message" {
		t.Fatalf("expected message, got %v", frame["type"])
	}
	if frame["channel"] != ch {
		t.Fatalf("expected channel %q, got %v", ch, frame["channel"])
	}
	payload, ok := frame["payload"].(string)
	if !ok || payload != "hello" {
		t.Fatalf("expected payload %q, got %v", "hello", frame["payload"])
	}
	seq, ok := frame["seq_id"].(float64)
	if !ok || seq < 1 {
		t.Fatalf("expected seq_id >= 1, got %v", frame["seq_id"])
	}
}

func TestIntegration_Subscribe_HistoryReplay(t *testing.T) {
	_, ts, h, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)

	integPublish(t, h, ch, json.RawMessage(`"h1"`))
	integPublish(t, h, ch, json.RawMessage(`"h2"`))
	integPublish(t, h, ch, json.RawMessage(`"h3"`))

	conn := integDial(t, ts, token)
	frame := integSubscribe(t, conn, []string{ch}, map[string]int64{ch: 0})
	// Per SPEC H-7, history replay completes before subscribe registration,
	// so history messages always precede the subscribed ack. We collect all
	// 4 frames to verify both message payloads and the ack independently.

	// Collect frames: expect 3 history messages then 1 subscribed.
	var msgFrames []map[string]any
	var subscribedFrame map[string]any
	allFrames := []map[string]any{frame}
	for len(allFrames) < 4 {
		f := integReadFrame(t, conn, 5*time.Second)
		allFrames = append(allFrames, f)
	}

	for _, f := range allFrames {
		switch f["type"] {
		case "message":
			msgFrames = append(msgFrames, f)
		case "subscribed":
			subscribedFrame = f
		}
	}

	if len(msgFrames) != 3 {
		t.Fatalf("expected 3 history messages, got %d", len(msgFrames))
	}
	for i, expected := range []string{"h1", "h2", "h3"} {
		payload, _ := msgFrames[i]["payload"].(string)
		if payload != expected {
			t.Fatalf("history[%d]: expected %q, got %v", i, expected, msgFrames[i]["payload"])
		}
	}
	if subscribedFrame == nil {
		t.Fatal("expected subscribed frame in response")
	}

	// Real-time message after history
	integPublish(t, h, ch, json.RawMessage(`"rt"`))
	rtFrame := integReadFrame(t, conn, 5*time.Second)
	if rtFrame["type"] != "message" {
		t.Fatalf("expected message, got %v", rtFrame["type"])
	}
	payload, _ := rtFrame["payload"].(string)
	if payload != "rt" {
		t.Fatalf("expected payload %q, got %v", "rt", rtFrame["payload"])
	}
}

func TestIntegration_Subscribe_GapDetection(t *testing.T) {
	_, ts, h, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)

	integPublish(t, h, ch, json.RawMessage(`"m1"`))
	integPublish(t, h, ch, json.RawMessage(`"m2"`))
	integPublish(t, h, ch, json.RawMessage(`"m3"`))

	conn := integDial(t, ts, token)
	integWriteJSON(t, conn, map[string]any{
		"type":      "subscribe",
		"channels":  []string{ch},
		"after_seq": map[string]int64{ch: -1},
	})

	// First frame should be a gap frame
	frame := integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "gap" {
		t.Fatalf("expected gap frame, got type=%v", frame["type"])
	}
	if frame["channel"] != ch {
		t.Fatalf("expected gap channel %q, got %v", ch, frame["channel"])
	}
	availableSeq, ok := frame["available_from_seq"].(float64)
	if !ok || availableSeq != 1 {
		t.Fatalf("expected available_from_seq=1, got %v", frame["available_from_seq"])
	}
	requestedSeq, ok := frame["requested_from_seq"].(float64)
	if !ok || requestedSeq != -1 {
		t.Fatalf("expected requested_from_seq=-1, got %v", frame["requested_from_seq"])
	}
}

func TestIntegration_Subscribe_UnauthorizedChannel(t *testing.T) {
	_, ts, _, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	// Token allows only "orders.*" channels, ch won't match
	token := integGenerateToken(t, "limited", []string{"orders.*"}, time.Hour)
	conn := integDial(t, ts, token)

	integWriteJSON(t, conn, map[string]any{
		"type":     "subscribe",
		"channels": []string{ch},
	})

	frame := integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "error" {
		t.Fatalf("expected error frame, got type=%v", frame["type"])
	}
	code, ok := frame["code"].(float64)
	if !ok || int(code) != hub.ErrCodeUnauthorized {
		t.Fatalf("expected code %d, got %v", hub.ErrCodeUnauthorized, frame["code"])
	}
	msg, _ := frame["message"].(string)
	if !strings.Contains(msg, "unauthorized") {
		t.Fatalf("expected message to contain 'unauthorized', got %q", msg)
	}
}

func TestIntegration_Subscribe_AuthorizedChannel(t *testing.T) {
	_, ts, h, _ := integNewTestServer(t)
	// "orders.new" matches the "orders.*" pattern
	ch := "orders.new"
	token := integGenerateToken(t, "orders-sub", []string{"orders.*"}, time.Hour)
	conn := integDial(t, ts, token)

	frame := integSubscribe(t, conn, []string{ch}, nil)
	if frame["type"] != "subscribed" {
		t.Fatalf("expected subscribed, got %v", frame["type"])
	}

	// Verify we can receive messages on the authorized channel
	integPublish(t, h, ch, json.RawMessage(`"authorized"`))
	frame = integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "message" {
		t.Fatalf("expected message, got %v", frame["type"])
	}
}

// --- multi-subscriber tests ---

func TestIntegration_MultiSubscriber_Fanout(t *testing.T) {
	_, ts, h, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)

	conn1 := integDial(t, ts, token)
	conn2 := integDial(t, ts, token)

	integSubscribe(t, conn1, []string{ch}, nil)
	integSubscribe(t, conn2, []string{ch}, nil)

	integPublish(t, h, ch, json.RawMessage(`"fanout"`))

	for i, conn := range []*websocket.Conn{conn1, conn2} {
		frame := integReadFrame(t, conn, 5*time.Second)
		if frame["type"] != "message" {
			t.Fatalf("conn%d: expected message, got %v", i+1, frame["type"])
		}
		payload, _ := frame["payload"].(string)
		if payload != "fanout" {
			t.Fatalf("conn%d: expected payload %q, got %v", i+1, "fanout", frame["payload"])
		}
	}
}

func TestIntegration_Unsubscribe_StopsDelivery(t *testing.T) {
	_, ts, h, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)
	conn := integDial(t, ts, token)

	integSubscribe(t, conn, []string{ch}, nil)

	// Verify message delivery works before unsubscribe
	integPublish(t, h, ch, json.RawMessage(`"before"`))
	frame := integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "message" {
		t.Fatalf("expected message, got %v", frame["type"])
	}

	// Unsubscribe
	unsubFrame := integUnsubscribe(t, conn, []string{ch})
	if unsubFrame["type"] != "unsubscribed" {
		t.Fatalf("expected unsubscribed, got %v", unsubFrame["type"])
	}

	// Publish after unsub — no message should arrive
	integPublish(t, h, ch, json.RawMessage(`"after"`))
	integAssertNoFrame(t, conn, 500*time.Millisecond)
}

// --- connection lifecycle tests ---

func TestIntegration_Connection_ClientClose(t *testing.T) {
	mgr, ts, h, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)
	conn := integDial(t, ts, token)

	integSubscribe(t, conn, []string{ch}, nil)
	integPublish(t, h, ch, json.RawMessage(`"msg"`))
	frame := integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "message" {
		t.Fatalf("expected message, got %v", frame["type"])
	}

	// Close client-side, wait for server cleanup via polling
	conn.Close(websocket.StatusNormalClosure, "bye")

	deadline := time.Now().Add(2 * time.Second)
	for {
		mgr.mu.Lock()
		count := len(mgr.conns)
		mgr.mu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection not cleaned up within 2s, still have %d conns", count)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify publishing after close doesn't panic or deliver
	integPublish(t, h, ch, json.RawMessage(`"orphan"`))
}

func TestIntegration_Connection_RapidMessageDelivery(t *testing.T) {
	_, ts, h, _ := integNewTestServer(t)
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)
	conn := integDial(t, ts, token)

	integSubscribe(t, conn, []string{ch}, nil)

	// Publish many messages in rapid succession to exercise buffering and
	// writeLoop under load; the TCP send buffer on localhost is large enough
	// that buffer overflow won't trigger, but we verify all messages are
	// delivered in order.
	const count = 50
	for i := 0; i < count; i++ {
		payload, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		integPublish(t, h, ch, payload)
	}

	for i := 0; i < count; i++ {
		frame := integReadFrame(t, conn, 5*time.Second)
		if frame["type"] != "message" {
			t.Fatalf("frame %d: expected message, got %v", i, frame["type"])
		}
		seq, ok := frame["seq_id"].(float64)
		if !ok || int(seq) != i+1 {
			t.Fatalf("frame %d: expected seq_id=%d, got %v", i, i+1, frame["seq_id"])
		}
	}
}

// --- shutdown tests ---

func TestIntegration_Shutdown_ActiveConnection(t *testing.T) {
	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)

	st := integNewTestStore(t)
	a := integNewTestAuth(t, st.(store.KeyStore))
	hubInst := integNewTestHub(t, st, a)
	cfg := config.WebSocketConfig{
		PingInterval:   30 * time.Second,
		PongTimeout:    60 * time.Second,
		OutboundBuffer: 256,
		MaxMessageSize: 65536,
		AllowedOrigins: []string{"*"},
	}
	mgr := NewManager(hubInst, a, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	conn := integDial(t, ts, token)
	integSubscribe(t, conn, []string{ch}, nil)
	integPublish(t, hubInst, ch, json.RawMessage(`"alive"`))
	frame := integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "message" {
		t.Fatalf("expected message, got %v", frame["type"])
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- mgr.Shutdown(ctx)
	}()

	readCtx, readCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer readCancel()
	_, _, err := conn.Read(readCtx)
	if err == nil {
		t.Fatal("expected read error after shutdown close")
	}
	status := websocket.CloseStatus(err)
	if status != websocket.StatusGoingAway {
		t.Fatalf("expected close status 1001, got %d", status)
	}

	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestIntegration_Shutdown_RejectNewConnections(t *testing.T) {
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)

	st := integNewTestStore(t)
	a := integNewTestAuth(t, st.(store.KeyStore))
	hubInst := integNewTestHub(t, st, a)
	cfg := config.WebSocketConfig{
		PingInterval:   30 * time.Second,
		PongTimeout:    60 * time.Second,
		OutboundBuffer: 256,
		MaxMessageSize: 65536,
		AllowedOrigins: []string{"*"},
	}
	mgr := NewManager(hubInst, a, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	_, resp, err := websocket.Dial(context.Background(), integWSURL(ts.URL, token), nil)
	if err == nil {
		t.Fatal("expected dial error after shutdown")
	}
	if resp.StatusCode != 503 {
		t.Fatalf("expected status 503, got %d", resp.StatusCode)
	}
}

func TestIntegration_Shutdown_MultipleConnections(t *testing.T) {
	st := integNewTestStore(t)
	a := integNewTestAuth(t, st.(store.KeyStore))
	hubInst := integNewTestHub(t, st, a)
	cfg := config.WebSocketConfig{
		PingInterval:   30 * time.Second,
		PongTimeout:    60 * time.Second,
		OutboundBuffer: 256,
		MaxMessageSize: 65536,
		AllowedOrigins: []string{"*"},
	}
	mgr := NewManager(hubInst, a, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)

	var conns []*websocket.Conn
	for i := 0; i < 3; i++ {
		conn := integDial(t, ts, token)
		conns = append(conns, conn)
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- mgr.Shutdown(ctx)
	}()

	readCtx, readCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer readCancel()
	for i, conn := range conns {
		_, _, err := conn.Read(readCtx)
		if err == nil {
			t.Fatalf("conn%d: expected read error after shutdown", i+1)
		}
		status := websocket.CloseStatus(err)
		if status != websocket.StatusGoingAway {
			t.Fatalf("conn%d: expected close status 1001, got %d", i+1, status)
		}
	}

	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// --- heartbeat test ---

func TestIntegration_Heartbeat_PingPong(t *testing.T) {
	st := integNewTestStore(t)
	a := integNewTestAuth(t, st.(store.KeyStore))
	hubInst := integNewTestHub(t, st, a)
	cfg := config.WebSocketConfig{
		PingInterval:   100 * time.Millisecond,
		PongTimeout:    500 * time.Millisecond,
		OutboundBuffer: 256,
		MaxMessageSize: 65536,
		AllowedOrigins: []string{"*"},
	}
	mgr := NewManager(hubInst, a, cfg)
	ts := httptest.NewServer(mgr)
	defer ts.Close()

	ch := "int-ws-" + t.Name()
	token := integGenerateToken(t, "sub", []string{"*"}, time.Hour)
	conn := integDial(t, ts, token)

	// Wait long enough for multiple ping/pong cycles
	time.Sleep(600 * time.Millisecond)

	// Connection should still be alive
	integSubscribe(t, conn, []string{ch}, nil)
	integPublish(t, hubInst, ch, json.RawMessage(`"after-ping"`))

	frame := integReadFrame(t, conn, 5*time.Second)
	if frame["type"] != "message" {
		t.Fatalf("expected message, got %v", frame["type"])
	}
}

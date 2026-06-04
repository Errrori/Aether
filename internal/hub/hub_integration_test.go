//go:build integration

package hub

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/store"
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

func integNewTestAuth(t *testing.T, ks store.KeyStore) auth.Auth {
	t.Helper()
	cfg := &config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  30 * time.Second,
	}
	a, err := auth.New(cfg, ks)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return a
}

func integAdminClaims() *auth.Claims {
	return &auth.Claims{Subject: "test-sub", Channels: []string{"*"}}
}

func integNewTestHub(t *testing.T) (Hub, store.Store) {
	t.Helper()
	st := integNewTestStore(t)
	a := integNewTestAuth(t, st.(store.KeyStore))
	cfg := HubConfig{
		OutboundBufferSize:      256,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
	}
	h := New(st, a, cfg, NopMetrics())
	return h, st
}

func integNewTestConnection(t *testing.T, id string) *Connection {
	t.Helper()
	return NewConnection(id, "sub-"+id, integAdminClaims(), 256)
}

func integDrainFrame(t *testing.T, conn *Connection) []byte {
	t.Helper()
	select {
	case data := <-conn.Send:
		return data
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for frame")
		return nil
	}
}

func integAssertNoFrame(t *testing.T, conn *Connection) {
	t.Helper()
	select {
	case data := <-conn.Send:
		t.Fatalf("unexpected frame: %s", string(data))
	default:
	}
}

// integPublish is a helper that calls Publish and fails the test on error.
func integPublish(t *testing.T, h Hub, ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (int64, time.Time) {
	t.Helper()
	seqID, ts, err := h.Publish(ctx, channel, payload, idempotencyKey)
	if err != nil {
		t.Fatalf("Publish to %s: %v", channel, err)
	}
	return seqID, ts
}

// integSubscribe is a helper that calls Subscribe and fails the test on error.
func integSubscribe(t *testing.T, h Hub, conn *Connection, channels []string, afterSeq map[string]int64) {
	t.Helper()
	if err := h.Subscribe(conn, channels, afterSeq); err != nil {
		t.Fatalf("Subscribe to %v: %v", channels, err)
	}
}

// --- tests ---

// TestIntegration_Publish_PersistsAndReturnsSeqID verifies that Publish
// writes to the real store and returns the correct seq_id.
func TestIntegration_Publish_PersistsAndReturnsSeqID(t *testing.T) {
	h, st := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	payload := json.RawMessage(`{"msg":"hello"}`)
	seqID, _ := integPublish(t, h, ctx, ch, payload, nil)
	if seqID != 1 {
		t.Errorf("expected seq_id 1, got %d", seqID)
	}

	result, err := st.ReadHistory(ctx, ch, 0, 100)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message in store, got %d", len(result.Messages))
	}
	if result.Messages[0].SeqID != 1 {
		t.Errorf("expected seq_id 1 in store, got %d", result.Messages[0].SeqID)
	}
	if string(result.Messages[0].Payload) != `{"msg":"hello"}` {
		t.Errorf("expected payload 'hello', got %s", string(result.Messages[0].Payload))
	}
}

// TestIntegration_Publish_IdempotentKey verifies that publishing with the
// same idempotency key returns the original seq_id and does not duplicate data.
func TestIntegration_Publish_IdempotentKey(t *testing.T) {
	h, st := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	key := "idem-key-1"
	seq1, _ := integPublish(t, h, ctx, ch, json.RawMessage(`"first"`), &key)
	seq2, _ := integPublish(t, h, ctx, ch, json.RawMessage(`"second"`), &key)
	if seq1 != seq2 {
		t.Errorf("expected same seq_id for idempotent publish, got %d and %d", seq1, seq2)
	}

	result, err := st.ReadHistory(ctx, ch, 0, 100)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	if string(result.Messages[0].Payload) != `"first"` {
		t.Errorf("expected payload to be first value, got %s", string(result.Messages[0].Payload))
	}
}

// TestIntegration_Subscribe_HistoryReplayThenRealTime verifies that
// subscribe with after_seq replays history in order, then receives
// real-time messages.
func TestIntegration_Subscribe_HistoryReplayThenRealTime(t *testing.T) {
	h, _ := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	integPublish(t, h, ctx, ch, json.RawMessage(`"h1"`), nil)
	integPublish(t, h, ctx, ch, json.RawMessage(`"h2"`), nil)
	integPublish(t, h, ctx, ch, json.RawMessage(`"h3"`), nil)

	conn := integNewTestConnection(t, "c1")
	integSubscribe(t, h, conn, []string{ch}, map[string]int64{ch: 0})

	f1 := integDrainFrame(t, conn)
	if !strings.Contains(string(f1), `"h1"`) {
		t.Errorf("expected history msg 1, got %s", string(f1))
	}
	f2 := integDrainFrame(t, conn)
	if !strings.Contains(string(f2), `"h2"`) {
		t.Errorf("expected history msg 2, got %s", string(f2))
	}
	f3 := integDrainFrame(t, conn)
	if !strings.Contains(string(f3), `"h3"`) {
		t.Errorf("expected history msg 3, got %s", string(f3))
	}

	f4 := integDrainFrame(t, conn)
	if !strings.Contains(string(f4), `"subscribed"`) {
		t.Errorf("expected subscribed frame, got %s", string(f4))
	}

	integPublish(t, h, ctx, ch, json.RawMessage(`"rt"`), nil)
	f5 := integDrainFrame(t, conn)
	if !strings.Contains(string(f5), `"rt"`) {
		t.Errorf("expected real-time message, got %s", string(f5))
	}
}

// TestIntegration_Subscribe_NoHistory verifies that subscribe without
// after_seq only receives a subscribed ack (no history), then real-time
// messages are delivered.
func TestIntegration_Subscribe_NoHistory(t *testing.T) {
	h, _ := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	integPublish(t, h, ctx, ch, json.RawMessage(`"old"`), nil)

	conn := integNewTestConnection(t, "c1")
	integSubscribe(t, h, conn, []string{ch}, nil)

	f1 := integDrainFrame(t, conn)
	if !strings.Contains(string(f1), `"subscribed"`) {
		t.Errorf("expected subscribed frame, got %s", string(f1))
	}

	integPublish(t, h, ctx, ch, json.RawMessage(`"new"`), nil)
	f2 := integDrainFrame(t, conn)
	if !strings.Contains(string(f2), `"new"`) {
		t.Errorf("expected real-time message, got %s", string(f2))
	}
}

// TestIntegration_Subscribe_GapDetection verifies that when after_seq is
// before the earliest available message, a gap frame is sent.
func TestIntegration_Subscribe_GapDetection(t *testing.T) {
	h, _ := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	integPublish(t, h, ctx, ch, json.RawMessage(`"m1"`), nil)
	integPublish(t, h, ctx, ch, json.RawMessage(`"m2"`), nil)
	integPublish(t, h, ctx, ch, json.RawMessage(`"m3"`), nil)

	conn := integNewTestConnection(t, "c1")
	integSubscribe(t, h, conn, []string{ch}, map[string]int64{ch: -1})

	f1 := integDrainFrame(t, conn)
	if !strings.Contains(string(f1), `"type":"gap"`) {
		t.Errorf("expected gap frame first, got %s", string(f1))
	}
}

// TestIntegration_Subscribe_MultipleSubscribers verifies that all
// subscribers receive a published message.
func TestIntegration_Subscribe_MultipleSubscribers(t *testing.T) {
	h, _ := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	conn1 := integNewTestConnection(t, "c1")
	conn2 := integNewTestConnection(t, "c2")
	conn3 := integNewTestConnection(t, "c3")

	integSubscribe(t, h, conn1, []string{ch}, nil)
	integSubscribe(t, h, conn2, []string{ch}, nil)
	integSubscribe(t, h, conn3, []string{ch}, nil)

	integDrainFrame(t, conn1)
	integDrainFrame(t, conn2)
	integDrainFrame(t, conn3)

	integPublish(t, h, ctx, ch, json.RawMessage(`"broadcast"`), nil)

	for i, conn := range []*Connection{conn1, conn2, conn3} {
		data := integDrainFrame(t, conn)
		if !strings.Contains(string(data), `"broadcast"`) {
			t.Errorf("conn%d did not receive broadcast: %s", i+1, string(data))
		}
	}
}

// TestIntegration_Unsubscribe_StopsDelivery verifies that after
// unsubscribe, published messages are not delivered.
func TestIntegration_Unsubscribe_StopsDelivery(t *testing.T) {
	h, _ := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	conn := integNewTestConnection(t, "c1")
	integSubscribe(t, h, conn, []string{ch}, nil)
	integDrainFrame(t, conn)

	h.Unsubscribe(conn, []string{ch})
	data := integDrainFrame(t, conn)
	if !strings.Contains(string(data), `"unsubscribed"`) {
		t.Fatalf("expected unsubscribed frame, got %s", string(data))
	}

	integPublish(t, h, ctx, ch, json.RawMessage(`"orphan"`), nil)
	integAssertNoFrame(t, conn)
}

// TestIntegration_RemoveConnection_Cleanup verifies that after removing a
// connection, it is closed and no longer receives messages.
func TestIntegration_RemoveConnection_Cleanup(t *testing.T) {
	h, _ := integNewTestHub(t)
	ctx := context.Background()
	ch := "int-" + t.Name()

	conn := integNewTestConnection(t, "c1")
	integSubscribe(t, h, conn, []string{ch}, nil)
	integDrainFrame(t, conn)

	h.RemoveConnection(conn)

	select {
	case <-conn.Done():
	default:
		t.Error("connection should be closed after RemoveConnection")
	}

	integPublish(t, h, ctx, ch, json.RawMessage(`"orphan"`), nil)
	integAssertNoFrame(t, conn)
}

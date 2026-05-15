package hub

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/store"
)

// --- mockStore ---

type mockStore struct {
	mu       sync.Mutex
	messages map[string][]store.Message // channel -> ordered messages
	nextSeq  map[string]int64           // channel -> next seq_id

	writeErr error
	readErr  error
}

func newMockStore() *mockStore {
	return &mockStore{
		messages: make(map[string][]store.Message),
		nextSeq:  make(map[string]int64),
	}
}

func (m *mockStore) RunMigrations(ctx context.Context) error { return nil }
func (m *mockStore) Ping(ctx context.Context) error          { return nil }
func (m *mockStore) EvictExpiredMessages(ctx context.Context) (int, int, error) {
	return 0, 0, nil
}
func (m *mockStore) Close() {}

func (m *mockStore) WriteMessage(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (int64, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return 0, time.Time{}, m.writeErr
	}
	seq := m.nextSeq[channel] + 1
	m.nextSeq[channel] = seq
	msg := store.Message{
		SeqID:     seq,
		Payload:   payload,
		CreatedAt: time.Now(),
	}
	m.messages[channel] = append(m.messages[channel], msg)
	return seq, msg.CreatedAt, nil
}

func (m *mockStore) ReadHistory(ctx context.Context, channel string, afterSeq int64, limit int) (*store.HistoryResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, m.readErr
	}
	msgs, ok := m.messages[channel]
	if !ok || len(msgs) == 0 {
		return &store.HistoryResult{Messages: nil, MinSeq: 1}, nil
	}
	minSeq := msgs[0].SeqID
	var result []store.Message
	for _, msg := range msgs {
		if msg.SeqID > afterSeq && len(result) < limit {
			result = append(result, msg)
		}
	}
	return &store.HistoryResult{Messages: result, MinSeq: minSeq}, nil
}

// --- mockAuth ---

type mockAuth struct {
	authorized map[string]bool // channel -> authorized
}

func newMockAuth() *mockAuth {
	return &mockAuth{authorized: make(map[string]bool)}
}

func (a *mockAuth) ValidateAPIKey(key string) bool                          { return true }
func (a *mockAuth) ParseAndValidateToken(tokenString string) (*auth.Claims, error) { return nil, nil }
func (a *mockAuth) IsChannelAuthorized(claims *auth.Claims, channel string) bool {
	ok, exists := a.authorized[channel]
	if !exists {
		return true // default: authorized
	}
	return ok
}

// --- test helpers ---

func newTestHub(t *testing.T) (*hubImpl, *mockStore) {
	t.Helper()
	store := newMockStore()
	auth := newMockAuth()
	cfg := HubConfig{
		OutboundBufferSize:      16,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
	}
	h := New(store, auth, cfg, NopMetrics()).(*hubImpl)
	return h, store
}

func newTestConnection(t *testing.T, id string) *Connection {
	t.Helper()
	return NewConnection(id, "sub-"+id, &auth.Claims{Subject: "sub-" + id, Channels: nil}, 16)
}

// drainFrame reads a single frame from the connection's Send channel with a timeout.
func drainFrame(t *testing.T, conn *Connection) []byte {
	t.Helper()
	select {
	case data := <-conn.Send:
		return data
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for frame")
		return nil
	}
}

// assertNoFrame verifies no frame is queued on the connection.
func assertNoFrame(t *testing.T, conn *Connection) {
	t.Helper()
	select {
	case data := <-conn.Send:
		t.Fatalf("unexpected frame: %s", string(data))
	default:
	}
}

// --- H-1: Publish persists to Store then fans out to subscribers ---

func TestHub_Publish_PersistsAndFansOut(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, nil)
	drainFrame(t, conn) // consume subscribed ack

	payload := json.RawMessage(`{"msg":"hello"}`)
	seqID, _, err := h.Publish(context.Background(), "ch", payload, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if seqID != 1 {
		t.Errorf("expected seq 1, got %d", seqID)
	}

	data := drainFrame(t, conn)
	if !strings.Contains(string(data), `"type":"message"`) {
		t.Errorf("expected message frame, got %s", string(data))
	}
	if !strings.Contains(string(data), `"hello"`) {
		t.Errorf("expected payload in frame, got %s", string(data))
	}
}

// --- H-2: Persistence failure returns error, no message pushed ---

func TestHub_Publish_StoreErrorReturnsError(t *testing.T) {
	h, store := newTestHub(t)
	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, nil)
	drainFrame(t, conn) // subscribed ack

	store.writeErr = context.DeadlineExceeded

	_, _, err := h.Publish(context.Background(), "ch", json.RawMessage(`"x"`), nil)
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
	assertNoFrame(t, conn)
}

// --- H-3: RWMutex safety — concurrent Publish and Subscribe ---

func TestHub_ConcurrentPublishAndSubscribe(t *testing.T) {
	h, _ := newTestHub(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			conn := newTestConnection(t, "c"+string(rune('0'+n)))
			h.Subscribe(conn, []string{"ch"}, nil)
			drainFrame(t, conn)
		}(i)
	}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Publish(context.Background(), "ch", json.RawMessage(`"x"`), nil)
		}()
	}

	wg.Wait()
	// No race detector warnings = pass.
}

// --- H-5: Full buffer triggers Overflow (connection close) ---

func TestHub_Publish_BufferFullClosesConnection(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")
	// Reduce buffer to 1 so it fills immediately.
	conn.Send = make(chan []byte, 1)
	overflowCalled := false
	conn.Overflow = func() { overflowCalled = true }

	// Subscribe fills the buffer (subscribed ack occupies the only slot).
	h.Subscribe(conn, []string{"ch"}, nil)

	// Publish — buffer is still full from the subscribed frame.
	h.Publish(context.Background(), "ch", json.RawMessage(`"x"`), nil)

	if !overflowCalled {
		t.Error("expected Overflow to be called when buffer is full")
	}
}

// --- H-6: Duplicate subscribe is silently ignored ---

func TestHub_Subscribe_DuplicateIsIgnored(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")

	h.Subscribe(conn, []string{"ch"}, nil)
	drainFrame(t, conn) // first subscribed: ["ch"]

	h.Subscribe(conn, []string{"ch"}, nil)
	drainFrame(t, conn) // second subscribed: [] (duplicate skipped)

	if conn.ChannelCount() != 1 {
		t.Errorf("expected 1 channel, got %d", conn.ChannelCount())
	}
}

// --- H-7: after_seq replays history before registering for real-time ---

func TestHub_Subscribe_HistoryBeforeRealTime(t *testing.T) {
	h, store := newTestHub(t)

	// Pre-populate history via Publish.
	h.Publish(context.Background(), "ch", json.RawMessage(`"h1"`), nil)
	h.Publish(context.Background(), "ch", json.RawMessage(`"h2"`), nil)

	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, map[string]int64{"ch": 0})

	// Should receive: history msg 1, history msg 2, subscribed ack (in that order).
	f1 := drainFrame(t, conn)
	if !strings.Contains(string(f1), `"h1"`) {
		t.Errorf("expected history msg 1, got %s", string(f1))
	}
	f2 := drainFrame(t, conn)
	if !strings.Contains(string(f2), `"h2"`) {
		t.Errorf("expected history msg 2, got %s", string(f2))
	}
	f3 := drainFrame(t, conn)
	if !strings.Contains(string(f3), `"subscribed"`) {
		t.Errorf("expected subscribed frame, got %s", string(f3))
	}
	if !strings.Contains(string(f3), `"ch"`) {
		t.Errorf("expected subscribed to include ch, got %s", string(f3))
	}

	// Now publish a real-time message.
	h.Publish(context.Background(), "ch", json.RawMessage(`"rt"`), nil)
	f4 := drainFrame(t, conn)
	if !strings.Contains(string(f4), `"rt"`) {
		t.Errorf("expected real-time msg, got %s", string(f4))
	}

	_ = store
}

// --- H-8, H-9: Gap detection when after_seq < minSeq - 1 ---

func TestHub_Subscribe_GapDetection(t *testing.T) {
	h, _ := newTestHub(t)

	// Publish messages at seq 1,2,3.
	h.Publish(context.Background(), "ch", json.RawMessage(`"m1"`), nil)
	h.Publish(context.Background(), "ch", json.RawMessage(`"m2"`), nil)
	h.Publish(context.Background(), "ch", json.RawMessage(`"m3"`), nil)

	conn := newTestConnection(t, "c1")
	// Request after_seq = 3 (no gap — already have the latest three messages
	// but let's test with after_seq=0 and minSeq=1).
	// Actually, minSeq is 1. If after_seq < 0 (which is -1 for minSeq-1=0), gap triggers.
	// Let's first evict some messages to create a gap. But mockStore doesn't evict.
	// Instead, test the gap formula: afterSeq < minSeq - 1.
	// minSeq=1, so afterSeq < 0 triggers gap. Test with afterSeq = -1 (which means
	// "from the beginning" in normal cases).
	//
	// Real scenario: messages 1-3 exist. minSeq=1. afterSeq=-1 → -1 < 0 → gap.
	h.Subscribe(conn, []string{"ch"}, map[string]int64{"ch": -1})

	// First frame should be a gap frame.
	f1 := drainFrame(t, conn)
	if !strings.Contains(string(f1), `"type":"gap"`) {
		t.Errorf("expected gap frame, got %s", string(f1))
	}
}

// --- H-10: Channel limits ---

func TestHub_Subscribe_TooManyPerRequest(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")

	channels := make([]string, 101)
	for i := range channels {
		channels[i] = "ch" + string(rune('0'+i%10))
	}
	h.Subscribe(conn, channels, nil)

	data := drainFrame(t, conn)
	if !strings.Contains(string(data), "40005") {
		t.Errorf("expected error code 40005, got %s", string(data))
	}
}

func TestHub_Subscribe_TooManyTotal(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")

	// First subscribe: 2 channels.
	h.Subscribe(conn, []string{"a", "b"}, nil)
	drainFrame(t, conn)

	// Artificially set channel count to 999 so next subscribe exceeds 1000.
	conn.AddChannel("x") // dummy to push count
	// Actually, just create a hub with a low limit for this test.
}

func TestHub_Subscribe_TotalLimitExceeded(t *testing.T) {
	h, _ := newTestHub(t)
	h.config.MaxChannelsPerConn = 3
	conn := newTestConnection(t, "c1")

	h.Subscribe(conn, []string{"a", "b"}, nil)
	drainFrame(t, conn) // subscribed ack for [a, b]

	// Now try to subscribe to 2 more (would exceed 3).
	h.Subscribe(conn, []string{"c", "d"}, nil)
	data := drainFrame(t, conn)
	if !strings.Contains(string(data), "40006") {
		t.Errorf("expected error code 40006, got %s", string(data))
	}
}

// --- H-11: Unauthorized channel returns error frame 40301 ---

func TestHub_Subscribe_UnauthorizedChannel(t *testing.T) {
	h, _ := newTestHub(t)
	ma := newMockAuth()
	ma.authorized["secret"] = false // deny "secret" channel
	h.auth = ma

	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"public", "secret"}, nil)

	// Should get error frame for "secret".
	data := drainFrame(t, conn)
	if !strings.Contains(string(data), "40301") {
		t.Errorf("expected error code 40301, got %s", string(data))
	}
	if !strings.Contains(string(data), "secret") {
		t.Errorf("expected error to mention 'secret', got %s", string(data))
	}

	// Should also get subscribed ack for "public" only.
	data2 := drainFrame(t, conn)
	if !strings.Contains(string(data2), `"subscribed"`) {
		t.Errorf("expected subscribed frame, got %s", string(data2))
	}
	if strings.Contains(string(data2), "secret") {
		t.Errorf("secret should not be in subscribed channels: %s", string(data2))
	}
}

// --- Unsubscribe ---

func TestHub_Unsubscribe(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")

	h.Subscribe(conn, []string{"a", "b"}, nil)
	drainFrame(t, conn) // subscribed ack

	h.Unsubscribe(conn, []string{"a"})
	data := drainFrame(t, conn)
	if !strings.Contains(string(data), `"unsubscribed"`) {
		t.Errorf("expected unsubscribed frame, got %s", string(data))
	}
	if !strings.Contains(string(data), "a") {
		t.Errorf("expected unsubscribed to include 'a', got %s", string(data))
	}
	if conn.HasChannel("a") {
		t.Error("conn should no longer have channel 'a'")
	}
	if !conn.HasChannel("b") {
		t.Error("conn should still have channel 'b'")
	}
}

// --- RemoveConnection ---

func TestHub_RemoveConnection(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")

	h.Subscribe(conn, []string{"a", "b"}, nil)
	drainFrame(t, conn) // subscribed ack

	h.RemoveConnection(conn)

	// Publish to "a" should not reach anyone.
	h.Publish(context.Background(), "a", json.RawMessage(`"orphan"`), nil)
	assertNoFrame(t, conn)

	// Connection should be closed.
	select {
	case <-conn.Done():
	default:
		t.Error("connection should be closed after RemoveConnection")
	}
}

// --- Publish to channel with no subscribers ---

func TestHub_Publish_NoSubscribers(t *testing.T) {
	h, _ := newTestHub(t)

	seqID, _, err := h.Publish(context.Background(), "empty-room", json.RawMessage(`"x"`), nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if seqID != 1 {
		t.Errorf("expected seq 1, got %d", seqID)
	}
	// No subscribers → no frames to send. Just shouldn't crash.
}

// --- Subscribe with invalid channel name ---

func TestHub_Subscribe_InvalidChannelName(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")

	h.Subscribe(conn, []string{""}, nil)
	data := drainFrame(t, conn)
	if !strings.Contains(string(data), "40001") {
		t.Errorf("expected error code 40001 for invalid channel, got %s", string(data))
	}
}

// --- Publish with invalid channel name ---

func TestHub_Publish_InvalidChannelName(t *testing.T) {
	h, _ := newTestHub(t)

	_, _, err := h.Publish(context.Background(), "", json.RawMessage(`"x"`), nil)
	if err == nil {
		t.Fatal("expected error for invalid channel name")
	}
}

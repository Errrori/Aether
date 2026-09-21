package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
)

// --- mock cursor store ---

// mockCursorStore augments mockStore with subscriber cursor persistence so the
// hub can be built with a store that implements store.CursorStore.
type mockCursorStore struct {
	*mockStore

	mu        sync.Mutex
	cursors   map[pendingCursorKey]int64
	saveCalls int
	saveErr   error
	loadErr   error
	saveHook  func()
}

func newMockCursorStore() *mockCursorStore {
	return &mockCursorStore{
		mockStore: newMockStore(),
		cursors:   make(map[pendingCursorKey]int64),
	}
}

func (m *mockCursorStore) LoadCursors(ctx context.Context, subscriberID string, channels []string) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	result := make(map[string]int64, len(channels))
	for _, ch := range channels {
		if seq, ok := m.cursors[pendingCursorKey{subscriberID: subscriberID, channel: ch}]; ok {
			result[ch] = seq
		}
	}
	return result, nil
}

func (m *mockCursorStore) SaveCursors(ctx context.Context, subscriberID string, cursors map[string]int64) error {
	m.mu.Lock()
	m.saveCalls++
	err := m.saveErr
	hook := m.saveHook
	m.mu.Unlock()

	if hook != nil {
		hook()
	}
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for ch, seq := range cursors {
		key := pendingCursorKey{subscriberID: subscriberID, channel: ch}
		if seq > m.cursors[key] {
			m.cursors[key] = seq
		}
	}
	return nil
}

func (m *mockCursorStore) DeleteStaleCursors(ctx context.Context, ttl time.Duration) (int64, error) {
	return 0, nil
}

func (m *mockCursorStore) cursor(subscriberID, channel string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seq, ok := m.cursors[pendingCursorKey{subscriberID: subscriberID, channel: channel}]
	return seq, ok
}

func (m *mockCursorStore) setCursor(subscriberID, channel string, seq int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursors[pendingCursorKey{subscriberID: subscriberID, channel: channel}] = seq
}

func (m *mockCursorStore) saveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCalls
}

func (m *mockCursorStore) setSaveErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveErr = err
}

func (m *mockCursorStore) setLoadErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadErr = err
}

func (m *mockCursorStore) setSaveHook(hook func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveHook = hook
}

// --- test helpers ---

func newTestHubWithCursorStore(t testing.TB) (*hubImpl, *mockCursorStore) {
	t.Helper()
	ms := newMockCursorStore()
	cfg := HubConfig{
		OutboundBufferSize:      16,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
		AckFlushInterval:        10 * time.Millisecond,
	}
	h := New(ms, newMockAuth(), cfg, NopMetrics()).(*hubImpl)
	return h, ms
}

func publishN(t *testing.T, h *hubImpl, channel string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, _, err := h.Publish(context.Background(), channel, json.RawMessage(`"m"`), nil); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
}

func sameSubscriberConn(t *testing.T, id string, reference *Connection) *Connection {
	t.Helper()
	return NewConnection(id, reference.SubscriberID, &auth.Claims{Subject: reference.SubscriberID}, 16)
}

func waitForCursor(t *testing.T, ms *mockCursorStore, subscriberID, channel string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if seq, ok := ms.cursor(subscriberID, channel); ok && seq == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("cursor %s/%s did not reach %d in time", subscriberID, channel, want)
}

// --- AK-1: ack validation ---

func TestHub_Ack_InvalidEntriesRejected(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn)

	h.Ack(conn, map[string]int64{"other": 1}) // not subscribed
	h.Ack(conn, map[string]int64{"ch": -1})   // negative seq

	for i := 0; i < 2; i++ {
		var frame ErrorFrame
		if err := json.Unmarshal(drainFrame(t, conn), &frame); err != nil {
			t.Fatalf("unmarshal error frame: %v", err)
		}
		if frame.Code != ErrCodeInvalidAck {
			t.Errorf("error code = %d, want %d", frame.Code, ErrCodeInvalidAck)
		}
	}

	h.flushPending()
	if _, ok := ms.cursor(conn.SubscriberID, "other"); ok {
		t.Error("ack for unsubscribed channel was persisted")
	}
	if _, ok := ms.cursor(conn.SubscriberID, "ch"); ok {
		t.Error("negative ack was persisted")
	}
}

func TestHub_Ack_UnauthorizedChannel(t *testing.T) {
	h, _ := newTestHubWithCursorStore(t)
	conn := newTestConnection(t, "c1")
	// Simulate claims that no longer authorize a subscribed channel.
	conn.AddChannel("secret")
	h.auth.(*mockAuth).authorized["secret"] = false

	h.Ack(conn, map[string]int64{"secret": 3})

	var frame ErrorFrame
	if err := json.Unmarshal(drainFrame(t, conn), &frame); err != nil {
		t.Fatalf("unmarshal error frame: %v", err)
	}
	if frame.Code != ErrCodeUnauthorized {
		t.Errorf("error code = %d, want %d", frame.Code, ErrCodeUnauthorized)
	}
}

func TestHub_Ack_MetricsCountAcceptedOnly(t *testing.T) {
	accepted := 0
	ms := newMockCursorStore()
	cfg := HubConfig{
		OutboundBufferSize: 16, MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn: 1000, HistoryLimit: 1000,
	}
	h := New(ms, newMockAuth(), cfg, Metrics{IncAcks: func() { accepted++ }}).(*hubImpl)
	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn)

	h.Ack(conn, map[string]int64{"ch": 1, "other": 2, "nope": -1})
	h.Ack(conn, map[string]int64{"ch": 4})

	if accepted != 2 {
		t.Fatalf("IncAcks called %d times, want 2 (accepted only)", accepted)
	}
	// Drain the two error frames.
	drainFrame(t, conn)
	drainFrame(t, conn)
}

// --- AK-2 / AK-3: merging, monotonicity, flush semantics ---

func TestHub_Ack_MonotonicAndMergedIntoSingleFlush(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn)

	h.Ack(conn, map[string]int64{"ch": 10})
	h.Ack(conn, map[string]int64{"ch": 5})
	assertNoFrame(t, conn) // success is silent

	h.flushPending()
	seq, ok := ms.cursor(conn.SubscriberID, "ch")
	if !ok || seq != 10 {
		t.Fatalf("persisted cursor = %d (ok=%v), want 10", seq, ok)
	}
	if calls := ms.saveCount(); calls != 1 {
		t.Fatalf("SaveCursors calls = %d, want 1 merged write", calls)
	}
	if pending := h.pendingCursor(conn.SubscriberID, "ch"); pending != 0 {
		t.Fatalf("pending = %d after successful flush, want 0", pending)
	}
}

func TestHub_AckFlush_FailureKeepsPendingAndRetries(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn)

	h.Ack(conn, map[string]int64{"ch": 7})
	ms.setSaveErr(errors.New("db down"))
	h.flushPending()

	if pending := h.pendingCursor(conn.SubscriberID, "ch"); pending != 7 {
		t.Fatalf("pending = %d after failed flush, want 7 kept for retry", pending)
	}

	ms.setSaveErr(nil)
	h.flushPending()
	waitForCursor(t, ms, conn.SubscriberID, "ch", 7)
	if pending := h.pendingCursor(conn.SubscriberID, "ch"); pending != 0 {
		t.Fatalf("pending = %d after retry, want 0", pending)
	}
}

func TestHub_AckFlush_KeepsCursorAdvancedDuringWrite(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn)

	h.Ack(conn, map[string]int64{"ch": 5})
	// Advance the pending cursor while the flusher is mid-write (same
	// goroutine via the hook, so no data race).
	ms.setSaveHook(func() { h.mergePending(conn.SubscriberID, "ch", 9) })
	h.flushPending()

	if seq, _ := ms.cursor(conn.SubscriberID, "ch"); seq != 5 {
		t.Fatalf("persisted cursor = %d, want the snapshot value 5", seq)
	}
	if pending := h.pendingCursor(conn.SubscriberID, "ch"); pending != 9 {
		t.Fatalf("pending = %d, want 9 kept (advanced during write)", pending)
	}

	ms.setSaveHook(nil)
	h.flushPending()
	waitForCursor(t, ms, conn.SubscriberID, "ch", 9)
}

// --- AK-4 / AK-5: resume from persisted and pending cursors ---

func TestHub_Resume_ReplaysAfterPersistedCursor(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	publishN(t, h, "ch", 5)

	conn := newTestConnection(t, "c1")
	ms.setCursor(conn.SubscriberID, "ch", 3)

	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{Resume: true})

	for want := int64(4); want <= 5; want++ {
		var mf MessageFrame
		if err := json.Unmarshal(drainFrame(t, conn), &mf); err != nil {
			t.Fatalf("unmarshal message frame: %v", err)
		}
		if mf.Type != FrameTypeMessage || mf.SeqID != want {
			t.Fatalf("frame = %+v, want message seq %d", mf, want)
		}
	}
	var subscribed SubscribedFrame
	if err := json.Unmarshal(drainFrame(t, conn), &subscribed); err != nil {
		t.Fatalf("unmarshal subscribed frame: %v", err)
	}
	if subscribed.Type != FrameTypeSubscribed {
		t.Fatalf("expected subscribed frame, got %+v", subscribed)
	}

	if _, _, err := h.Publish(context.Background(), "ch", json.RawMessage(`"rt"`), nil); err != nil {
		t.Fatalf("Publish realtime: %v", err)
	}
	var rt MessageFrame
	if err := json.Unmarshal(drainFrame(t, conn), &rt); err != nil {
		t.Fatalf("unmarshal realtime frame: %v", err)
	}
	if rt.SeqID != 6 {
		t.Fatalf("realtime seq = %d, want 6", rt.SeqID)
	}
}
func TestHub_Resume_ExplicitAfterSeqWins(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	publishN(t, h, "ch", 5)

	conn := newTestConnection(t, "c1")
	ms.setCursor(conn.SubscriberID, "ch", 3)

	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{
		AfterSeq: map[string]int64{"ch": 1},
		Resume:   true,
	})

	for want := int64(2); want <= 5; want++ {
		var mf MessageFrame
		if err := json.Unmarshal(drainFrame(t, conn), &mf); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if mf.SeqID != want {
			t.Fatalf("seq = %d, want %d (explicit after_seq)", mf.SeqID, want)
		}
	}
}

func TestHub_Resume_NoCursorIsLiveOnly(t *testing.T) {
	h, _ := newTestHubWithCursorStore(t)
	publishN(t, h, "ch", 3)

	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{Resume: true})

	var subscribed SubscribedFrame
	if err := json.Unmarshal(drainFrame(t, conn), &subscribed); err != nil {
		t.Fatalf("unmarshal subscribed frame: %v", err)
	}
	if subscribed.Type != FrameTypeSubscribed {
		t.Fatalf("expected subscribed frame, got %+v", subscribed)
	}
	assertNoFrame(t, conn) // history must not be replayed without a cursor
}

func TestHub_Resume_UsesPendingCursorBeyondPersisted(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	publishN(t, h, "ch", 7)

	conn1 := newTestConnection(t, "c1")
	ms.setCursor(conn1.SubscriberID, "ch", 2)
	h.Subscribe(conn1, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn1)

	h.Ack(conn1, map[string]int64{"ch": 5}) // pending, never flushed

	conn2 := sameSubscriberConn(t, "c2", conn1)
	h.Subscribe(conn2, []string{"ch"}, SubscribeOptions{Resume: true})

	for want := int64(6); want <= 7; want++ {
		var mf MessageFrame
		if err := json.Unmarshal(drainFrame(t, conn2), &mf); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if mf.SeqID != want {
			t.Fatalf("seq = %d, want %d (pending cursor 5 beats persisted 2)", mf.SeqID, want)
		}
	}
}

func TestHub_Resume_LoadErrorFailsClosedPerChannel(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	publishN(t, h, "b", 2)

	conn := newTestConnection(t, "c1")
	ms.setLoadErr(errors.New("db down"))

	h.Subscribe(conn, []string{"a", "b"}, SubscribeOptions{
		Resume:   true,
		AfterSeq: map[string]int64{"b": 0},
	})

	// Expect: 50001 for "a", replay of b1/b2, subscribed for ["b"] only.
	var sawError, sawSubscribed bool
	var replayed int
	for i := 0; i < 4; i++ {
		data := drainFrame(t, conn)
		if strings.Contains(string(data), `"type":"error"`) {
			var ef ErrorFrame
			if err := json.Unmarshal(data, &ef); err != nil {
				t.Fatalf("unmarshal error frame: %v", err)
			}
			if ef.Code != ErrCodeHistoryFailed {
				t.Errorf("error code = %d, want %d", ef.Code, ErrCodeHistoryFailed)
			}
			sawError = true
		} else if strings.Contains(string(data), `"type":"subscribed"`) {
			var sf SubscribedFrame
			if err := json.Unmarshal(data, &sf); err != nil {
				t.Fatalf("unmarshal subscribed frame: %v", err)
			}
			if len(sf.Channels) != 1 || sf.Channels[0] != "b" {
				t.Errorf("subscribed channels = %v, want [b]", sf.Channels)
			}
			sawSubscribed = true
		} else {
			replayed++
		}
	}
	if !sawError || !sawSubscribed || replayed != 2 {
		t.Fatalf("error=%v subscribed=%v replayed=%d, want true/true/2", sawError, sawSubscribed, replayed)
	}

	// "a" must not be registered: publishes produce no frame on this conn.
	if _, _, err := h.Publish(context.Background(), "a", json.RawMessage(`"x"`), nil); err != nil {
		t.Fatalf("Publish to a: %v", err)
	}
	assertNoFrame(t, conn)
}

// --- AK-6 / AK-7: replay paging and gap ---

func TestHub_ReplayHistory_PagesPastSingleBatch(t *testing.T) {
	h, _ := newTestHubWithCursorStore(t)
	h.config.HistoryLimit = 2
	publishN(t, h, "ch", 5)

	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{AfterSeq: map[string]int64{"ch": 0}})

	seen := 0
	for i := 0; i < 6; i++ {
		if strings.Contains(string(drainFrame(t, conn)), `"type":"message"`) {
			seen++
		}
	}
	if seen != 5 {
		t.Fatalf("replayed %d messages, want all 5 across batches", seen)
	}
}

func TestHub_ReplayHistory_GapThenAvailableMessages(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	publishN(t, h, "ch", 5)

	// Simulate eviction of seq 1..3: the earliest available becomes 4.
	ms.mockStore.mu.Lock()
	ms.mockStore.messages["ch"] = ms.mockStore.messages["ch"][3:]
	ms.mockStore.mu.Unlock()

	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{AfterSeq: map[string]int64{"ch": 0}})

	var gap GapFrame
	if err := json.Unmarshal(drainFrame(t, conn), &gap); err != nil {
		t.Fatalf("unmarshal gap frame: %v", err)
	}
	if gap.Type != FrameTypeGap || gap.AvailableFromSeq != 4 {
		t.Fatalf("gap frame = %+v, want gap available_from_seq 4", gap)
	}
	for want := int64(4); want <= 5; want++ {
		var mf MessageFrame
		if err := json.Unmarshal(drainFrame(t, conn), &mf); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if mf.SeqID != want {
			t.Fatalf("seq = %d, want %d after gap", mf.SeqID, want)
		}
	}
}

// --- AK-11: flusher lifecycle ---

func TestHub_Flusher_FinalFlushOnCancel(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	h.config.AckFlushInterval = time.Hour // only the final flush can run

	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn)
	h.Ack(conn, map[string]int64{"ch": 42})

	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	cancel()
	h.Wait()

	if seq, ok := ms.cursor(conn.SubscriberID, "ch"); !ok || seq != 42 {
		t.Fatalf("final flush cursor = %d (ok=%v), want 42", seq, ok)
	}
}

func TestHub_Flusher_FlushesOnConnectionRemoval(t *testing.T) {
	h, ms := newTestHubWithCursorStore(t)
	h.config.AckFlushInterval = time.Hour // only the removal signal can trigger

	conn := newTestConnection(t, "c1")
	h.Subscribe(conn, []string{"ch"}, SubscribeOptions{})
	drainFrame(t, conn)
	h.Ack(conn, map[string]int64{"ch": 11})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		h.Wait()
	}()
	h.Start(ctx)

	h.RemoveConnection(conn)
	waitForCursor(t, ms, conn.SubscriberID, "ch", 11)
}

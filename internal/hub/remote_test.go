package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

func newClusterTestHub(t testing.TB, nodeID string) (*hubImpl, *mockStore) {
	t.Helper()
	st := newMockStore()
	a := newMockAuth()
	cfg := HubConfig{
		OutboundBufferSize:      16,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
		NodeID:                  nodeID,
	}
	h := New(st, a, cfg, NopMetrics()).(*hubImpl)
	return h, st
}

// seed stages pre-built messages, allowing tests to set specific origins.
func (m *mockStore) seed(channel string, msgs ...store.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages[channel] = append(m.messages[channel], msgs...)
	for _, msg := range msgs {
		if msg.SeqID > m.nextSeq[channel] {
			m.nextSeq[channel] = msg.SeqID
		}
	}
}

func TestHub_HasSubscribers(t *testing.T) {
	h, _ := newTestHub(t)
	conn := newTestConnection(t, "c1")

	if h.HasSubscribers("chan.a") {
		t.Fatal("HasSubscribers = true before any subscription")
	}
	if err := h.Subscribe(conn, []string{"chan.a"}, SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if !h.HasSubscribers("chan.a") {
		t.Fatal("HasSubscribers = false after Subscribe")
	}
	h.RemoveConnection(conn)
	if h.HasSubscribers("chan.a") {
		t.Fatal("HasSubscribers = true after RemoveConnection")
	}
}

func TestHub_DeliverRemoteDeliversAndDedups(t *testing.T) {
	h, st := newClusterTestHub(t, "node-a")
	conn := newTestConnection(t, "c1")
	ctx := context.Background()

	if err := h.Subscribe(conn, []string{"chan.a"}, SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainFrame(t, conn) // subscribed confirmation

	st.seed("chan.a", store.Message{
		SeqID:     1,
		Payload:   json.RawMessage(`{"v":1}`),
		CreatedAt: time.Now(),
		Origin:    "node-b",
	})

	if err := h.DeliverRemote(ctx, "chan.a", 1); err != nil {
		t.Fatalf("DeliverRemote: %v", err)
	}
	var mf MessageFrame
	if err := json.Unmarshal(drainFrame(t, conn), &mf); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if mf.Type != FrameTypeMessage || mf.SeqID != 1 || string(mf.Payload) != `{"v":1}` {
		t.Fatalf("frame = %+v, want message seq 1", mf)
	}

	// A duplicate notification for the same seq is a no-op (cursor dedup).
	if err := h.DeliverRemote(ctx, "chan.a", 1); err != nil {
		t.Fatalf("DeliverRemote duplicate: %v", err)
	}
	assertNoFrame(t, conn)
}

func TestHub_DeliverRemoteEvictedAndReadError(t *testing.T) {
	h, st := newClusterTestHub(t, "node-a")
	conn := newTestConnection(t, "c1")
	ctx := context.Background()

	if err := h.Subscribe(conn, []string{"chan.a"}, SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainFrame(t, conn) // subscribed confirmation

	// A missing message is treated as evicted: no frame, cursor advances past
	// it so catch-up does not retry it forever.
	if err := h.DeliverRemote(ctx, "chan.a", 7); err != nil {
		t.Fatalf("DeliverRemote(evicted): %v", err)
	}
	assertNoFrame(t, conn)
	if cur, ok := h.nodeCursor("chan.a"); !ok || cur != 7 {
		t.Fatalf("node cursor = %d (ok=%v), want 7", cur, ok)
	}

	// Subsequent seqs must still be delivered.
	st.seed("chan.a", store.Message{
		SeqID:     8,
		Payload:   json.RawMessage(`{"v":8}`),
		CreatedAt: time.Now(),
		Origin:    "node-b",
	})
	if err := h.DeliverRemote(ctx, "chan.a", 8); err != nil {
		t.Fatalf("DeliverRemote(after evicted): %v", err)
	}
	var mf MessageFrame
	if err := json.Unmarshal(drainFrame(t, conn), &mf); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if mf.SeqID != 8 {
		t.Fatalf("frame seq = %d, want 8", mf.SeqID)
	}

	// Store failures propagate so the listener can log them.
	st.readErr = errors.New("boom")
	if err := h.DeliverRemote(ctx, "chan.a", 9); err == nil {
		t.Fatal("expected store error to propagate, got nil")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want the store error", err)
	}
}

func TestHub_CatchUpDeliversMissedAndSkipsSelfOrigin(t *testing.T) {
	h, st := newClusterTestHub(t, "node-a")
	h.config.HistoryLimit = 2 // force paging across batches
	conn := newTestConnection(t, "c1")

	if err := h.Subscribe(conn, []string{"chan.a"}, SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainFrame(t, conn) // subscribed confirmation

	now := time.Now()
	st.seed("chan.a",
		store.Message{SeqID: 1, Payload: json.RawMessage(`{"v":1}`), CreatedAt: now, Origin: "node-b"},
		store.Message{SeqID: 2, Payload: json.RawMessage(`{"v":2}`), CreatedAt: now, Origin: "node-a"},
		store.Message{SeqID: 3, Payload: json.RawMessage(`{"v":3}`), CreatedAt: now, Origin: "node-c"},
		store.Message{SeqID: 4, Payload: json.RawMessage(`{"v":4}`), CreatedAt: now, Origin: "node-b"},
		store.Message{SeqID: 5, Payload: json.RawMessage(`{"v":5}`), CreatedAt: now, Origin: "node-b"},
	)

	if err := h.CatchUp(context.Background()); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	for _, want := range []int64{1, 3, 4, 5} {
		var mf MessageFrame
		if err := json.Unmarshal(drainFrame(t, conn), &mf); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if mf.Type != FrameTypeMessage || mf.SeqID != want {
			t.Fatalf("frame = %+v, want message seq %d", mf, want)
		}
	}
	assertNoFrame(t, conn)

	if cur, _ := h.nodeCursor("chan.a"); cur != 5 {
		t.Fatalf("node cursor = %d, want 5", cur)
	}
}

func TestHub_CatchUpSendsGapWhenWindowAdvanced(t *testing.T) {
	h, st := newClusterTestHub(t, "node-a")
	conn := newTestConnection(t, "c1")

	if err := h.Subscribe(conn, []string{"chan.a"}, SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainFrame(t, conn) // subscribed confirmation

	// The earliest available message is seq 50 while the cursor sits at 0:
	// messages 1..49 are permanently gone.
	st.seed("chan.a", store.Message{
		SeqID:     50,
		Payload:   json.RawMessage(`{"v":50}`),
		CreatedAt: time.Now(),
		Origin:    "node-b",
	})

	if err := h.CatchUp(context.Background()); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	var gf GapFrame
	if err := json.Unmarshal(drainFrame(t, conn), &gf); err != nil {
		t.Fatalf("unmarshal gap frame: %v", err)
	}
	if gf.Type != FrameTypeGap || gf.AvailableFromSeq != 50 || gf.RequestedFromSeq != 0 {
		t.Fatalf("gap frame = %+v, want available_from 50 requested_from 0", gf)
	}

	var mf MessageFrame
	if err := json.Unmarshal(drainFrame(t, conn), &mf); err != nil {
		t.Fatalf("unmarshal message frame: %v", err)
	}
	if mf.SeqID != 50 {
		t.Fatalf("message seq = %d, want 50", mf.SeqID)
	}
}

func TestHub_CatchUpSkipsChannelWithoutCursor(t *testing.T) {
	h, st := newClusterTestHub(t, "node-a")
	conn := newTestConnection(t, "c1")

	// Register the connection directly so the channel has a subscriber but no
	// node cursor (as if the subscribe-time init had failed).
	h.mu.Lock()
	h.channels["chan.nocursor"] = map[string]*Connection{conn.ID: conn}
	h.mu.Unlock()

	st.seed("chan.nocursor", store.Message{
		SeqID:     1,
		Payload:   json.RawMessage(`{}`),
		CreatedAt: time.Now(),
		Origin:    "node-b",
	})

	if err := h.CatchUp(context.Background()); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	assertNoFrame(t, conn)
}

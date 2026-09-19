package cluster

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

// fakeDeliverer records every call the listener makes. It is safe for
// concurrent use because integration tests read the records while the
// listener goroutine writes them.
type fakeDeliverer struct {
	mu             sync.Mutex
	hasSubscribers bool
	deliverErr     error

	hasCalls     []string
	deliverCalls []deliverCall
}

type deliverCall struct {
	channel string
	seqID   int64
}

func (f *fakeDeliverer) HasSubscribers(channel string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasCalls = append(f.hasCalls, channel)
	return f.hasSubscribers
}

func (f *fakeDeliverer) DeliverRemote(ctx context.Context, channel string, seqID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliverCalls = append(f.deliverCalls, deliverCall{channel: channel, seqID: seqID})
	return f.deliverErr
}

func (f *fakeDeliverer) CatchUp(ctx context.Context) error { return nil }

func (f *fakeDeliverer) hasCallsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hasCalls...)
}

func (f *fakeDeliverer) deliverCallsSnapshot() []deliverCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deliverCall(nil), f.deliverCalls...)
}

func mustEncodeEvent(t *testing.T, nodeID, channel string, seqID int64) string {
	t.Helper()
	payload, err := store.EncodeMessageEvent(store.MessageEvent{NodeID: nodeID, Channel: channel, SeqID: seqID})
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return payload
}

func TestHandleNotification(t *testing.T) {
	tests := []struct {
		name          string
		payload       string
		selfNode      string
		subscribers   bool
		wantHasCalls  int
		wantDelivered []deliverCall
	}{
		{
			name:          "delivers to a channel with local subscribers",
			payload:       mustEncodeEvent(t, "node-b", "chan.a", 5),
			selfNode:      "node-a",
			subscribers:   true,
			wantHasCalls:  1,
			wantDelivered: []deliverCall{{channel: "chan.a", seqID: 5}},
		},
		{
			name:         "skips its own notification without checking subscribers",
			payload:      mustEncodeEvent(t, "node-a", "chan.a", 5),
			selfNode:     "node-a",
			subscribers:  true,
			wantHasCalls: 0,
		},
		{
			name:         "skips channels without local subscribers",
			payload:      mustEncodeEvent(t, "node-b", "chan.a", 5),
			selfNode:     "node-a",
			subscribers:  false,
			wantHasCalls: 1,
		},
		{
			name:         "discards malformed payloads without touching the deliverer",
			payload:      `{"node_id":`,
			selfNode:     "node-a",
			subscribers:  true,
			wantHasCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDeliverer{hasSubscribers: tt.subscribers}
			l := New(Config{NodeID: tt.selfNode}, d, slog.New(slog.DiscardHandler))

			l.handleNotification(context.Background(), tt.payload)

			if hasCalls := d.hasCallsSnapshot(); len(hasCalls) != tt.wantHasCalls {
				t.Errorf("HasSubscribers calls = %v, want %d calls", hasCalls, tt.wantHasCalls)
			}
			delivered := d.deliverCallsSnapshot()
			if len(delivered) != len(tt.wantDelivered) {
				t.Fatalf("DeliverRemote calls = %+v, want %+v", delivered, tt.wantDelivered)
			}
			for i, want := range tt.wantDelivered {
				if delivered[i] != want {
					t.Errorf("DeliverRemote call %d = %+v, want %+v", i, delivered[i], want)
				}
			}
		})
	}
}

func TestHandleNotificationDeliveryErrorIsContained(t *testing.T) {
	d := &fakeDeliverer{hasSubscribers: true, deliverErr: errors.New("boom")}
	l := New(Config{NodeID: "node-a"}, d, slog.New(slog.DiscardHandler))

	// The failure must be logged and swallowed: the listener keeps consuming.
	l.handleNotification(context.Background(), mustEncodeEvent(t, "node-b", "chan.a", 7))

	delivered := d.deliverCallsSnapshot()
	if len(delivered) != 1 || delivered[0].seqID != 7 {
		t.Fatalf("DeliverRemote calls = %+v, want one call for seq 7", delivered)
	}
}

func TestNewDefaults(t *testing.T) {
	l := New(Config{NodeID: "node-a"}, &fakeDeliverer{}, nil)

	if l.cfg.ReconnectBase != time.Second {
		t.Errorf("ReconnectBase = %v, want 1s", l.cfg.ReconnectBase)
	}
	if l.cfg.ReconnectMax != 30*time.Second {
		t.Errorf("ReconnectMax = %v, want 30s", l.cfg.ReconnectMax)
	}
	if l.logger == nil {
		t.Error("logger is nil, want default logger")
	}
}

func TestNextBackoff(t *testing.T) {
	tests := []struct {
		current time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{time.Second, 30 * time.Second, 2 * time.Second},
		{20 * time.Second, 30 * time.Second, 30 * time.Second},
		{30 * time.Second, 30 * time.Second, 30 * time.Second},
	}

	for _, tt := range tests {
		if got := nextBackoff(tt.current, tt.max); got != tt.want {
			t.Errorf("nextBackoff(%v, %v) = %v, want %v", tt.current, tt.max, got, tt.want)
		}
	}
}

func TestTruncateAppName(t *testing.T) {
	long := "aether-cluster-" + strings.Repeat("x", 80)
	if got := truncateAppName(long); len(got) != 63 {
		t.Errorf("truncated length = %d, want 63", len(got))
	}

	short := "aether-cluster-node-a"
	if got := truncateAppName(short); got != short {
		t.Errorf("short name changed: %q", got)
	}
}

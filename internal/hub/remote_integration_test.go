//go:build integration

package hub

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/store"
	"github.com/aether-mq/aether/internal/store/storetest"
)

func integNewClusterStore(t *testing.T, nodeID string) store.Store {
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

	st, err := store.New(ctx, dbCfg, retCfg, store.Options{NodeID: nodeID})
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if err := storetest.TruncateAll(ctx, testDSN()); err != nil {
		t.Fatalf("truncate test tables: %v", err)
	}
	return st
}

func integNewClusterHub(t *testing.T, st store.Store, nodeID string) *hubImpl {
	t.Helper()
	a := integNewTestAuth(t, st.(store.KeyStore))
	cfg := HubConfig{
		OutboundBufferSize:      256,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
		NodeID:                  nodeID,
	}
	return New(st, a, cfg, NopMetrics()).(*hubImpl)
}

// integPublishStore writes directly through the store, as a peer node would.
func integPublishStore(t *testing.T, st store.Store, channel, payload string) int64 {
	t.Helper()
	seq, _, err := st.WriteMessage(context.Background(), channel, json.RawMessage(payload), nil)
	if err != nil {
		t.Fatalf("store write to %s: %v", channel, err)
	}
	return seq
}

func TestIntegration_DeliverRemote_DeduplicatesAndSkipsEvicted(t *testing.T) {
	st := integNewClusterStore(t, "node-b")
	h := integNewClusterHub(t, st, "node-a")
	conn := integNewTestConnection(t, "remote-1")
	ctx := context.Background()

	integSubscribe(t, h, conn, []string{"int.remote"}, nil)
	integDrainFrame(t, conn) // subscribed confirmation

	integPublishStore(t, st, "int.remote", `{"v":1}`)
	integPublishStore(t, st, "int.remote", `{"v":2}`)

	if err := h.DeliverRemote(ctx, "int.remote", 1); err != nil {
		t.Fatalf("DeliverRemote(1): %v", err)
	}
	var mf MessageFrame
	if err := json.Unmarshal(integDrainFrame(t, conn), &mf); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if mf.Type != FrameTypeMessage || mf.SeqID != 1 || string(mf.Payload) != `{"v":1}` {
		t.Fatalf("frame = %+v, want message seq 1 payload {\"v\":1}", mf)
	}

	if err := h.DeliverRemote(ctx, "int.remote", 2); err != nil {
		t.Fatalf("DeliverRemote(2): %v", err)
	}
	if err := json.Unmarshal(integDrainFrame(t, conn), &mf); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if mf.SeqID != 2 {
		t.Fatalf("frame seq = %d, want 2", mf.SeqID)
	}

	// Duplicate notification for an already-delivered seq: no-op.
	if err := h.DeliverRemote(ctx, "int.remote", 2); err != nil {
		t.Fatalf("DeliverRemote duplicate: %v", err)
	}
	integAssertNoFrame(t, conn)

	// Unknown (evicted) seq: no-op, no error, no frame.
	if err := h.DeliverRemote(ctx, "int.remote", 99); err != nil {
		t.Fatalf("DeliverRemote(99): %v", err)
	}
	integAssertNoFrame(t, conn)
}

func TestIntegration_CatchUp_DeliversMissedMessagesOnce(t *testing.T) {
	st := integNewClusterStore(t, "node-b")
	h := integNewClusterHub(t, st, "node-a")
	conn := integNewTestConnection(t, "catchup-1")
	ctx := context.Background()

	// Subscribe while the channel is empty: cursors anchor at 0.
	integSubscribe(t, h, conn, []string{"int.catchup"}, nil)
	integDrainFrame(t, conn) // subscribed confirmation

	// Messages published by the peer node while this node's LISTEN was down.
	for i := 0; i < 3; i++ {
		integPublishStore(t, st, "int.catchup", `{"v":1}`)
	}

	if err := h.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	for want := int64(1); want <= 3; want++ {
		var mf MessageFrame
		if err := json.Unmarshal(integDrainFrame(t, conn), &mf); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if mf.Type != FrameTypeMessage || mf.SeqID != want {
			t.Fatalf("frame = %+v, want message seq %d", mf, want)
		}
	}
	integAssertNoFrame(t, conn)

	// A second catch-up must not re-deliver anything.
	if err := h.CatchUp(ctx); err != nil {
		t.Fatalf("second CatchUp: %v", err)
	}
	integAssertNoFrame(t, conn)
}

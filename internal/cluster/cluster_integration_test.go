//go:build integration

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/store"
	"github.com/aether-mq/aether/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

func testDSN() string {
	if dsn := os.Getenv("AETHER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://aether:aether@localhost:5433/aether_test?sslmode=disable"
}

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

func integNewHub(t *testing.T, st store.Store, nodeID string) hub.Hub {
	t.Helper()
	a, err := auth.New(&config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  30 * time.Second,
	}, st.(store.KeyStore))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return hub.New(st, a, hub.HubConfig{
		OutboundBufferSize:      256,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
		NodeID:                  nodeID,
	}, hub.NopMetrics())
}

func integNewConn(t *testing.T, id string) *hub.Connection {
	t.Helper()
	return hub.NewConnection(id, "sub-"+id, &auth.Claims{Subject: "sub-" + id, Channels: []string{"*"}}, 256)
}

func integSubscribe(t *testing.T, h hub.Hub, conn *hub.Connection, channel string) {
	t.Helper()
	if err := h.Subscribe(conn, []string{channel}, nil); err != nil {
		t.Fatalf("Subscribe to %s: %v", channel, err)
	}
}

func integDrainFrame(t *testing.T, conn *hub.Connection, timeout time.Duration) []byte {
	t.Helper()
	select {
	case data := <-conn.Send:
		return data
	case <-time.After(timeout):
		t.Fatal("timeout waiting for frame")
		return nil
	}
}

func integReceiveMessage(t *testing.T, conn *hub.Connection, timeout time.Duration) hub.MessageFrame {
	t.Helper()
	var mf hub.MessageFrame
	if err := json.Unmarshal(integDrainFrame(t, conn, timeout), &mf); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if mf.Type != hub.FrameTypeMessage {
		t.Fatalf("frame type = %q, want message (%+v)", mf.Type, mf)
	}
	return mf
}

func integAssertNoFrame(t *testing.T, conn *hub.Connection) {
	t.Helper()
	select {
	case data := <-conn.Send:
		t.Fatalf("unexpected frame: %s", string(data))
	default:
	}
}

func integWrite(t *testing.T, st store.Store, channel, payload string) int64 {
	t.Helper()
	seq, _, err := st.WriteMessage(context.Background(), channel, json.RawMessage(payload), nil)
	if err != nil {
		t.Fatalf("store write to %s: %v", channel, err)
	}
	return seq
}

// integStartListener runs a listener for the duration of the test. The
// cleanup cancels it and waits for Run to return before other cleanups (such
// as closing the store) run: cleanups are LIFO and the listener is started
// after the store, so it must stop first.
func integStartListener(t *testing.T, d Deliverer, nodeID string, base, max time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	l := New(Config{DSN: testDSN(), NodeID: nodeID, ReconnectBase: base, ReconnectMax: max}, d, slog.New(slog.DiscardHandler))
	go func() {
		defer close(done)
		if err := l.Run(ctx); err != nil {
			slog.Default().Debug("cluster listener stopped", "node_id", nodeID, "err", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("cluster listener %s did not stop within 5s", nodeID)
		}
	})
}

func appNameFor(nodeID string) string { return "aether-cluster-" + nodeID }

// integWaitForListenerPID waits until the listener's backend is visible in
// pg_stat_activity with a pid different from oldPID, proving a (re)connection
// was established.
func integWaitForListenerPID(t *testing.T, admin *pgx.Conn, nodeID string, oldPID int32, timeout time.Duration) int32 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var pid int32
		err := admin.QueryRow(ctx,
			`SELECT pid FROM pg_stat_activity WHERE application_name = $1`, appNameFor(nodeID)).Scan(&pid)
		if err == nil && pid != oldPID {
			return pid
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("query pg_stat_activity: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("listener %s did not establish a connection within %v", nodeID, timeout)
	return 0
}

func integConnectAdmin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testDSN())
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close admin: %v", err)
		}
	})
	return conn
}

// --- tests ---

// TestIntegration_TwoNodeFanout covers CL-2 and CL-5: a publish on one node
// reaches the other node's subscriber, and each node delivers exactly once.
func TestIntegration_TwoNodeFanout(t *testing.T) {
	ctx := context.Background()

	stA := integNewClusterStore(t, "twin-a")
	stB := integNewClusterStore(t, "twin-b")
	hubA := integNewHub(t, stA, "twin-a")
	hubB := integNewHub(t, stB, "twin-b")

	admin := integConnectAdmin(t)
	integStartListener(t, hubA.(Deliverer), "twin-a", 100*time.Millisecond, 500*time.Millisecond)
	integStartListener(t, hubB.(Deliverer), "twin-b", 100*time.Millisecond, 500*time.Millisecond)
	integWaitForListenerPID(t, admin, "twin-a", 0, 5*time.Second)
	integWaitForListenerPID(t, admin, "twin-b", 0, 5*time.Second)

	connA := integNewConn(t, "twin-a1")
	integSubscribe(t, hubA, connA, "int.twin")
	integDrainFrame(t, connA, time.Second) // subscribed confirmation

	connB := integNewConn(t, "twin-b1")
	integSubscribe(t, hubB, connB, "int.twin")
	integDrainFrame(t, connB, time.Second) // subscribed confirmation

	seqID, _, err := hubA.Publish(ctx, "int.twin", json.RawMessage(`{"v":1}`), nil)
	if err != nil {
		t.Fatalf("publish on node A: %v", err)
	}

	mfA := integReceiveMessage(t, connA, 2*time.Second)
	if mfA.Channel != "int.twin" || mfA.SeqID != seqID || string(mfA.Payload) != `{"v":1}` {
		t.Fatalf("local frame = %+v, want channel int.twin seq %d", mfA, seqID)
	}

	mfB := integReceiveMessage(t, connB, 2*time.Second)
	if mfB.Channel != "int.twin" || mfB.SeqID != seqID || string(mfB.Payload) != `{"v":1}` {
		t.Fatalf("remote frame = %+v, want channel int.twin seq %d", mfB, seqID)
	}

	// Exactly once on both nodes: the self notification must not cause a
	// second delivery on A, and the notification must not duplicate on B.
	time.Sleep(300 * time.Millisecond)
	integAssertNoFrame(t, connA)
	integAssertNoFrame(t, connB)
}

// TestIntegration_ListenerSkipsChannelsWithoutSubscribers covers CL-3: a
// notification for a channel with no local subscriber is skipped without any
// read-back.
func TestIntegration_ListenerSkipsChannelsWithoutSubscribers(t *testing.T) {
	stA := integNewClusterStore(t, "skip-a")
	admin := integConnectAdmin(t)

	d := &fakeDeliverer{hasSubscribers: false}
	integStartListener(t, d, "skip-obs", 100*time.Millisecond, 500*time.Millisecond)
	integWaitForListenerPID(t, admin, "skip-obs", 0, 5*time.Second)

	integWrite(t, stA, "skip.no.subs", `{"v":1}`)

	consulted := func() bool {
		for _, ch := range d.hasCallsSnapshot() {
			if ch == "skip.no.subs" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !consulted() {
		time.Sleep(25 * time.Millisecond)
	}
	if !consulted() {
		t.Fatal("listener never consulted HasSubscribers for skip.no.subs")
	}
	if calls := d.deliverCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("DeliverRemote was called %+v, want no read-back", calls)
	}
}

// TestIntegration_ReconnectCatchUp covers CL-7 and CL-8: killing the LISTEN
// session and publishing during the outage must be recovered by the reconnect
// catch-up, exactly once.
func TestIntegration_ReconnectCatchUp(t *testing.T) {
	ctx := context.Background()

	stA := integNewClusterStore(t, "recon-a")
	stB := integNewClusterStore(t, "recon-b")
	hubB := integNewHub(t, stB, "recon-b")

	admin := integConnectAdmin(t)
	integStartListener(t, hubB.(Deliverer), "recon-b", 100*time.Millisecond, 500*time.Millisecond)
	pid := integWaitForListenerPID(t, admin, "recon-b", 0, 5*time.Second)

	connB := integNewConn(t, "recon-b1")
	integSubscribe(t, hubB, connB, "int.reconnect")
	integDrainFrame(t, connB, time.Second) // subscribed confirmation

	// Steady state: the notification path delivers.
	integWrite(t, stA, "int.reconnect", `{"n":1}`)
	if mf := integReceiveMessage(t, connB, 2*time.Second); mf.SeqID != 1 {
		t.Fatalf("steady-state frame seq = %d, want 1", mf.SeqID)
	}

	// Kill the LISTEN session, then publish while it is down: these
	// notifications are lost by PostgreSQL and must be recovered by catch-up.
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate listener backend: %v", err)
	}
	integWrite(t, stA, "int.reconnect", `{"n":2}`)
	integWrite(t, stA, "int.reconnect", `{"n":3}`)

	integWaitForListenerPID(t, admin, "recon-b", pid, 5*time.Second)
	if mf := integReceiveMessage(t, connB, 5*time.Second); mf.SeqID != 2 {
		t.Fatalf("catch-up frame seq = %d, want 2", mf.SeqID)
	}
	if mf := integReceiveMessage(t, connB, 5*time.Second); mf.SeqID != 3 {
		t.Fatalf("catch-up frame seq = %d, want 3", mf.SeqID)
	}
	integAssertNoFrame(t, connB)

	// Post-recovery publishes flow through the notification path again.
	integWrite(t, stA, "int.reconnect", `{"n":4}`)
	if mf := integReceiveMessage(t, connB, 2*time.Second); mf.SeqID != 4 {
		t.Fatalf("post-recovery frame seq = %d, want 4", mf.SeqID)
	}
	integAssertNoFrame(t, connB)
}

// TestIntegration_ConcurrentPublishBothNodes covers CL-9: concurrent publishes
// from both nodes are seen exactly once by each node's subscriber, with no
// duplicates and no holes (ordering is not asserted: documented boundary).
func TestIntegration_ConcurrentPublishBothNodes(t *testing.T) {
	ctx := context.Background()

	stA := integNewClusterStore(t, "conc-a")
	stB := integNewClusterStore(t, "conc-b")
	hubA := integNewHub(t, stA, "conc-a")
	hubB := integNewHub(t, stB, "conc-b")

	admin := integConnectAdmin(t)
	integStartListener(t, hubA.(Deliverer), "conc-a", 100*time.Millisecond, 500*time.Millisecond)
	integStartListener(t, hubB.(Deliverer), "conc-b", 100*time.Millisecond, 500*time.Millisecond)
	integWaitForListenerPID(t, admin, "conc-a", 0, 5*time.Second)
	integWaitForListenerPID(t, admin, "conc-b", 0, 5*time.Second)

	connA := integNewConn(t, "conc-a1")
	integSubscribe(t, hubA, connA, "int.conc")
	integDrainFrame(t, connA, time.Second)

	connB := integNewConn(t, "conc-b1")
	integSubscribe(t, hubB, connB, "int.conc")
	integDrainFrame(t, connB, time.Second)

	const perNode = 100

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	publishAll := func(h hub.Hub) {
		defer wg.Done()
		for i := 0; i < perNode; i++ {
			if _, _, err := h.Publish(ctx, "int.conc", json.RawMessage(`{"v":1}`), nil); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
		}
	}
	wg.Add(2)
	go publishAll(hubA)
	go publishAll(hubB)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("concurrent publish failed: %v", firstErr)
	}

	// Collect on the test goroutine so t.Fatal stays on the main goroutine.
	collect := func(conn *hub.Connection) map[int64]int {
		seen := make(map[int64]int)
		for total := 0; total < 2*perNode; total++ {
			mf := integReceiveMessage(t, conn, 10*time.Second)
			seen[mf.SeqID]++
		}
		return seen
	}

	for name, conn := range map[string]*hub.Connection{"A": connA, "B": connB} {
		seen := collect(conn)
		if len(seen) != 2*perNode {
			t.Fatalf("node %s saw %d distinct seqs, want %d", name, len(seen), 2*perNode)
		}
		for seq := int64(1); seq <= 2*perNode; seq++ {
			if seen[seq] != 1 {
				t.Fatalf("node %s: seq %d received %d times, want exactly once", name, seq, seen[seq])
			}
		}
	}
}

// TestIntegration_RunReturnsAfterCancel covers CL-12: Run must return promptly
// when cancelled, both while connected and while waiting to reconnect.
func TestIntegration_RunReturnsAfterCancel(t *testing.T) {
	t.Run("while connected", func(t *testing.T) {
		st := integNewClusterStore(t, "canc-a")
		h := integNewHub(t, st, "canc-a")
		admin := integConnectAdmin(t)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		l := New(Config{
			DSN:           testDSN(),
			NodeID:        "canc-a",
			ReconnectBase: 100 * time.Millisecond,
			ReconnectMax:  500 * time.Millisecond,
		}, h.(Deliverer), slog.New(slog.DiscardHandler))
		go func() { done <- l.Run(ctx) }()

		integWaitForListenerPID(t, admin, "canc-a", 0, 5*time.Second)
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned error on cancel: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return within 2s of cancellation")
		}
	})

	t.Run("while reconnecting", func(t *testing.T) {
		// An unreachable DSN keeps the listener in its backoff loop.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		l := New(Config{
			DSN:           "postgres://127.0.0.1:1/aether?sslmode=disable&connect_timeout=1",
			NodeID:        "canc-b",
			ReconnectBase: 200 * time.Millisecond,
			ReconnectMax:  time.Second,
		}, &fakeDeliverer{}, slog.New(slog.DiscardHandler))
		go func() { done <- l.Run(ctx) }()

		time.Sleep(100 * time.Millisecond) // let the first connect attempt fail
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned error on cancel: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return within 2s of cancellation during backoff")
		}
	})
}

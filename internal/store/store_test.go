//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/store/storetest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func testDSN() string {
	if dsn := os.Getenv("AETHER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://aether:aether@localhost:5433/aether_test?sslmode=disable"
}

func newTestStore(t *testing.T) *pgStore {
	t.Helper()
	return newTestStoreWithRetention(t, &config.RetentionConfig{
		DefaultTTL:      720 * time.Hour,
		DefaultMaxCount: 10000,
		EvictionInterval: 5 * time.Minute,
		Rules: []config.RetentionRule{
			{Pattern: "alerts.*", TTL: 24 * time.Hour, MaxCount: 5000},
			{Pattern: "shortlived", TTL: 1 * time.Hour, MaxCount: 100},
		},
	})
}

func newTestStoreWithRetention(t *testing.T, retCfg *config.RetentionConfig, opts ...Options) *pgStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbCfg := &config.DatabaseConfig{
		DSN:             testDSN(),
		MaxOpenConns:    10,
		ConnMaxIdleTime: time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
	}

	st, err := New(ctx, dbCfg, retCfg, opts...)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return st.(*pgStore)
}

func newClusterTestStore(t *testing.T, nodeID string) *pgStore {
	t.Helper()
	return newTestStoreWithRetention(t, &config.RetentionConfig{
		DefaultTTL:       720 * time.Hour,
		DefaultMaxCount:  10000,
		EvictionInterval: 5 * time.Minute,
	}, Options{NodeID: nodeID})
}

// newNotifyWitness opens a raw LISTEN connection observing cross-node
// notifications independently of any store under test.
func newNotifyWitness(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testDSN())
	if err != nil {
		t.Fatalf("connect witness: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close witness: %v", err)
		}
	})
	if _, err := conn.Exec(context.Background(), "LISTEN "+NotifyChannel); err != nil {
		t.Fatalf("witness LISTEN: %v", err)
	}
	return conn
}

func truncateAll(t *testing.T, s *pgStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, storetest.TruncateStmt); err != nil {
		t.Fatalf("truncate test tables: %v", err)
	}
}

// backdateChannel makes a channel eligible for the empty-channel cleanup,
// which only reclaims rows that have been quiet for an eviction interval
// (the quiet period guards against concurrent publishes, see EvictExpiredMessages).
func backdateChannel(t *testing.T, s *pgStore, channel string, age time.Duration) {
	t.Helper()
	tag, err := s.pool.Exec(context.Background(),
		`UPDATE channels SET updated_at = now() - make_interval(secs => $2) WHERE name = $1`,
		channel, age.Seconds())
	if err != nil {
		t.Fatalf("backdate channel %s: %v", channel, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdate channel %s: %d rows affected, want 1", channel, tag.RowsAffected())
	}
}

// --- S-1: RunMigrations creates all tables and indexes ---

func TestRunMigrations_EmptyDB(t *testing.T) {
	s := newTestStore(t)

	tables := []string{"channels", "messages", "api_keys", "webhooks", "webhook_deliveries", "subscriber_cursors", "schema_migrations"}
	for _, tbl := range tables {
		var exists bool
		err := s.pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tbl,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("table %q not created", tbl)
		}
	}
}

// --- S-1: RunMigrations is idempotent ---

func TestRunMigrations_Idempotent(t *testing.T) {
	s := newTestStore(t)

	if err := s.RunMigrations(context.Background()); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}
}

// --- S-2: Migration versioning ---

func TestRunMigrations_VersionTracking(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.pool.Query(context.Background(),
		`SELECT version FROM schema_migrations ORDER BY version`,
	)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, v)
	}
	if want := []int{1, 2, 3, 4, 5, 6}; !slices.Equal(versions, want) {
		t.Fatalf("expected versions %v, got %v", want, versions)
	}
}

// --- S-3: WriteMessage basic + sequential ---

func TestWriteMessage_Basic(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	seqID, ts, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{"hello":"world"}`), nil)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if seqID != 1 {
		t.Errorf("expected seqID=1, got %d", seqID)
	}
	if ts.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestWriteMessage_Sequential(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	for i := 1; i <= 5; i++ {
		seqID, _, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if seqID != int64(i) {
			t.Errorf("expected seqID=%d, got %d", i, seqID)
		}
	}
}

// --- S-3: WriteMessage concurrency ---

func TestWriteMessage_Concurrent(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	const goroutines = 50
	const writesPer = 20

	var wg sync.WaitGroup
	var mu sync.Mutex
	var seqIDs []int64
	var firstErr error

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := 0; w < writesPer; w++ {
				seqID, _, err := s.WriteMessage(context.Background(), "concurrent", json.RawMessage(`{}`), nil)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				seqIDs = append(seqIDs, seqID)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent write error: %v", firstErr)
	}

	expected := goroutines * writesPer
	if len(seqIDs) != expected {
		t.Fatalf("expected %d seq_ids, got %d", expected, len(seqIDs))
	}

	sort.Slice(seqIDs, func(i, j int) bool { return seqIDs[i] < seqIDs[j] })
	for i, id := range seqIDs {
		if id != int64(i+1) {
			t.Fatalf("expected seq_id %d, got %d", i+1, id)
		}
	}
}

// --- S-4: WriteMessage idempotency ---

func TestWriteMessage_IdempotentConflict(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	key := "unique-key-1"
	seq1, ts1, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{"v":1}`), &key)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	seq2, ts2, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{"v":2}`), &key)
	if err != nil {
		t.Fatalf("idempotent write: %v", err)
	}

	if seq1 != seq2 {
		t.Errorf("seq mismatch: first=%d, idempotent=%d", seq1, seq2)
	}
	if !ts1.Equal(ts2) {
		t.Errorf("timestamp mismatch: first=%v, idempotent=%v", ts1, ts2)
	}
}

// --- S-5: WriteMessage nil idempotency key ---

func TestWriteMessage_NilIdempotencyKey(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	seq1, _, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	seq2, _, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if seq1 == seq2 {
		t.Error("nil key should not deduplicate, but got same seq_id")
	}
}

// --- S-6: ReadHistory basic ---

func TestReadHistory_Basic(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	for i := 0; i < 5; i++ {
		_, _, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)), nil)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	result, err := s.ReadHistory(context.Background(), "test.ch", 2, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(result.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result.Messages))
	}
	for i, m := range result.Messages {
		if m.SeqID != int64(i+3) {
			t.Errorf("message[%d]: expected seq_id=%d, got %d", i, i+3, m.SeqID)
		}
	}
}

// --- S-6: ReadHistory limit cap ---

func TestReadHistory_LimitCap(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	for i := 0; i < 5; i++ {
		_, _, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	result, err := s.ReadHistory(context.Background(), "test.ch", 0, 5000)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	// Only 5 messages exist, so we get 5 back even with limit 5000 (capped to 1000).
	if len(result.Messages) != 5 {
		t.Errorf("expected 5 messages, got %d", len(result.Messages))
	}
}

// --- S-7: ReadHistory channel not found ---

func TestReadHistory_ChannelNotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	result, err := s.ReadHistory(context.Background(), "nonexistent", 0, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Errorf("expected empty slice, got %d messages", len(result.Messages))
	}
}

// --- MinSeq correctness ---

func TestReadHistory_MinSeq(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	for i := 0; i < 5; i++ {
		_, _, err := s.WriteMessage(context.Background(), "test.ch", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	result, err := s.ReadHistory(context.Background(), "test.ch", 2, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if result.MinSeq != 1 {
		t.Errorf("expected MinSeq=1, got %d", result.MinSeq)
	}
}

// --- S-8: Eviction ---

func TestEvict_MaxCount(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	// "shortlived" channel matches retention rule: MaxCount=100
	for i := 0; i < 120; i++ {
		_, _, err := s.WriteMessage(context.Background(), "shortlived", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	cleaned, evicted, err := s.EvictExpiredMessages(context.Background())
	if err != nil {
		t.Fatalf("EvictExpiredMessages: %v", err)
	}
	if evicted != 20 {
		t.Errorf("expected 20 evicted, got %d", evicted)
	}
	if cleaned != 1 {
		t.Errorf("expected 1 cleaned, got %d", cleaned)
	}
}

func TestEvict_EmptyChannelCleanup(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	// Write to a channel, then delete all messages to make it empty.
	for i := 0; i < 3; i++ {
		_, _, err := s.WriteMessage(context.Background(), "temp.ch", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Delete all messages directly.
	_, err := s.pool.Exec(context.Background(), `DELETE FROM messages WHERE channel = $1`, "temp.ch")
	if err != nil {
		t.Fatalf("delete messages: %v", err)
	}

	// The channel must have been quiet for an eviction interval before the
	// cleanup reclaims it.
	backdateChannel(t, s, "temp.ch", time.Hour)

	_, _, err = s.EvictExpiredMessages(context.Background())
	if err != nil {
		t.Fatalf("EvictExpiredMessages: %v", err)
	}

	// Channel should be cleaned up.
	var count int
	err = s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM channels WHERE name = $1`, "temp.ch",
	).Scan(&count)
	if err != nil {
		t.Fatalf("query channels: %v", err)
	}
	if count != 0 {
		t.Errorf("expected channel to be deleted, but it still exists")
	}
}

func TestEvict_RetentionRules(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	// Write to "alerts.cpu" which matches rule Pattern "alerts.*" (MaxCount=5000).
	// Only write 3 messages so none are evicted by max_count.
	for i := 0; i < 3; i++ {
		_, _, err := s.WriteMessage(context.Background(), "alerts.cpu", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	cleaned, evicted, err := s.EvictExpiredMessages(context.Background())
	if err != nil {
		t.Fatalf("EvictExpiredMessages: %v", err)
	}
	if evicted != 0 {
		t.Errorf("expected 0 evicted (within limits), got %d", evicted)
	}
	if cleaned != 0 {
		t.Errorf("expected 0 cleaned, got %d", cleaned)
	}
}

// --- S-9: Ping ---

func TestPing(t *testing.T) {
	s := newTestStore(t)

	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// --- SPEC 7.4.6: empty-channel cleanup must not fail concurrent publishes ---

func TestWriteMessage_EvictPublishRace(t *testing.T) {
	// 1ms TTL makes every published message immediately evictable, so the
	// eviction loop repeatedly empties and reclaims the channel row while
	// publishers race against it.
	retCfg := &config.RetentionConfig{
		DefaultTTL:       time.Millisecond,
		DefaultMaxCount:  10000,
		EvictionInterval: time.Minute,
	}
	s := newTestStoreWithRetention(t, retCfg)
	truncateAll(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var evictWG sync.WaitGroup
	var evictMu sync.Mutex
	var evictErr error
	var totalEvicted int
	evictWG.Add(1)
	go func() {
		defer evictWG.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
			_, evicted, err := s.EvictExpiredMessages(ctx)
			if err != nil && ctx.Err() == nil {
				evictMu.Lock()
				if evictErr == nil {
					evictErr = err
				}
				evictMu.Unlock()
				return
			}
			if evicted > 0 {
				evictMu.Lock()
				totalEvicted += evicted
				evictMu.Unlock()
			}
		}
	}()

	const workers = 8
	const perWorker = 500

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	success := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if _, _, err := s.WriteMessage(context.Background(), "race.evict", json.RawMessage(`{"n":1}`), nil); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	cancel()
	evictWG.Wait()

	if firstErr != nil {
		t.Fatalf("publish failed while eviction ran concurrently: %v", firstErr)
	}
	if want := workers * perWorker; success != want {
		t.Fatalf("expected %d successful publishes, got %d", want, success)
	}
	if evictErr != nil {
		t.Fatalf("concurrent eviction failed: %v", evictErr)
	}
	if totalEvicted == 0 {
		t.Fatal("eviction evicted no messages; the race scenario was not exercised")
	}
}

func TestWriteMessage_ChannelRecreatedAfterEviction(t *testing.T) {
	retCfg := &config.RetentionConfig{
		DefaultTTL:       10 * time.Millisecond,
		DefaultMaxCount:  10000,
		EvictionInterval: time.Minute,
	}
	s := newTestStoreWithRetention(t, retCfg)
	truncateAll(t, s)
	ctx := context.Background()

	seq1, _, err := s.WriteMessage(ctx, "recreate.test", json.RawMessage(`{"v":1}`), nil)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if seq1 != 1 {
		t.Fatalf("expected first seq 1, got %d", seq1)
	}

	// Let the message expire, then run eviction: TTL delete removes the last
	// message and the empty-channel cleanup reclaims the channel row once it
	// has been quiet for an eviction interval.
	time.Sleep(20 * time.Millisecond)
	backdateChannel(t, s, "recreate.test", time.Hour)
	if _, _, err := s.EvictExpiredMessages(ctx); err != nil {
		t.Fatalf("eviction: %v", err)
	}

	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM channels WHERE name = $1`, "recreate.test").Scan(&count); err != nil {
		t.Fatalf("query channel row: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected channel row reclaimed by eviction, got %d rows", count)
	}

	// Publishing after the reclaim must succeed and start a new incarnation.
	seq2, _, err := s.WriteMessage(ctx, "recreate.test", json.RawMessage(`{"v":2}`), nil)
	if err != nil {
		t.Fatalf("publish after channel reclaim: %v", err)
	}
	if seq2 != 1 {
		t.Fatalf("expected seq to restart at 1 after channel reclaim, got %d", seq2)
	}

	hist, err := s.ReadHistory(ctx, "recreate.test", 0, 100)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(hist.Messages) != 1 || hist.Messages[0].SeqID != 1 {
		t.Fatalf("expected exactly the new incarnation message, got %+v", hist.Messages)
	}
}

func TestWriteMessage_InvalidPayloadRollsBack(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)
	ctx := context.Background()

	_, _, err := s.WriteMessage(ctx, "rollback.test", json.RawMessage(`{"broken":`), nil)
	if err == nil {
		t.Fatal("expected invalid JSON payload to fail, got nil error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
		t.Fatalf("expected invalid-JSON SQLSTATE 22P02, got: %v", err)
	}

	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM channels WHERE name = $1`, "rollback.test").Scan(&count); err != nil {
		t.Fatalf("query channel row: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected channel row rolled back with the failed insert, got %d rows", count)
	}
}

// --- v2 第3层：跨节点通知（CL-1 / CL-4）

func TestWriteMessage_NotifyOnlyInClusterMode(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)
	ctx := context.Background()

	witness := newNotifyWitness(t)

	// Positive control: a cluster-mode publish emits a notification carrying
	// only the locating fields.
	cs := newClusterTestStore(t, "node-a")
	if _, _, err := cs.WriteMessage(ctx, "notify.test", json.RawMessage(`{"v":1}`), nil); err != nil {
		t.Fatalf("cluster publish: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := witness.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("expected notification for cluster-mode publish: %v", err)
	}
	if n.Channel != NotifyChannel {
		t.Fatalf("notification channel = %q, want %q", n.Channel, NotifyChannel)
	}
	ev, err := DecodeMessageEvent(n.Payload)
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if want := (MessageEvent{NodeID: "node-a", Channel: "notify.test", SeqID: 1}); ev != want {
		t.Fatalf("event = %+v, want %+v", ev, want)
	}

	// Negative: a single-node publish must produce no notification at all
	// (the wait deadline is the assertion; the witness is not reused after).
	if _, _, err := s.WriteMessage(ctx, "notify.single", json.RawMessage(`{"v":2}`), nil); err != nil {
		t.Fatalf("single-node publish: %v", err)
	}

	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer shortCancel()
	if n, err := witness.WaitForNotification(shortCtx); err == nil {
		t.Fatalf("single-node publish produced notification %+v, want none", n)
	}
}

func TestWriteMessage_NoNotifyOnIdempotentReplay(t *testing.T) {
	cs := newClusterTestStore(t, "node-a")
	truncateAll(t, cs)
	ctx := context.Background()

	witness := newNotifyWitness(t)

	key := "idem-notify-1"
	if _, _, err := cs.WriteMessage(ctx, "notify.idem", json.RawMessage(`{"v":1}`), &key); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := witness.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("expected notification for first publish: %v", err)
	}
	first, err := DecodeMessageEvent(n.Payload)
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}

	seq, _, err := cs.WriteMessage(ctx, "notify.idem", json.RawMessage(`{"v":1}`), &key)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if seq != first.SeqID {
		t.Fatalf("replay seq = %d, want %d (original)", seq, first.SeqID)
	}

	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer shortCancel()
	if n, err := witness.WaitForNotification(shortCtx); err == nil {
		t.Fatalf("idempotent replay produced notification %+v, want none", n)
	}
}

func TestWriteMessage_NoNotifyOnFailedWrite(t *testing.T) {
	cs := newClusterTestStore(t, "node-a")
	truncateAll(t, cs)
	ctx := context.Background()

	witness := newNotifyWitness(t)

	if _, _, err := cs.WriteMessage(ctx, "notify.fail", json.RawMessage(`{"broken":`), nil); err == nil {
		t.Fatal("expected invalid payload to fail, got nil error")
	}

	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer shortCancel()
	if n, err := witness.WaitForNotification(shortCtx); err == nil {
		t.Fatalf("failed write produced notification %+v, want none", n)
	}
}

func TestWriteMessage_NoNotifyWhenTransactionAbortsAfterNotify(t *testing.T) {
	cs := newClusterTestStore(t, "node-a")
	truncateAll(t, cs)
	ctx := context.Background()

	// Fail the seq-advance UPDATE (step 5, which runs after pg_notify) so the
	// notification is queued and then discarded by the rollback. A trigger is
	// the only way to fail that late.
	if _, err := cs.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION aether_test_fail_update() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'test-induced failure'; END;
		$$ LANGUAGE plpgsql;`); err != nil {
		t.Fatalf("create failure function: %v", err)
	}
	if _, err := cs.pool.Exec(ctx, `
		CREATE TRIGGER aether_test_fail_update_trigger
		BEFORE UPDATE ON channels FOR EACH ROW
		EXECUTE FUNCTION aether_test_fail_update()`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := cs.pool.Exec(context.Background(),
			`DROP TRIGGER IF EXISTS aether_test_fail_update_trigger ON channels`); err != nil {
			t.Errorf("drop failure trigger: %v", err)
		}
		if _, err := cs.pool.Exec(context.Background(),
			`DROP FUNCTION IF EXISTS aether_test_fail_update()`); err != nil {
			t.Errorf("drop failure function: %v", err)
		}
	})

	witness := newNotifyWitness(t)

	if _, _, err := cs.WriteMessage(ctx, "notify.abort", json.RawMessage(`{"v":1}`), nil); err == nil {
		t.Fatal("expected publish to fail under the failure trigger")
	}

	var count int
	if err := cs.pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE channel = $1`, "notify.abort").Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected rolled-back write, found %d messages", count)
	}

	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer shortCancel()
	if n, err := witness.WaitForNotification(shortCtx); err == nil {
		t.Fatalf("aborted transaction produced notification %+v, want none", n)
	}
}

func TestWriteMessage_OriginStampedInClusterMode(t *testing.T) {
	cs := newClusterTestStore(t, "node-origin")
	truncateAll(t, cs)
	ctx := context.Background()

	if _, _, err := cs.WriteMessage(ctx, "origin.test", json.RawMessage(`{"v":1}`), nil); err != nil {
		t.Fatalf("cluster publish: %v", err)
	}

	hist, err := cs.ReadHistory(ctx, "origin.test", 0, 10)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(hist.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(hist.Messages))
	}
	if hist.Messages[0].Origin != "node-origin" {
		t.Errorf("cluster Origin = %q, want node-origin", hist.Messages[0].Origin)
	}

	msg, err := cs.ReadMessage(ctx, "origin.test", hist.Messages[0].SeqID)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Origin != "node-origin" {
		t.Errorf("ReadMessage Origin = %q, want node-origin", msg.Origin)
	}

	s := newTestStore(t)
	if _, _, err := s.WriteMessage(ctx, "origin.single", json.RawMessage(`{"v":1}`), nil); err != nil {
		t.Fatalf("single-node publish: %v", err)
	}
	hist, err = s.ReadHistory(ctx, "origin.single", 0, 10)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(hist.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(hist.Messages))
	}
	if hist.Messages[0].Origin != "" {
		t.Errorf("single-node Origin = %q, want empty", hist.Messages[0].Origin)
	}
}

// --- v2 第3层：ReadMessage / LatestSeq ---

func TestReadMessage(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)
	ctx := context.Background()

	seq, ts, err := s.WriteMessage(ctx, "read.one", json.RawMessage(`{"v":7}`), nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	msg, err := s.ReadMessage(ctx, "read.one", seq)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.SeqID != seq {
		t.Errorf("SeqID = %d, want %d", msg.SeqID, seq)
	}
	// jsonb round-trips normalise whitespace, so compare semantically.
	var got map[string]int
	if err := json.Unmarshal(msg.Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(got) != 1 || got["v"] != 7 {
		t.Errorf("Payload = %s, want {\"v\":7}", msg.Payload)
	}
	if !msg.CreatedAt.Equal(ts) {
		t.Errorf("CreatedAt = %v, want %v", msg.CreatedAt, ts)
	}
	if msg.Origin != "" {
		t.Errorf("Origin = %q, want empty for single-node write", msg.Origin)
	}

	if _, err := s.ReadMessage(ctx, "read.one", seq+1); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("ReadMessage(missing) error = %v, want ErrMessageNotFound", err)
	}
}

func TestLatestSeq(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)
	ctx := context.Background()

	if seq, err := s.LatestSeq(ctx, "latest.unknown"); err != nil || seq != 0 {
		t.Fatalf("LatestSeq(unknown) = %d, %v; want 0, nil", seq, err)
	}

	for i := 0; i < 3; i++ {
		if _, _, err := s.WriteMessage(ctx, "latest.test", json.RawMessage(`{}`), nil); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	seq, err := s.LatestSeq(ctx, "latest.test")
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 3 {
		t.Fatalf("LatestSeq = %d, want 3", seq)
	}
}

// --- v2 第3层：驱逐 leader 锁（CL-10）---

func TestEvictionLock_Contention(t *testing.T) {
	ctx := context.Background()
	a := newTestStore(t)
	b := newTestStore(t)

	// Release is idempotent, so cleanup can release every lock ever acquired
	// and a mid-test failure never leaves the database-wide lock held.
	var acquired []*EvictionLock
	t.Cleanup(func() {
		for _, l := range acquired {
			if err := l.Release(context.Background()); err != nil {
				t.Errorf("cleanup release: %v", err)
			}
		}
	})

	lockA, err := a.TryEvictionLock(ctx)
	if err != nil {
		t.Fatalf("A TryEvictionLock: %v", err)
	}
	if lockA == nil {
		t.Fatal("A did not acquire the eviction lock")
	}
	acquired = append(acquired, lockA)

	lockB, err := b.TryEvictionLock(ctx)
	if err != nil {
		t.Fatalf("B TryEvictionLock: %v", err)
	}
	if lockB != nil {
		t.Fatal("B acquired the lock while A holds it")
	}

	if err := lockA.Release(ctx); err != nil {
		t.Fatalf("A Release: %v", err)
	}

	lockB, err = b.TryEvictionLock(ctx)
	if err != nil {
		t.Fatalf("B TryEvictionLock after release: %v", err)
	}
	if lockB == nil {
		t.Fatal("B did not acquire the lock after A released it")
	}
	acquired = append(acquired, lockB)
	if err := lockB.Release(ctx); err != nil {
		t.Fatalf("B Release: %v", err)
	}

	// Release must still work when the caller's context is already cancelled
	// (shutdown path).
	lockA, err = a.TryEvictionLock(ctx)
	if err != nil {
		t.Fatalf("A re-acquire: %v", err)
	}
	if lockA == nil {
		t.Fatal("A could not re-acquire after release")
	}
	acquired = append(acquired, lockA)
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := lockA.Release(cancelledCtx); err != nil {
		t.Fatalf("Release with cancelled context: %v", err)
	}
}

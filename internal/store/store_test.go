//go:build integration

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/config"
)

func testDSN() string {
	if dsn := os.Getenv("AETHER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://aether:aether@localhost:5433/aether_test?sslmode=disable"
}

func newTestStore(t *testing.T) *pgStore {
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
		DefaultTTL:      720 * time.Hour,
		DefaultMaxCount: 10000,
		EvictionInterval: 5 * time.Minute,
		Rules: []config.RetentionRule{
			{Pattern: "alerts.*", TTL: 24 * time.Hour, MaxCount: 5000},
			{Pattern: "shortlived", TTL: 1 * time.Hour, MaxCount: 100},
		},
	}

	st, err := New(ctx, dbCfg, retCfg)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return st.(*pgStore)
}

func truncateAll(t *testing.T, s *pgStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `TRUNCATE messages, channels RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate test tables: %v", err)
	}
}

// --- S-1: RunMigrations creates all tables and indexes ---

func TestRunMigrations_EmptyDB(t *testing.T) {
	s := newTestStore(t)

	tables := []string{"channels", "messages", "schema_migrations"}
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
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("expected versions [1 2], got %v", versions)
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

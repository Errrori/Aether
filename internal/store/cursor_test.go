//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestCursorStore_SaveLoadMonotonicAndIsolated(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)
	ctx := context.Background()

	if err := s.SaveCursors(ctx, "sub-1", map[string]int64{"a": 5, "b": 3}); err != nil {
		t.Fatalf("SaveCursors: %v", err)
	}

	got, err := s.LoadCursors(ctx, "sub-1", []string{"a", "b", "missing"})
	if err != nil {
		t.Fatalf("LoadCursors: %v", err)
	}
	if len(got) != 2 || got["a"] != 5 || got["b"] != 3 {
		t.Fatalf("LoadCursors = %v, want map[a:5 b:3]", got)
	}

	// A lower value must not move the cursor backwards.
	if err := s.SaveCursors(ctx, "sub-1", map[string]int64{"a": 2}); err != nil {
		t.Fatalf("SaveCursors lower: %v", err)
	}
	got, err = s.LoadCursors(ctx, "sub-1", []string{"a"})
	if err != nil {
		t.Fatalf("LoadCursors after lower write: %v", err)
	}
	if got["a"] != 5 {
		t.Fatalf("cursor a = %d after lower write, want 5", got["a"])
	}

	// A higher value advances it.
	if err := s.SaveCursors(ctx, "sub-1", map[string]int64{"a": 9}); err != nil {
		t.Fatalf("SaveCursors higher: %v", err)
	}
	got, _ = s.LoadCursors(ctx, "sub-1", []string{"a"})
	if got["a"] != 9 {
		t.Fatalf("cursor a = %d after higher write, want 9", got["a"])
	}

	// Cursors are isolated per subscriber.
	got, err = s.LoadCursors(ctx, "sub-2", []string{"a"})
	if err != nil {
		t.Fatalf("LoadCursors other subscriber: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("sub-2 cursors = %v, want empty", got)
	}

	// Empty inputs are no-ops.
	if err := s.SaveCursors(ctx, "sub-1", nil); err != nil {
		t.Fatalf("SaveCursors empty: %v", err)
	}
	got, err = s.LoadCursors(ctx, "sub-1", nil)
	if err != nil {
		t.Fatalf("LoadCursors empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LoadCursors empty = %v, want empty", got)
	}
}

func TestCursorStore_UpdatedAtOnlyAdvancesOnProgress(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)
	ctx := context.Background()

	if err := s.SaveCursors(ctx, "sub-1", map[string]int64{"a": 5}); err != nil {
		t.Fatalf("SaveCursors: %v", err)
	}

	var first time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT updated_at FROM subscriber_cursors WHERE subscriber_id = $1 AND channel = $2`,
		"sub-1", "a").Scan(&first); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	// A stale (lower) replay must not extend the retention TTL.
	if err := s.SaveCursors(ctx, "sub-1", map[string]int64{"a": 5}); err != nil {
		t.Fatalf("SaveCursors stale: %v", err)
	}
	var second time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT updated_at FROM subscriber_cursors WHERE subscriber_id = $1 AND channel = $2`,
		"sub-1", "a").Scan(&second); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if !second.Equal(first) {
		t.Fatalf("updated_at advanced on a stale write: %v -> %v", first, second)
	}
}

func TestCursorStore_DeleteStale(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)
	ctx := context.Background()

	if err := s.SaveCursors(ctx, "sub-1", map[string]int64{"old": 1, "fresh": 2}); err != nil {
		t.Fatalf("SaveCursors: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE subscriber_cursors SET updated_at = now() - interval '2 hours' WHERE channel = 'old'`); err != nil {
		t.Fatalf("age cursor: %v", err)
	}

	removed, err := s.DeleteStaleCursors(ctx, time.Hour)
	if err != nil {
		t.Fatalf("DeleteStaleCursors: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	got, err := s.LoadCursors(ctx, "sub-1", []string{"old", "fresh"})
	if err != nil {
		t.Fatalf("LoadCursors: %v", err)
	}
	if _, ok := got["old"]; ok {
		t.Fatal("stale cursor survived cleanup")
	}
	if got["fresh"] != 2 {
		t.Fatalf("fresh cursor = %d, want 2", got["fresh"])
	}
}

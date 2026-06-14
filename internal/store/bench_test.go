//go:build bench

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/config"
)

func benchDSN() string {
	if dsn := os.Getenv("AETHER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://aether:aether@localhost:5433/aether_test?sslmode=disable"
}

func newBenchStore(b *testing.B) *pgStore {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbCfg := &config.DatabaseConfig{
		DSN:             benchDSN(),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxIdleTime: time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
	}
	retCfg := &config.RetentionConfig{
		DefaultTTL:      720 * time.Hour,
		DefaultMaxCount: 10000,
		EvictionInterval: 5 * time.Minute,
	}

	st, err := New(ctx, dbCfg, retCfg)
	if err != nil {
		b.Fatalf("connect to test db: %v", err)
	}
	b.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		b.Fatalf("run migrations: %v", err)
	}

	return st.(*pgStore)
}

func truncateBench(b *testing.B, s *pgStore) {
	b.Helper()
	if _, err := s.pool.Exec(context.Background(), `TRUNCATE messages, channels RESTART IDENTITY CASCADE`); err != nil {
		b.Fatalf("truncate test tables: %v", err)
	}
}

func benchPayload1KB() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"data":"%s"}`, strings.Repeat("x", 1008)))
}

func BenchmarkWriteMessage(b *testing.B) {
	s := newBenchStore(b)
	truncateBench(b, s)
	payload := benchPayload1KB()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := s.WriteMessage(context.Background(), "bench.ch", payload, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteMessageParallel(b *testing.B) {
	s := newBenchStore(b)
	truncateBench(b, s)
	payload := benchPayload1KB()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, err := s.WriteMessage(context.Background(), "same.ch", payload, nil)
			if err != nil {
				b.Error(err)
			}
		}
	})
}

func BenchmarkWriteMessageMultiChannel(b *testing.B) {
	s := newBenchStore(b)
	truncateBench(b, s)
	payload := benchPayload1KB()
	var counter atomic.Int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		id := int(counter.Add(1) % 10)
		channel := fmt.Sprintf("bench.ch.%d", id)
		for pb.Next() {
			_, _, err := s.WriteMessage(context.Background(), channel, payload, nil)
			if err != nil {
				b.Error(err)
			}
		}
	})
}

func BenchmarkReadHistory(b *testing.B) {
	for _, count := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("count=%d", count), func(b *testing.B) {
			s := newBenchStore(b)
			truncateBench(b, s)
			payload := json.RawMessage(`{"x":1}`)
			for i := 0; i < count; i++ {
				_, _, err := s.WriteMessage(context.Background(), "hist.ch", payload, nil)
				if err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := s.ReadHistory(context.Background(), "hist.ch", 0, count)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkReadHistoryEmpty(b *testing.B) {
	s := newBenchStore(b)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := s.ReadHistory(context.Background(), "nonexistent", 0, 10)
		if err != nil {
			b.Fatal(err)
		}
	}
}

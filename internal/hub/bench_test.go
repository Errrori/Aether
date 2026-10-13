//go:build bench

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

const (
	benchSubs = 10
	// benchBufferSize is deliberately far larger than the production default
	// (256). At benchmark speed the publisher outruns any drain scheduling, so
	// with a small buffer the non-blocking fanout disconnects every subscriber
	// within microseconds and the benchmark then measures the disconnect path
	// instead of a real fanout. The large buffer absorbs those bursts so the
	// subscribers stay alive and every publish is actually fanned out.
	// assertAlive turns that assumption into a hard check.
	benchBufferSize = 1 << 18
	// slowBufferSize is intentionally tiny so the slow subscriber backs up and
	// triggers the backpressure branch quickly.
	slowBufferSize = 16
)

func BenchmarkPublish_NoSubscribers(b *testing.B) {
	h, _ := newTestHub(b)
	payload := json.RawMessage(`{"msg":"bench"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := h.Publish(context.Background(), "bench.ch", payload, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalFrame(b *testing.B) {
	frame := MessageFrame{
		Type:      "message",
		Channel:   "bench.ch",
		SeqID:     42,
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Payload:   json.RawMessage(`{"msg":"bench"}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := MarshalFrame(frame)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
}

// --- fanout benchmark setup ---

// fanoutSetup owns a hub plus its subscriber connections and drain goroutines.
type fanoutSetup struct {
	h     *hubImpl
	conns []*Connection
	wg    sync.WaitGroup
}

// newFanoutSetup creates nFast fast-draining subscribers on fastBuf, plus
// (when slowDelay > 0) one slow-draining subscriber on slowBuf. All join
// "bench.ch".
func newFanoutSetup(b *testing.B, nFast, fastBuf int, slowDelay time.Duration) *fanoutSetup {
	b.Helper()
	h, _ := newTestHub(b)
	s := &fanoutSetup{h: h}

	add := func(id string, buf int, delay time.Duration) {
		c := NewConnection(id, "sub-"+id, nil, buf)
		if err := h.Subscribe(c, []string{"bench.ch"}, SubscribeOptions{}); err != nil {
			b.Fatalf("Subscribe: %v", err)
		}
		// Consume the subscribed ack frame.
		select {
		case <-c.Send:
		default:
		}
		s.conns = append(s.conns, c)
		s.wg.Add(1)
		go s.drain(c, delay)
	}

	for i := 0; i < nFast; i++ {
		add(fmt.Sprintf("fast-%d", i), fastBuf, 0)
	}
	if slowDelay > 0 {
		add("slow", slowBufferSize, slowDelay)
	}
	return s
}

func (s *fanoutSetup) drain(c *Connection, delay time.Duration) {
	defer s.wg.Done()
	for {
		select {
		case _, ok := <-c.Send:
			if !ok {
				return
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		case <-c.Done():
			return
		}
	}
}

// assertAlive fails the benchmark if the fanout disconnected a subscriber,
// which would mean the run measured the disconnect path rather than a fanout.
func (s *fanoutSetup) assertAlive(b *testing.B) {
	b.Helper()
	for _, c := range s.conns {
		select {
		case <-c.Done():
			b.Errorf("subscriber %s was disconnected; buffer too small for this benchtime", c.ID)
		default:
		}
	}
}

func (s *fanoutSetup) teardown() {
	for _, c := range s.conns {
		c.Close()
	}
	s.wg.Wait()
}

// --- Naive baseline for the fanout design ---

// publishNaive reproduces the design the optimized Publish replaced: one
// global lock held across the whole fanout, with blocking sends to every
// subscriber. It exists only to give the production design a measured
// before/after; it is never used by production code.
func (h *hubImpl) publishNaive(ctx context.Context, channel string, payload json.RawMessage) error {
	if err := store.ValidateChannelName(channel); err != nil {
		return err
	}
	seqID, timestamp, err := h.store.WriteMessage(ctx, channel, payload, nil)
	if err != nil {
		return err
	}

	frame := MessageFrame{
		Type:      FrameTypeMessage,
		Channel:   channel,
		SeqID:     seqID,
		Timestamp: timestamp.Format(time.RFC3339Nano),
		Payload:   payload,
	}
	msgBytes, err := MarshalFrame(frame)
	if err != nil {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, conn := range h.channels[channel] {
		conn.Send <- msgBytes
	}
	return nil
}

// BenchmarkPublish_WithSubscribers measures the real hub fanout to benchSubs
// live subscribers (optimized design).
func BenchmarkPublish_WithSubscribers(b *testing.B) {
	s := newFanoutSetup(b, benchSubs, benchBufferSize, 0)
	payload := json.RawMessage(`{"msg":"bench"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := s.h.Publish(context.Background(), "bench.ch", payload, nil); err != nil {
			b.Error(err)
		}
	}
	b.StopTimer()
	s.assertAlive(b)
	s.teardown()
}

// BenchmarkPublish_Naive is the apples-to-apples baseline for
// BenchmarkPublish_WithSubscribers: same channel, subscriber count and drain
// behavior, but the naive global-lock + blocking-send fanout.
func BenchmarkPublish_Naive(b *testing.B) {
	s := newFanoutSetup(b, benchSubs, benchBufferSize, 0)
	payload := json.RawMessage(`{"msg":"bench"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.h.publishNaive(context.Background(), "bench.ch", payload); err != nil {
			b.Error(err)
		}
	}
	b.StopTimer()
	s.assertAlive(b)
	s.teardown()
}

// BenchmarkPublish_Parallel compares the two designs under concurrent
// publishers, where the naive global lock serializes every fanout.
func BenchmarkPublish_Parallel(b *testing.B) {
	payload := json.RawMessage(`{"msg":"bench"}`)

	b.Run("optimized", func(b *testing.B) {
		s := newFanoutSetup(b, benchSubs, benchBufferSize, 0)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, _, err := s.h.Publish(context.Background(), "bench.ch", payload, nil); err != nil {
					b.Error(err)
				}
			}
		})
		b.StopTimer()
		s.assertAlive(b)
		s.teardown()
	})

	b.Run("naive", func(b *testing.B) {
		s := newFanoutSetup(b, benchSubs, benchBufferSize, 0)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := s.h.publishNaive(context.Background(), "bench.ch", payload); err != nil {
					b.Error(err)
				}
			}
		})
		b.StopTimer()
		s.assertAlive(b)
		s.teardown()
	})
}

// BenchmarkPublish_SlowSubscriber compares the two designs when one of the
// subscribers drains slowly. The naive blocking send couples Publish latency
// to the slowest consumer (head-of-line blocking); the optimized non-blocking
// send bounds it by disconnecting the backed-up connection instead.
func BenchmarkPublish_SlowSubscriber(b *testing.B) {
	const slowDelay = time.Millisecond
	payload := json.RawMessage(`{"msg":"bench"}`)

	b.Run("optimized", func(b *testing.B) {
		s := newFanoutSetup(b, benchSubs, benchBufferSize, slowDelay)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, _, err := s.h.Publish(context.Background(), "bench.ch", payload, nil); err != nil {
				b.Error(err)
			}
		}
		b.StopTimer()
		s.teardown()
	})

	b.Run("naive", func(b *testing.B) {
		s := newFanoutSetup(b, benchSubs, benchBufferSize, slowDelay)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := s.h.publishNaive(context.Background(), "bench.ch", payload); err != nil {
				b.Error(err)
			}
		}
		b.StopTimer()
		s.teardown()
	})
}

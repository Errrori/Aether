//go:build bench

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
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

func BenchmarkPublish_WithSubscribers(b *testing.B) {
	h, _ := newTestHub(b)
	payload := json.RawMessage(`{"msg":"bench"}`)

	var conns []*Connection
	var drainWg sync.WaitGroup

	for i := 0; i < 10; i++ {
		conn := newTestConnection(b, fmt.Sprintf("bench-conn-%d", i))
		h.Subscribe(conn, []string{"bench.ch"}, nil)
		// Consume subscribed ack frame.
		select {
		case <-conn.Send:
		default:
		}
		conns = append(conns, conn)

		drainWg.Add(1)
		go func(c *Connection) {
			defer drainWg.Done()
			for {
				select {
				case _, ok := <-c.Send:
					if !ok {
						return
					}
				case <-c.Done():
					return
				}
			}
		}(conn)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := h.Publish(context.Background(), "bench.ch", payload, nil)
		if err != nil {
			b.Error(err)
		}
	}
	b.StopTimer()

	for _, c := range conns {
		c.Close()
	}
	drainWg.Wait()
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

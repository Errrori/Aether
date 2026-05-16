package hub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

func (h *hubImpl) Publish(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (int64, time.Time, error) {
	start := time.Now()
	if err := store.ValidateChannelName(channel); err != nil {
		return 0, time.Time{}, err
	}

	writeStart := time.Now()
	seqID, timestamp, err := h.store.WriteMessage(ctx, channel, payload, idempotencyKey)
	if err != nil {
		return 0, time.Time{}, err
	}
	if h.metrics.ObserveStorageWrite != nil {
		h.metrics.ObserveStorageWrite(channel, time.Since(writeStart).Seconds())
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
		// Persisted successfully but couldn't marshal — don't fail the publish.
		if h.metrics.IncMessagesPublished != nil {
			h.metrics.IncMessagesPublished(channel)
		}
		if h.metrics.ObservePublish != nil {
			h.metrics.ObservePublish(channel, time.Since(start).Seconds())
		}
		return seqID, timestamp, nil
	}

	h.mu.RLock()
	subs := h.channels[channel]
	conns := make([]*Connection, 0, len(subs))
	for _, conn := range subs {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()

	pushed := 0
	for _, conn := range conns {
		select {
		case conn.Send <- msgBytes:
			pushed++
		default:
			conn.Close()
		}
	}

	if h.metrics.IncMessagesPublished != nil {
		h.metrics.IncMessagesPublished(channel)
	}
	if h.metrics.AddMessagesPushed != nil && pushed > 0 {
		h.metrics.AddMessagesPushed(channel, pushed)
	}
	if h.metrics.ObservePublish != nil {
		h.metrics.ObservePublish(channel, time.Since(start).Seconds())
	}

	return seqID, timestamp, nil
}

package hub

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	// ackFlushTimeout bounds a single flusher write attempt. It intentionally
	// does not inherit the flusher's canceled context, so the final flush on
	// shutdown still has a chance to complete.
	ackFlushTimeout = 5 * time.Second
)

// pendingCursorKey identifies an unflushed acknowledgment cursor.
type pendingCursorKey struct {
	subscriberID string
	channel      string
}

// Ack records subscriber acknowledgments for channels the connection is
// currently subscribed to. Values are merged into the in-memory pending set
// (max per subscriber/channel) and persisted by the flusher goroutine.
// Success is silent; invalid entries produce error frames.
func (h *hubImpl) Ack(conn *Connection, acks map[string]int64) {
	if conn == nil {
		return
	}

	for channel, seq := range acks {
		if seq < 0 {
			conn.SendError(ErrCodeInvalidAck,
				fmt.Sprintf("invalid ack seq %d for channel %s", seq, channel))
			continue
		}
		if !conn.HasChannel(channel) {
			conn.SendError(ErrCodeInvalidAck,
				fmt.Sprintf("not subscribed to channel: %s", channel))
			continue
		}
		if !h.auth.IsChannelAuthorized(conn.Claims, channel) {
			conn.SendError(ErrCodeUnauthorized,
				fmt.Sprintf("unauthorized channel: %s", channel))
			continue
		}
		h.mergePending(conn.SubscriberID, channel, seq)
		if h.metrics.IncAcks != nil {
			h.metrics.IncAcks()
		}
	}
}

// mergePending raises the pending cursor for (subscriberID, channel).
// All writers must go through max so concurrent acks cannot move it back.
func (h *hubImpl) mergePending(subscriberID, channel string, seq int64) {
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	key := pendingCursorKey{subscriberID: subscriberID, channel: channel}
	if seq > h.pending[key] {
		h.pending[key] = seq
	}
}

// pendingCursor returns the unflushed cursor for (subscriberID, channel),
// or 0 when there is none.
func (h *hubImpl) pendingCursor(subscriberID, channel string) int64 {
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	return h.pending[pendingCursorKey{subscriberID: subscriberID, channel: channel}]
}

// signalFlush wakes the flusher without blocking the caller. Used on
// connection teardown so a subscriber's final acks are persisted promptly.
func (h *hubImpl) signalFlush() {
	select {
	case h.flushSignal <- struct{}{}:
	default:
	}
}

// Start launches the cursor flusher goroutine. It is safe to call once per
// hub; Wait must be called to join it before the store is closed.
func (h *hubImpl) Start(ctx context.Context) {
	h.flushWG.Add(1)
	go func() {
		defer h.flushWG.Done()
		h.runFlusher(ctx)
	}()
}

// Wait blocks until the flusher goroutine has exited.
func (h *hubImpl) Wait() {
	h.flushWG.Wait()
}

func (h *hubImpl) runFlusher(ctx context.Context) {
	ticker := time.NewTicker(h.config.AckFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			h.flushPending()
			return
		case <-ticker.C:
			h.flushPending()
		case <-h.flushSignal:
			h.flushPending()
		}
	}
}

// flushPending writes the current pending cursors to the store. On success
// the written entries are removed with a compare-and-delete so a value that
// advanced during the write stays pending; on failure the entries are kept
// for the next round.
func (h *hubImpl) flushPending() {
	if h.cursorStore == nil {
		return
	}

	h.pendingMu.Lock()
	if len(h.pending) == 0 {
		h.pendingMu.Unlock()
		return
	}
	snapshot := make(map[pendingCursorKey]int64, len(h.pending))
	for key, seq := range h.pending {
		snapshot[key] = seq
	}
	h.pendingMu.Unlock()

	bySubscriber := make(map[string]map[string]int64)
	for key, seq := range snapshot {
		cursors := bySubscriber[key.subscriberID]
		if cursors == nil {
			cursors = make(map[string]int64)
			bySubscriber[key.subscriberID] = cursors
		}
		cursors[key.channel] = seq
	}

	ctx, cancel := context.WithTimeout(context.Background(), ackFlushTimeout)
	defer cancel()

	for subscriberID, cursors := range bySubscriber {
		if err := h.cursorStore.SaveCursors(ctx, subscriberID, cursors); err != nil {
			slog.Warn("cursor flush failed",
				"subscriber_id", subscriberID, "channels", len(cursors), "err", err)
			continue
		}
		h.pendingMu.Lock()
		for channel, written := range cursors {
			key := pendingCursorKey{subscriberID: subscriberID, channel: channel}
			if current, ok := h.pending[key]; ok && current <= written {
				delete(h.pending, key)
			}
		}
		h.pendingMu.Unlock()
	}
}

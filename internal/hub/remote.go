package hub

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

// HasSubscribers reports whether any local connection is subscribed to the
// channel. The cluster listener uses it to skip read-backs for channels this
// node does not serve.
func (h *hubImpl) HasSubscribers(channel string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.channels[channel]) > 0
}

// DeliverRemote delivers a message published on another node. Seqs at or below
// the node cursor are already accounted for (delivered, or skipped because
// they were evicted), so duplicate notifications and catch-up overlap are
// no-ops.
func (h *hubImpl) DeliverRemote(ctx context.Context, channel string, seqID int64) error {
	if !h.HasSubscribers(channel) {
		return nil
	}
	if cur, ok := h.nodeCursor(channel); ok && seqID <= cur {
		return nil
	}

	msg, err := h.store.ReadMessage(ctx, channel, seqID)
	if err != nil {
		if errors.Is(err, store.ErrMessageNotFound) {
			// Evicted before it could be delivered; advancing the cursor keeps
			// catch-up from retrying a message that can never be delivered.
			slog.Warn("remote message already evicted", "channel", channel, "seq", seqID)
			h.advanceNodeCursor(channel, seqID)
			return nil
		}
		return err
	}

	frame := MessageFrame{
		Type:      FrameTypeMessage,
		Channel:   channel,
		SeqID:     msg.SeqID,
		Timestamp: msg.CreatedAt.Format(time.RFC3339Nano),
		Payload:   msg.Payload,
	}
	data, err := MarshalFrame(frame)
	if err != nil {
		// Persisted but unmarshalable — same policy as Publish: log loudly and
		// advance past it so it is not retried forever.
		slog.Error("marshal remote frame failed", "channel", channel, "seq", seqID, "err", err)
		h.advanceNodeCursor(channel, seqID)
		return nil
	}

	if pushed := h.fanout(channel, data); pushed > 0 {
		if h.metrics.AddMessagesPushed != nil {
			h.metrics.AddMessagesPushed(channel, pushed)
		}
	}
	h.advanceNodeCursor(channel, seqID)
	return nil
}

// CatchUp re-delivers messages that were published while this node's LISTEN
// connection was down. The cluster listener calls it after (re)connecting.
// Messages originated by this node are skipped: they were delivered inline at
// publish time, and origin_node makes that distinction exact.
func (h *hubImpl) CatchUp(ctx context.Context) error {
	for _, channel := range h.channelsWithSubscribers() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.catchUpChannel(ctx, channel); err != nil {
			// One channel failing must not abort the catch-up of the others.
			slog.Warn("catch-up failed for channel", "channel", channel, "err", err)
		}
	}
	return nil
}

func (h *hubImpl) catchUpChannel(ctx context.Context, channel string) error {
	anchor, ok := h.nodeCursor(channel)
	if !ok {
		// Cursors are initialised when the first subscriber registers; a
		// missing cursor means we cannot know what was already delivered, and
		// replaying from zero could flood the subscribers.
		slog.Warn("catch-up skipped: channel has no node cursor", "channel", channel)
		return nil
	}

	// Local moving anchor: DeliverRemote may advance the shared cursor ahead
	// concurrently, but this loop must not skip messages it has not read yet.
	cursor := anchor

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		result, err := h.store.ReadHistory(ctx, channel, cursor, h.config.HistoryLimit)
		if err != nil {
			return err
		}

		if result.MinSeq > cursor+1 {
			h.sendGapToChannel(channel, cursor, result.MinSeq)
		}

		if len(result.Messages) == 0 {
			return nil
		}

		delivered := 0
		for _, msg := range result.Messages {
			if msg.SeqID > cursor {
				cursor = msg.SeqID
			}
			if msg.Origin == h.config.NodeID {
				continue // delivered inline by this node at publish time
			}
			if h.deliverCatchUpMessage(channel, msg) {
				delivered++
			}
		}
		if delivered > 0 && h.metrics.AddMessagesPushed != nil {
			h.metrics.AddMessagesPushed(channel, delivered)
		}

		h.advanceNodeCursor(channel, cursor)

		if len(result.Messages) < h.config.HistoryLimit {
			return nil
		}
	}
}

// deliverCatchUpMessage pushes one catch-up message to every local subscriber
// that has not accounted for its seq yet (connection cursors are set on
// subscribe, so clients that joined later never receive pre-subscribe
// history). It reports whether any frame was queued.
func (h *hubImpl) deliverCatchUpMessage(channel string, msg store.Message) bool {
	frame := MessageFrame{
		Type:      FrameTypeMessage,
		Channel:   channel,
		SeqID:     msg.SeqID,
		Timestamp: msg.CreatedAt.Format(time.RFC3339Nano),
		Payload:   msg.Payload,
	}
	data, err := MarshalFrame(frame)
	if err != nil {
		slog.Error("marshal catch-up frame failed", "channel", channel, "seq", msg.SeqID, "err", err)
		return false
	}

	h.mu.RLock()
	subs := h.channels[channel]
	conns := make([]*Connection, 0, len(subs))
	for _, conn := range subs {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()

	sent := false
	for _, conn := range conns {
		if cur, ok := conn.GetCursor(channel); ok && msg.SeqID <= cur {
			continue
		}
		select {
		case conn.Send <- data:
			conn.SetCursor(channel, msg.SeqID)
			sent = true
		default:
			conn.Close()
		}
	}
	return sent
}

// sendGapToChannel mirrors subscribe-time gap semantics for every local
// subscriber of the channel.
func (h *hubImpl) sendGapToChannel(channel string, anchor, availableFrom int64) {
	h.mu.RLock()
	subs := h.channels[channel]
	conns := make([]*Connection, 0, len(subs))
	for _, conn := range subs {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()

	for _, conn := range conns {
		requested := anchor
		if cur, ok := conn.GetCursor(channel); ok {
			requested = cur
		}
		h.sendGap(conn, channel, requested, availableFrom)
	}
}

// channelsWithSubscribers snapshots the channels that have at least one local
// subscriber.
func (h *hubImpl) channelsWithSubscribers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]string, 0, len(h.channels))
	for ch, subs := range h.channels {
		if len(subs) > 0 {
			result = append(result, ch)
		}
	}
	return result
}

// --- node cursor helpers ---

func (h *hubImpl) nodeCursor(channel string) (int64, bool) {
	h.cursorMu.Lock()
	defer h.cursorMu.Unlock()
	seq, ok := h.nodeCursors[channel]
	return seq, ok
}

// advanceNodeCursor moves the cursor forward; it never moves backwards.
func (h *hubImpl) advanceNodeCursor(channel string, seq int64) {
	h.cursorMu.Lock()
	defer h.cursorMu.Unlock()
	if seq > h.nodeCursors[channel] {
		h.nodeCursors[channel] = seq
	}
}

// setNodeCursorIfAbsent initialises the cursor without clobbering a value that
// deliveries may already have advanced.
func (h *hubImpl) setNodeCursorIfAbsent(channel string, seq int64) {
	h.cursorMu.Lock()
	defer h.cursorMu.Unlock()
	if _, ok := h.nodeCursors[channel]; !ok {
		h.nodeCursors[channel] = seq
	}
}

func (h *hubImpl) dropNodeCursor(channel string) {
	h.cursorMu.Lock()
	defer h.cursorMu.Unlock()
	delete(h.nodeCursors, channel)
}

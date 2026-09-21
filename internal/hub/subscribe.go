package hub

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

func (h *hubImpl) Subscribe(conn *Connection, channels []string, opts SubscribeOptions) error {
	if conn == nil {
		return ErrNilConnection
	}

	// H-10: max channels per subscribe request.
	if len(channels) > h.config.MaxChannelsPerSubscribe {
		conn.SendError(ErrCodeTooManyPerReq,
			fmt.Sprintf("too many channels per request: %d (max %d)", len(channels), h.config.MaxChannelsPerSubscribe))
		return nil
	}

	// Pre-filter: validate, check auth, skip duplicates.
	type pendingSub struct {
		channel  string
		afterSeq int64
		hasAfter bool
		resume   bool
	}
	var pending []pendingSub

	for _, ch := range channels {
		if err := store.ValidateChannelName(ch); err != nil {
			conn.SendError(ErrCodeInvalidChannel, err.Error())
			continue
		}
		if !h.auth.IsChannelAuthorized(conn.Claims, ch) {
			conn.SendError(ErrCodeUnauthorized, fmt.Sprintf("unauthorized channel: %s", ch))
			continue
		}
		if conn.HasChannel(ch) {
			continue // H-6: duplicate subscribe is silently ignored.
		}
		ps := pendingSub{channel: ch}
		if seq, ok := opts.AfterSeq[ch]; ok {
			ps.hasAfter = true
			ps.afterSeq = seq
		} else if opts.Resume {
			ps.resume = true
		}
		pending = append(pending, ps)
	}

	if len(pending) == 0 {
		h.sendSubscribed(conn, nil)
		return nil
	}

	// H-10: total channels per connection limit.
	if conn.ChannelCount()+len(pending) > h.config.MaxChannelsPerConn {
		conn.SendError(ErrCodeTooManyPerConn,
			fmt.Sprintf("total channels would exceed %d", h.config.MaxChannelsPerConn))
		return nil
	}

	// Load persisted acknowledgment cursors for all resume channels in one
	// query. A load failure fails closed below: silently falling back to
	// live-only would make the client believe it missed nothing.
	var resumeCursors map[string]int64
	var resumeErr error
	if opts.Resume && h.cursorStore != nil {
		var resumeChans []string
		for _, ps := range pending {
			if ps.resume {
				resumeChans = append(resumeChans, ps.channel)
			}
		}
		if len(resumeChans) > 0 {
			resumeCursors, resumeErr = h.cursorStore.LoadCursors(
				context.Background(), conn.SubscriberID, resumeChans)
			if resumeErr != nil {
				slog.Warn("cursor load failed",
					"subscriber_id", conn.SubscriberID, "channels", len(resumeChans), "err", resumeErr)
			}
		}
	}

	// H-7: replay history for channels with an anchor BEFORE registering for
	// real-time. Explicit after_seq wins over resume per channel.
	var registered []string
	for _, ps := range pending {
		anchor := ps.afterSeq
		shouldReplay := ps.hasAfter
		if ps.resume {
			if resumeErr != nil {
				conn.SendError(ErrCodeHistoryFailed,
					fmt.Sprintf("cursor load failed for %s: %v", ps.channel, resumeErr))
				continue
			}
			anchor = resumeCursors[ps.channel]
			if pendingSeq := h.pendingCursor(conn.SubscriberID, ps.channel); pendingSeq > anchor {
				anchor = pendingSeq
			}
			shouldReplay = anchor > 0
		}

		if shouldReplay {
			if err := h.replayHistory(conn, ps.channel, anchor); err != nil {
				conn.SendError(ErrCodeHistoryFailed,
					fmt.Sprintf("history read failed for %s: %v", ps.channel, err))
				continue
			}
		} else if h.config.NodeID != "" {
			h.initClusterCursor(conn, ps.channel)
		}
		registered = append(registered, ps.channel)
	}

	if len(registered) == 0 {
		h.sendSubscribed(conn, nil)
		return nil
	}

	// Register for real-time delivery.
	h.mu.Lock()

	// Re-check: connection may have been removed while replaying history.
	select {
	case <-conn.Done():
		h.mu.Unlock()
		return nil
	default:
	}

	for _, ch := range registered {
		if h.channels[ch] == nil {
			h.channels[ch] = make(map[string]*Connection)
		}
		if len(h.channels[ch]) == 0 {
			h.activeChans.Add(1)
			if h.metrics.IncChannels != nil {
				h.metrics.IncChannels()
			}
		}
		h.channels[ch][conn.ID] = conn
		conn.AddChannel(ch)
		// Cursor is set by replayHistory for channels with afterSeq;
		// for channels without afterSeq it remains at the zero value.
	}
	h.mu.Unlock()

	// Track connection globally. Only bump the metric on first registration.
	h.connsMu.Lock()
	_, exists := h.conns[conn.ID]
	if !exists {
		h.conns[conn.ID] = conn
		if h.metrics.IncConnections != nil {
			h.metrics.IncConnections()
		}
	}
	h.connsMu.Unlock()

	h.sendSubscribed(conn, registered)
	return nil
}

// replayHistory delivers messages after afterSeq in batches until the history
// is exhausted (or the connection closes). Batching removes the silent
// truncation of a single capped ReadHistory call: a client resuming far
// behind receives everything still retained, preceded by a gap frame when
// the retention window has already passed its anchor.
func (h *hubImpl) replayHistory(conn *Connection, channel string, afterSeq int64) error {
	limit := h.config.HistoryLimit
	if limit > store.MaxHistoryLimit {
		limit = store.MaxHistoryLimit
	}

	anchor := afterSeq
	for {
		select {
		case <-conn.Done():
			return fmt.Errorf("connection closed during history replay")
		default:
		}

		result, err := h.store.ReadHistory(context.Background(), channel, anchor, limit)
		if err != nil {
			return err
		}

		// H-8, H-9: Gap detection — if the anchor is before the earliest
		// available message, the client can never receive those seqs.
		if result.MinSeq > anchor+1 {
			h.sendGap(conn, channel, anchor, result.MinSeq)
			anchor = result.MinSeq - 1
		}

		for _, msg := range result.Messages {
			frame := MessageFrame{
				Type:      FrameTypeMessage,
				Channel:   channel,
				SeqID:     msg.SeqID,
				Timestamp: msg.CreatedAt.Format(time.RFC3339Nano),
				Payload:   msg.Payload,
			}
			data, err := MarshalFrame(frame)
			if err != nil {
				// MarshalFrame failure for a message is non-recoverable:
				// update the anchor anyway so the cursor does not lie about delivery.
				if msg.SeqID > anchor {
					anchor = msg.SeqID
				}
				continue
			}
			select {
			case conn.Send <- data:
			default:
				conn.Close()
				return fmt.Errorf("connection closed during history replay")
			}
			if msg.SeqID > anchor {
				anchor = msg.SeqID
			}
		}

		if len(result.Messages) < limit {
			break
		}
	}

	conn.SetCursor(channel, anchor)
	if h.config.NodeID != "" {
		h.advanceNodeCursor(channel, anchor)
	}
	return nil
}

// initClusterCursor initialises the node and connection cursors for a
// cluster-mode subscription without after_seq: the client asked for live
// messages only, so a later reconnect catch-up must not replay older history.
func (h *hubImpl) initClusterCursor(conn *Connection, channel string) {
	latest, err := h.store.LatestSeq(context.Background(), channel)
	if err != nil {
		// Leave the cursors unset: catch-up skips channels without a node
		// cursor rather than risking a history flood.
		slog.Warn("cluster cursor init failed", "channel", channel, "err", err)
		return
	}
	h.setNodeCursorIfAbsent(channel, latest)
	conn.SetCursor(channel, latest)
}

func (h *hubImpl) Unsubscribe(conn *Connection, channels []string) {
	var unsubscribed []string

	h.mu.Lock()
	for _, ch := range channels {
		if !conn.HasChannel(ch) {
			continue
		}
		if subs, ok := h.channels[ch]; ok {
			delete(subs, conn.ID)
			if len(subs) == 0 {
				delete(h.channels, ch)
				h.activeChans.Add(-1)
				h.dropNodeCursor(ch)
				if h.metrics.DecChannels != nil {
					h.metrics.DecChannels()
				}
			}
		}
		conn.RemoveChannel(ch)
		unsubscribed = append(unsubscribed, ch)
	}
	h.mu.Unlock()

	if len(unsubscribed) > 0 {
		h.sendUnsubscribed(conn, unsubscribed)
	}
}

func (h *hubImpl) RemoveConnection(conn *Connection) {
	// Close the connection first so that Done() signals before cleanup.
	// This prevents a concurrent Subscribe from registering a connection
	// that is being torn down (TOCTOU between h.mu.Unlock and conn.Close).
	conn.Close()

	h.mu.Lock()
	// Sweep all channels for this connection to avoid TOCTOU races with Subscribe.
	for ch, subs := range h.channels {
		if _, ok := subs[conn.ID]; ok {
			delete(subs, conn.ID)
			if len(subs) == 0 {
				delete(h.channels, ch)
				h.activeChans.Add(-1)
				h.dropNodeCursor(ch)
				if h.metrics.DecChannels != nil {
					h.metrics.DecChannels()
				}
			}
		}
	}
	h.connsMu.Lock()
	delete(h.conns, conn.ID)
	h.connsMu.Unlock()
	h.mu.Unlock()

	if h.metrics.DecConnections != nil {
		h.metrics.DecConnections()
	}

	// Persist any acks this connection made before it dropped; the signal is
	// non-blocking so connection teardown never waits on the flusher.
	h.signalFlush()
}

// --- helpers for sending frames to a connection ---

func (h *hubImpl) sendSubscribed(conn *Connection, channels []string) {
	data, err := MarshalFrame(SubscribedFrame{
		Type:     FrameTypeSubscribed,
		Channels: channels,
	})
	if err != nil {
		return
	}
	select {
	case conn.Send <- data:
	default:
		conn.Close()
	}
}

func (h *hubImpl) sendUnsubscribed(conn *Connection, channels []string) {
	data, err := MarshalFrame(UnsubscribedFrame{
		Type:     FrameTypeUnsubscribed,
		Channels: channels,
	})
	if err != nil {
		return
	}
	select {
	case conn.Send <- data:
	default:
		conn.Close()
	}
}

func (h *hubImpl) sendGap(conn *Connection, channel string, requestedSeq, availableFrom int64) {
	data, err := MarshalFrame(GapFrame{
		Type:             FrameTypeGap,
		Channel:          channel,
		AvailableFromSeq: availableFrom,
		RequestedFromSeq: requestedSeq,
		Message: fmt.Sprintf("gap: requested seq %d but earliest available is %d",
			requestedSeq, availableFrom),
	})
	if err != nil {
		return
	}
	select {
	case conn.Send <- data:
	default:
		conn.Close()
	}
}

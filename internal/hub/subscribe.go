package hub

import (
	"context"
	"fmt"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

func (h *hubImpl) Subscribe(conn *Connection, channels []string, afterSeq map[string]int64) error {
	if conn == nil {
		return ErrNilConnection
	}

	// H-10: max channels per subscribe request.
	if len(channels) > h.config.MaxChannelsPerSubscribe {
		h.sendError(conn, ErrCodeTooManyPerReq,
			fmt.Sprintf("too many channels per request: %d (max %d)", len(channels), h.config.MaxChannelsPerSubscribe))
		return nil
	}

	// Pre-filter: validate, check auth, skip duplicates.
	type pendingSub struct {
		channel  string
		afterSeq int64
		hasAfter bool
	}
	var pending []pendingSub

	for _, ch := range channels {
		if err := store.ValidateChannelName(ch); err != nil {
			h.sendError(conn, ErrCodeInvalidChannel, err.Error())
			continue
		}
		if !h.auth.IsChannelAuthorized(conn.Claims, ch) {
			h.sendError(conn, ErrCodeUnauthorized, fmt.Sprintf("unauthorized channel: %s", ch))
			continue
		}
		if conn.HasChannel(ch) {
			continue // H-6: duplicate subscribe is silently ignored.
		}
		ps := pendingSub{channel: ch}
		if seq, ok := afterSeq[ch]; ok {
			ps.hasAfter = true
			ps.afterSeq = seq
		}
		pending = append(pending, ps)
	}

	if len(pending) == 0 {
		h.sendSubscribed(conn, nil)
		return nil
	}

	// H-10: total channels per connection limit.
	if conn.ChannelCount()+len(pending) > h.config.MaxChannelsPerConn {
		h.sendError(conn, ErrCodeTooManyPerConn,
			fmt.Sprintf("total channels would exceed %d", h.config.MaxChannelsPerConn))
		return nil
	}

	// H-7: replay history for channels with after_seq BEFORE registering for real-time.
	var registered []string
	for _, ps := range pending {
		if ps.hasAfter {
			if err := h.replayHistory(conn, ps.channel, ps.afterSeq); err != nil {
				h.sendError(conn, ErrCodeHistoryFailed,
					fmt.Sprintf("history read failed for %s: %v", ps.channel, err))
				continue
			}
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

		if seq, ok := afterSeq[ch]; ok && seq >= 0 {
			conn.SetCursor(ch, seq)
		}
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

func (h *hubImpl) replayHistory(conn *Connection, channel string, afterSeq int64) error {
	result, err := h.store.ReadHistory(context.Background(), channel, afterSeq, h.config.HistoryLimit)
	if err != nil {
		return err
	}

	// H-8, H-9: Gap detection — if after_seq is before the earliest available message,
	// there are messages the client can never receive.
	if afterSeq < result.MinSeq-1 {
		h.sendGap(conn, channel, afterSeq, result.MinSeq)
	}

	maxSeq := afterSeq
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
			continue
		}
		select {
		case conn.Send <- data:
		default:
			conn.Close()
			return fmt.Errorf("connection closed during history replay")
		}
		if msg.SeqID > maxSeq {
			maxSeq = msg.SeqID
		}
	}

	conn.SetCursor(channel, maxSeq)
	return nil
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
	h.mu.Lock()
	// Sweep all channels for this connection to avoid TOCTOU races with Subscribe.
	for ch, subs := range h.channels {
		if _, ok := subs[conn.ID]; ok {
			delete(subs, conn.ID)
			if len(subs) == 0 {
				delete(h.channels, ch)
				h.activeChans.Add(-1)
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

	conn.Close()
}

// --- helpers for sending frames to a connection ---

func (h *hubImpl) sendError(conn *Connection, code int, message string) {
	data, _ := MarshalFrame(ErrorFrame{
		Type:    FrameTypeError,
		Code:    code,
		Message: message,
	})
	select {
	case conn.Send <- data:
	default:
		conn.Close()
	}
}

func (h *hubImpl) sendSubscribed(conn *Connection, channels []string) {
	data, _ := MarshalFrame(SubscribedFrame{
		Type:     FrameTypeSubscribed,
		Channels: channels,
	})
	select {
	case conn.Send <- data:
	default:
		conn.Close()
	}
}

func (h *hubImpl) sendUnsubscribed(conn *Connection, channels []string) {
	data, _ := MarshalFrame(UnsubscribedFrame{
		Type:     FrameTypeUnsubscribed,
		Channels: channels,
	})
	select {
	case conn.Send <- data:
	default:
		conn.Close()
	}
}

func (h *hubImpl) sendGap(conn *Connection, channel string, requestedSeq, availableFrom int64) {
	data, _ := MarshalFrame(GapFrame{
		Type:             FrameTypeGap,
		Channel:          channel,
		AvailableFromSeq: availableFrom,
		RequestedFromSeq: requestedSeq,
		Message: fmt.Sprintf("gap: requested seq %d but earliest available is %d",
			requestedSeq, availableFrom),
	})
	select {
	case conn.Send <- data:
	default:
		conn.Close()
	}
}

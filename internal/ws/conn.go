package ws

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/aether-mq/aether/internal/hub"
	"github.com/coder/websocket"
)

// Client frame types. Type constants are local to ws; the server→client
// frame type strings live in hub.
const (
	clientTypeSubscribe   = "subscribe"
	clientTypeUnsubscribe = "unsubscribe"
)

type subscribeRequest struct {
	Type     string           `json:"type"`
	Channels []string         `json:"channels"`
	AfterSeq map[string]int64 `json:"after_seq"`
}

type unsubscribeRequest struct {
	Type     string           `json:"type"`
	Channels []string         `json:"channels"`
}

func readLoop(ac *activeConn, h hub.Hub, maxMessageSize int, wg *sync.WaitGroup) {
	defer wg.Done()
	// readLoop is the only goroutine that detects client-side disconnects.
	// When Read returns an error, the other goroutines may still be blocked on
	// conn.Send or Done. Closing hubCnn cancels its context, which fires Done
	// and unblocks writeLoop and heartbeatLoop, preventing a ServeHTTP deadlock.
	defer ac.hubCnn.Close()
	defer ac.wsConn.CloseNow()

	ac.wsConn.SetReadLimit(int64(maxMessageSize))

	for {
		msgType, data, err := ac.wsConn.Read(context.Background())
		if err != nil {
			return
		}
		if msgType != websocket.MessageText {
			continue
		}

		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &peek); err != nil {
			sendErrorFrame(ac.hubCnn, hub.ErrCodeInvalidJSON, "invalid json")
			continue
		}

		switch peek.Type {
		case clientTypeSubscribe:
			var req subscribeRequest
			if err := json.Unmarshal(data, &req); err != nil {
				sendErrorFrame(ac.hubCnn, hub.ErrCodeInvalidJSON, "invalid subscribe frame")
				continue
			}
			_ = h.Subscribe(ac.hubCnn, req.Channels, req.AfterSeq)
		case clientTypeUnsubscribe:
			var req unsubscribeRequest
			if err := json.Unmarshal(data, &req); err != nil {
				sendErrorFrame(ac.hubCnn, hub.ErrCodeInvalidJSON, "invalid unsubscribe frame")
				continue
			}
			h.Unsubscribe(ac.hubCnn, req.Channels)
		default:
			sendErrorFrame(ac.hubCnn, hub.ErrCodeUnknownFrame, "unknown frame type")
		}
	}
}

func writeLoop(ac *activeConn, wg *sync.WaitGroup) {
	defer wg.Done()
	defer ac.wsConn.CloseNow()

	for {
		select {
		case data, ok := <-ac.hubCnn.Send:
			if !ok {
				return
			}
			if err := ac.wsConn.Write(context.Background(), websocket.MessageText, data); err != nil {
				return
			}
		case <-ac.hubCnn.Done():
			return
		}
	}
}

func heartbeatLoop(ac *activeConn, pingInterval, pongTimeout time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()
	defer ac.wsConn.CloseNow()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), pongTimeout)
			err := ac.wsConn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		case <-ac.hubCnn.Done():
			return
		}
	}
}

func sendErrorFrame(conn *hub.Connection, code int, message string) {
	frame, err := hub.MarshalFrame(hub.ErrorFrame{
		Type:    hub.FrameTypeError,
		Code:    code,
		Message: message,
	})
	if err != nil {
		return
	}
	select {
	case conn.Send <- frame:
	default:
	}
}

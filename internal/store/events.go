package store

import (
	"encoding/json"
	"fmt"
)

// NotifyChannel is the PostgreSQL notification channel used for cross-node
// message fan-out (a PG concept, unrelated to Aether channels).
const NotifyChannel = "aether_messages"

// MessageEvent is the payload carried by cross-node notifications. It contains
// only locating information: receivers read the message body back by seq_id,
// because pg_notify payloads are capped at 8000 bytes while Aether message
// payloads may be up to 64KB.
type MessageEvent struct {
	NodeID  string `json:"node_id"`
	Channel string `json:"channel"`
	SeqID   int64  `json:"seq_id"`
}

// EncodeMessageEvent serialises an event for pg_notify.
func EncodeMessageEvent(e MessageEvent) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("encode message event: %w", err)
	}
	return string(b), nil
}

// DecodeMessageEvent parses a pg_notify payload. Events missing required
// fields are rejected so a malformed notification cannot trigger a read of an
// unintended channel or seq.
func DecodeMessageEvent(payload string) (MessageEvent, error) {
	var e MessageEvent
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return MessageEvent{}, fmt.Errorf("decode message event: %w", err)
	}
	if e.NodeID == "" || e.Channel == "" || e.SeqID <= 0 {
		return MessageEvent{}, fmt.Errorf("decode message event: missing required fields")
	}
	return e, nil
}

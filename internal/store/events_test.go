package store

import (
	"strings"
	"testing"
)

func TestMessageEventRoundTrip(t *testing.T) {
	in := MessageEvent{NodeID: "node-a", Channel: "orders.created", SeqID: 42}

	encoded, err := EncodeMessageEvent(in)
	if err != nil {
		t.Fatalf("EncodeMessageEvent: %v", err)
	}

	out, err := DecodeMessageEvent(encoded)
	if err != nil {
		t.Fatalf("DecodeMessageEvent: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestDecodeMessageEventErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{"invalid json", `{"node_id":`, "decode message event"},
		{"missing node_id", `{"channel":"c","seq_id":1}`, "missing required fields"},
		{"missing channel", `{"node_id":"n","seq_id":1}`, "missing required fields"},
		{"non-positive seq_id", `{"node_id":"n","channel":"c","seq_id":0}`, "missing required fields"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeMessageEvent(tt.payload)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

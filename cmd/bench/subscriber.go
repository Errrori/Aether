package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
)

// Subscriber connects to Aether via WebSocket and receives messages.
type Subscriber struct {
	serverURL string
	jwtSecret string

	mu              sync.Mutex
	conn            *websocket.Conn
	messagesReceived int64
	e2eLatencies    []time.Duration
}

// NewSubscriber creates a subscriber for the given server and JWT secret.
func NewSubscriber(serverURL, jwtSecret string) *Subscriber {
	return &Subscriber{
		serverURL: strings.TrimRight(serverURL, "/"),
		jwtSecret: jwtSecret,
	}
}

// wsURL returns the WebSocket URL for the given server.
func (s *Subscriber) wsURL(token string) string {
	u := s.serverURL + "/ws"
	if strings.HasPrefix(u, "http://") {
		u = "ws://" + strings.TrimPrefix(u, "http://")
	} else if strings.HasPrefix(u, "https://") {
		u = "wss://" + strings.TrimPrefix(u, "https://")
	}
	return u + "?token=" + token
}

// generateJWT creates a JWT token for WebSocket authentication.
func (s *Subscriber) generateJWT(subject string, channels []string, exp time.Duration) (string, error) {
	if channels == nil {
		channels = []string{"*"}
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":      subject,
		"channels": channels,
		"iat":      now.Unix(),
		"exp":      now.Add(exp).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.jwtSecret))
}

// Connect establishes a WebSocket connection.
func (s *Subscriber) Connect(ctx context.Context, subject string) error {
	token, err := s.generateJWT(subject, []string{"*"}, time.Hour)
	if err != nil {
		return fmt.Errorf("generate jwt: %w", err)
	}

	conn, _, err := websocket.Dial(ctx, s.wsURL(token), &websocket.DialOptions{
		HTTPHeader: nil,
	})
	if err != nil {
		return fmt.Errorf("dial ws: %w", err)
	}

	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	return nil
}

// Subscribe sends a subscribe frame over the WebSocket.
func (s *Subscriber) Subscribe(ctx context.Context, channels []string, afterSeq map[string]int64) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	req := map[string]interface{}{
		"type":     "subscribe",
		"channels": channels,
	}
	if len(afterSeq) > 0 {
		req["after_seq"] = afterSeq
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal subscribe: %w", err)
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// ReadLoop reads frames until the connection closes. For each message frame,
// it extracts a _bench_ts if present and records the end-to-end latency.
// Returns the total number of messages received.
func (s *Subscriber) ReadLoop(ctx context.Context) int {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return 0
	}

	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			return int(s.messagesReceived)
		}
		if msgType != websocket.MessageText {
			continue
		}

		// Parse frame to extract benchmark timestamp.
		var frame struct {
			Type    string          `json:"type"`
			SeqID   int64           `json:"seq_id"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}

		if frame.Type == "message" {
			s.messagesReceived++

			// Try to extract _bench_ts from payload.
			var payload struct {
				BenchTS int64 `json:"_bench_ts"`
			}
			if err := json.Unmarshal(frame.Payload, &payload); err == nil && payload.BenchTS > 0 {
				ts := time.Unix(0, payload.BenchTS)
				d := time.Since(ts)

				s.mu.Lock()
				s.e2eLatencies = append(s.e2eLatencies, d)
				s.mu.Unlock()
			}
		}
	}
}

// ReadMessage reads a single WebSocket message. Returns msgType, data, error.
func (s *Subscriber) ReadMessage(ctx context.Context) (websocket.MessageType, []byte, error) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return 0, nil, fmt.Errorf("not connected")
	}
	return conn.Read(ctx)
}

// Stats returns aggregate subscriber statistics.
func (s *Subscriber) Stats() (messagesReceived int64, e2eLatencies []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messagesReceived, append([]time.Duration(nil), s.e2eLatencies...)
}

// Close closes the WebSocket connection.
func (s *Subscriber) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.conn.Close(websocket.StatusNormalClosure, "bench done")
	}
	return nil
}

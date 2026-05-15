package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/store"
)

// --- mockHub ---

type mockHub struct {
	publishErr error
	seqID      int64
	timestamp  time.Time
}

func newMockHub() *mockHub {
	return &mockHub{seqID: 1, timestamp: time.Now().UTC()}
}

func (h *mockHub) Publish(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (int64, time.Time, error) {
	if h.publishErr != nil {
		return 0, time.Time{}, h.publishErr
	}
	return h.seqID, h.timestamp, nil
}

func (h *mockHub) Subscribe(conn *hub.Connection, channels []string, afterSeq map[string]int64) error {
	return nil
}
func (h *mockHub) Unsubscribe(conn *hub.Connection, channels []string) {}
func (h *mockHub) RemoveConnection(conn *hub.Connection)                {}

// --- mockAuth ---

type mockAuth struct {
	validAPIKeys map[string]bool
}

func newMockAuth() *mockAuth {
	return &mockAuth{validAPIKeys: map[string]bool{"valid-key": true}}
}

func (a *mockAuth) ValidateAPIKey(key string) bool {
	return a.validAPIKeys[key]
}

func (a *mockAuth) ParseAndValidateToken(tokenString string) (*auth.Claims, error) {
	return nil, nil
}

func (a *mockAuth) IsChannelAuthorized(claims *auth.Claims, channel string) bool {
	return true
}

// --- mockStore ---

type mockStore struct {
	history      map[string]*store.HistoryResult
	pingErr      error
	historyErr   error
}

func newMockStore() *mockStore {
	return &mockStore{history: make(map[string]*store.HistoryResult)}
}

func (s *mockStore) RunMigrations(ctx context.Context) error { return nil }
func (s *mockStore) Close()                                    {}

func (s *mockStore) Ping(ctx context.Context) error {
	return s.pingErr
}

func (s *mockStore) WriteMessage(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (int64, time.Time, error) {
	return 0, time.Time{}, nil
}

func (s *mockStore) ReadHistory(ctx context.Context, channel string, afterSeq int64, limit int) (*store.HistoryResult, error) {
	if s.historyErr != nil {
		return nil, s.historyErr
	}
	result, ok := s.history[channel]
	if !ok {
		return &store.HistoryResult{Messages: nil, MinSeq: 0}, nil
	}
	if len(result.Messages) > limit {
		return &store.HistoryResult{Messages: result.Messages[:limit], MinSeq: result.MinSeq}, nil
	}
	return result, nil
}

func (s *mockStore) EvictExpiredMessages(ctx context.Context) (int, int, error) {
	return 0, 0, nil
}

// --- test helpers ---

func newTestServer(t *testing.T) (*Server, *mockHub, *mockAuth, *mockStore) {
	t.Helper()
	h := newMockHub()
	a := newMockAuth()
	s := newMockStore()
	cfg := ServerConfig{MaxPayloadSize: 65536}
	server := New(h, a, s, cfg)
	return server, h, a, s
}

func doRequest(t *testing.T, server *Server, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)
	return w.Result()
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return v
}

// --- tests: publish ---

func TestPublish_Success(t *testing.T) {
	server, h, _, _ := newTestServer(t)

	body := strings.NewReader(`{"channel":"test.channel","payload":{"msg":"hello"}}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	if json["ok"] != true {
		t.Fatalf("expected ok=true, got %v", json["ok"])
	}
	if json["seq_id"] != float64(h.seqID) {
		t.Fatalf("expected seq_id=%d, got %v", h.seqID, json["seq_id"])
	}
}

func TestPublish_Unauthorized(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	body := strings.NewReader(`{"channel":"test.channel","payload":"data"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer invalid-key",
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeInvalidAPIKey) {
		t.Fatalf("expected error code %d, got %v", ErrCodeInvalidAPIKey, errObj["code"])
	}
}

func TestPublish_MissingAuthHeader(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	body := strings.NewReader(`{"channel":"test.channel","payload":"data"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, nil)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestPublish_MissingChannel(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	body := strings.NewReader(`{"payload":"data"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeMissingField) {
		t.Fatalf("expected error code %d, got %v", ErrCodeMissingField, errObj["code"])
	}
}

func TestPublish_MissingPayload(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	body := strings.NewReader(`{"channel":"test.channel"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeMissingField) {
		t.Fatalf("expected error code %d, got %v", ErrCodeMissingField, errObj["code"])
	}
}

func TestPublish_InvalidChannelName(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	body := strings.NewReader(`{"channel":"invalid*channel","payload":"data"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeInvalidChannel) {
		t.Fatalf("expected error code %d, got %v", ErrCodeInvalidChannel, errObj["code"])
	}
}

func TestPublish_InvalidJSON(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	body := strings.NewReader(`not json`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeInvalidJSON) {
		t.Fatalf("expected error code %d, got %v", ErrCodeInvalidJSON, errObj["code"])
	}
}

func TestPublish_PayloadTooLarge(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	server.cfg.MaxPayloadSize = 10

	body := strings.NewReader(`{"channel":"test.channel","payload":"` + strings.Repeat("x", 100) + `"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodePayloadTooLarge) {
		t.Fatalf("expected error code %d, got %v", ErrCodePayloadTooLarge, errObj["code"])
	}
}

func TestPublish_WithIdempotencyKey(t *testing.T) {
	server, h, _, _ := newTestServer(t)

	body := strings.NewReader(`{"channel":"test.channel","payload":"data","idempotency_key":"idem-1"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	if json["ok"] != true {
		t.Fatalf("expected ok=true, got %v", json["ok"])
	}
	if json["seq_id"] != float64(h.seqID) {
		t.Fatalf("expected seq_id=%d, got %v", h.seqID, json["seq_id"])
	}
}

func TestPublish_StorageFailure(t *testing.T) {
	server, h, _, _ := newTestServer(t)
	h.publishErr = fmt.Errorf("postgresql not available")

	body := strings.NewReader(`{"channel":"test.channel","payload":"data"}`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeStorageFailure) {
		t.Fatalf("expected error code %d, got %v", ErrCodeStorageFailure, errObj["code"])
	}
}

// --- tests: history ---

func TestHistory_Success(t *testing.T) {
	server, _, _, s := newTestServer(t)
	s.history["test.channel"] = &store.HistoryResult{
		Messages: []store.Message{
			{SeqID: 1, Payload: json.RawMessage(`"hello"`), CreatedAt: time.Now()},
			{SeqID: 2, Payload: json.RawMessage(`"world"`), CreatedAt: time.Now()},
		},
		MinSeq: 1,
	}

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test.channel&limit=10", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	if json["ok"] != true {
		t.Fatalf("expected ok=true, got %v", json["ok"])
	}
	messages := json["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
}

func TestHistory_ChannelNotFound(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=nonexistent", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	if json["ok"] != true {
		t.Fatalf("expected ok=true, got %v", json["ok"])
	}
	messages := json["messages"].([]any)
	if len(messages) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(messages))
	}
}

func TestHistory_StorageFailure(t *testing.T) {
	server, _, _, s := newTestServer(t)
	s.historyErr = fmt.Errorf("postgresql not available")

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeStorageFailure) {
		t.Fatalf("expected error code %d, got %v", ErrCodeStorageFailure, errObj["code"])
	}
}

func TestHistory_MissingChannel(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/api/v1/history", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeMissingField) {
		t.Fatalf("expected error code %d, got %v", ErrCodeMissingField, errObj["code"])
	}
}

func TestHistory_InvalidChannelName(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=bad*name", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	errObj := json["error"].(map[string]any)
	if errObj["code"] != float64(ErrCodeInvalidChannel) {
		t.Fatalf("expected error code %d, got %v", ErrCodeInvalidChannel, errObj["code"])
	}
}

func TestHistory_InvalidAfterSeq(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test&after_seq=-1", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHistory_InvalidLimit(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test&limit=0", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHistory_LimitCap(t *testing.T) {
	server, _, _, s := newTestServer(t)

	var msgs []store.Message
	for i := int64(1); i <= 1001; i++ {
		msgs = append(msgs, store.Message{SeqID: i, Payload: json.RawMessage(`"x"`)})
	}
	s.history["test"] = &store.HistoryResult{Messages: msgs, MinSeq: 1}

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test&limit=2000", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	messages := json["messages"].([]any)
	// limit should be capped at 1000
	if len(messages) > 1000 {
		t.Fatalf("expected limit capped to 1000, got %d", len(messages))
	}
}

func TestHistory_Unauthorized(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test", nil, map[string]string{
		"Authorization": "Bearer bad-key",
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHistory_HasMore(t *testing.T) {
	server, _, _, s := newTestServer(t)

	var msgs []store.Message
	for i := int64(1); i <= 10; i++ {
		msgs = append(msgs, store.Message{SeqID: i, Payload: json.RawMessage(`"x"`)})
	}
	s.history["test"] = &store.HistoryResult{Messages: msgs, MinSeq: 1}

	// Request exactly 10 with 10 available → has_more should be true
	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test&limit=10", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	if json["has_more"] != true {
		t.Fatalf("expected has_more=true when limit matches result count, got %v", json["has_more"])
	}

	// Request 20 with only 10 available → has_more should be false
	resp = doRequest(t, server, "GET", "/api/v1/history?channel=test&limit=20", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	json = readJSON(t, resp)
	if json["has_more"] != false {
		t.Fatalf("expected has_more=false when results < limit, got %v", json["has_more"])
	}
}

func TestHistory_DefaultLimit(t *testing.T) {
	server, _, _, s := newTestServer(t)

	var msgs []store.Message
	for i := int64(1); i <= 50; i++ {
		msgs = append(msgs, store.Message{SeqID: i, Payload: json.RawMessage(`"x"`)})
	}
	s.history["test"] = &store.HistoryResult{Messages: msgs, MinSeq: 1}

	// No limit specified → should default to 100, return all 50
	resp := doRequest(t, server, "GET", "/api/v1/history?channel=test", nil, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	messages := json["messages"].([]any)
	if len(messages) != 50 {
		t.Fatalf("expected 50 messages with default limit, got %d", len(messages))
	}
}

// --- tests: ops endpoints ---

func TestHealthz_OK(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/healthz", nil, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHealthz_Unhealthy(t *testing.T) {
	server, _, _, s := newTestServer(t)
	s.pingErr = fmt.Errorf("db down")

	resp := doRequest(t, server, "GET", "/healthz", nil, nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

func TestReadyz_NotReady(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	// Server not started yet, ready is false by default

	resp := doRequest(t, server, "GET", "/readyz", nil, nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

func TestReadyz_Ready(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	server.ready.Store(true)

	resp := doRequest(t, server, "GET", "/readyz", nil, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestMetricsz(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	resp := doRequest(t, server, "GET", "/metricsz", nil, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Fatalf("expected text/plain content type, got %s", contentType)
	}
}

// --- tests: error response format ---

func TestErrorResponseFormat(t *testing.T) {
	server, _, _, _ := newTestServer(t)

	body := strings.NewReader(`not json`)
	resp := doRequest(t, server, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer valid-key",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	json := readJSON(t, resp)
	if json["ok"] != false {
		t.Fatal("expected ok=false in error response")
	}
	errObj, ok := json["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["code"] == nil {
		t.Fatal("expected error.code to be present")
	}
	if errObj["message"] == nil {
		t.Fatal("expected error.message to be present")
	}
}

// --- tests: Shutdown ---

func TestShutdown_ReadyState(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	server.ready.Store(true)
	ctx := context.Background()

	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown should succeed when srv is nil: %v", err)
	}
	if server.ready.Load() {
		t.Fatal("expected not ready after Shutdown")
	}
}

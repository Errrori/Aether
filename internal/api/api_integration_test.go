//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aether-mq/aether/internal/auth"
	"github.com/aether-mq/aether/internal/config"
	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/keymgmt"
	"github.com/aether-mq/aether/internal/store"
)

// --- helpers ---

func testDSN() string {
	if dsn := os.Getenv("AETHER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://aether:aether@localhost:5433/aether_test?sslmode=disable"
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func integNewTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbCfg := &config.DatabaseConfig{
		DSN:             testDSN(),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxIdleTime: time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
	}
	retCfg := &config.RetentionConfig{
		DefaultTTL:       720 * time.Hour,
		DefaultMaxCount:  10000,
		EvictionInterval: 5 * time.Minute,
	}

	st, err := store.New(ctx, dbCfg, retCfg)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.RunMigrations(ctx); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return st
}

func integNewTestAuth(t *testing.T, ks store.KeyStore) auth.Auth {
	t.Helper()
	cfg := &config.AuthConfig{
		JWTSigningKey: strings.Repeat("a", 32),
		JWTClockSkew:  30 * time.Second,
	}
	a, err := auth.New(cfg, ks)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return a
}

func integCreateTestAPIKey(t *testing.T, ks store.KeyStore, name string, perms store.KeyPermissions) string {
	t.Helper()
	rawKey := fmt.Sprintf("aek_integration_test_key_%s_%d", name, time.Now().UnixNano())
	hash := sha256Hex(rawKey)
	id := fmt.Sprintf("id-%s-%d", name, time.Now().UnixNano())

	kp := &store.APIKey{
		ID:          id,
		Name:        name,
		KeyHash:     hash,
		KeyPrefix:   rawKey[:8],
		Permissions: perms,
	}
	if err := ks.CreateAPIKey(context.Background(), kp); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return rawKey
}

func integNewTestServer(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	st := integNewTestStore(t)
	ks := st.(store.KeyStore)
	a := integNewTestAuth(t, ks)
	km := keymgmt.New(ks)

	h := hub.New(st, a, hub.HubConfig{
		OutboundBufferSize:      256,
		MaxChannelsPerSubscribe: 100,
		MaxChannelsPerConn:      1000,
		HistoryLimit:            1000,
	}, hub.NopMetrics())

	srv := New(h, a, st, km, ks, nil, ServerConfig{MaxPayloadSize: 65536})

	adminKey := integCreateTestAPIKey(t, ks, "admin", store.KeyPermissions{
		Publish:   []string{"*"},
		Subscribe: []string{"*"},
		Admin:     true,
	})

	return srv, st, adminKey
}

// --- publish ---

func TestIntegration_Publish_Success(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	channel := "int-pub-" + t.Name()

	body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":{"msg":"hello"}}`, channel))
	resp := doRequest(t, srv, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer " + adminKey,
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	seqID, ok := result["seq_id"].(float64)
	if !ok || seqID != 1 {
		t.Fatalf("expected seq_id=1, got %v", result["seq_id"])
	}
	if result["timestamp"] == "" {
		t.Fatal("expected non-empty timestamp")
	}

	history, err := st.ReadHistory(context.Background(), channel, 0, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(history.Messages) != 1 {
		t.Fatalf("expected 1 message in history, got %d", len(history.Messages))
	}
	if string(history.Messages[0].Payload) != `{"msg":"hello"}` {
		t.Fatalf("unexpected payload: %s", string(history.Messages[0].Payload))
	}
}

func TestIntegration_Publish_WithIdempotencyKey(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	channel := "int-idem-" + t.Name()

	payload := fmt.Sprintf(`{"channel":"%s","payload":"data","idempotency_key":"ik-1"}`, channel)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	resp1 := doRequest(t, srv, "POST", "/api/v1/publish", strings.NewReader(payload), authHeader)
	result1 := readJSON(t, resp1)

	resp2 := doRequest(t, srv, "POST", "/api/v1/publish", strings.NewReader(payload), authHeader)
	result2 := readJSON(t, resp2)

	if resp1.StatusCode != http.StatusOK || resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d / %d", resp1.StatusCode, resp2.StatusCode)
	}
	if result1["seq_id"] != result2["seq_id"] {
		t.Fatalf("seq_id mismatch: %v vs %v", result1["seq_id"], result2["seq_id"])
	}

	history, err := st.ReadHistory(context.Background(), channel, 0, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(history.Messages) != 1 {
		t.Fatalf("expected 1 message in history, got %d", len(history.Messages))
	}
}

func TestIntegration_Publish_SequentialSeqIDs(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-seq-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	for i := 1; i <= 5; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"msg-%d"}`, channel, i))
		resp := doRequest(t, srv, "POST", "/api/v1/publish", body, authHeader)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("publish %d: expected 200, got %d", i, resp.StatusCode)
		}
		result := readJSON(t, resp)
		if seqID, ok := result["seq_id"].(float64); !ok || int64(seqID) != int64(i) {
			t.Fatalf("publish %d: expected seq_id=%d, got %v", i, i, result["seq_id"])
		}
	}
}

func TestIntegration_Publish_MultipleChannels(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	chA := "int-ch-a-" + t.Name()
	chB := "int-ch-b-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	doRequest(t, srv, "POST", "/api/v1/publish", strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"a1"}`, chA)), authHeader)
	doRequest(t, srv, "POST", "/api/v1/publish", strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"b1"}`, chB)), authHeader)

	histA, err := st.ReadHistory(context.Background(), chA, 0, 10)
	if err != nil {
		t.Fatalf("ReadHistory chA: %v", err)
	}
	histB, err := st.ReadHistory(context.Background(), chB, 0, 10)
	if err != nil {
		t.Fatalf("ReadHistory chB: %v", err)
	}

	if len(histA.Messages) != 1 || string(histA.Messages[0].Payload) != `"a1"` {
		t.Fatal("channel-a should contain only its own message")
	}
	if len(histB.Messages) != 1 || string(histB.Messages[0].Payload) != `"b1"` {
		t.Fatal("channel-b should contain only its own message")
	}
}

func TestIntegration_Publish_Unauthorized(t *testing.T) {
	srv, _, _ := integNewTestServer(t)

	body := strings.NewReader(`{"channel":"test.chan","payload":"data"}`)
	resp := doRequest(t, srv, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer invalid-key-not-in-db",
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	result := readJSON(t, resp)
	errObj, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if code, ok := errObj["code"].(float64); !ok || int(code) != ErrCodeInvalidAPIKey {
		t.Fatalf("expected error code %d, got %v", ErrCodeInvalidAPIKey, errObj["code"])
	}
}

func TestIntegration_Publish_PayloadTooLarge(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	srv.cfg.MaxPayloadSize = 100

	bigPayload := strings.Repeat("x", 200)
	body := strings.NewReader(fmt.Sprintf(`{"channel":"test.chan","payload":"%s"}`, bigPayload))
	resp := doRequest(t, srv, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer " + adminKey,
	})

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	result := readJSON(t, resp)
	errObj := result["error"].(map[string]any)
	if code, ok := errObj["code"].(float64); !ok || int(code) != ErrCodePayloadTooLarge {
		t.Fatalf("expected error code %d, got %v", ErrCodePayloadTooLarge, errObj["code"])
	}
}

func TestIntegration_Publish_InvalidJSON(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)

	body := strings.NewReader("not json")
	resp := doRequest(t, srv, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer " + adminKey,
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	result := readJSON(t, resp)
	errObj := result["error"].(map[string]any)
	if code, ok := errObj["code"].(float64); !ok || int(code) != ErrCodeInvalidJSON {
		t.Fatalf("expected error code %d, got %v", ErrCodeInvalidJSON, errObj["code"])
	}
}

func TestIntegration_Publish_MissingChannel(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)

	body := strings.NewReader(`{"payload":"data"}`)
	resp := doRequest(t, srv, "POST", "/api/v1/publish", body, map[string]string{
		"Authorization": "Bearer " + adminKey,
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	result := readJSON(t, resp)
	errObj := result["error"].(map[string]any)
	if code, ok := errObj["code"].(float64); !ok || int(code) != ErrCodeMissingField {
		t.Fatalf("expected error code %d, got %v", ErrCodeMissingField, errObj["code"])
	}
}

// --- history ---

func TestIntegration_History_Success(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-hist-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	for i := 1; i <= 3; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"msg-%d"}`, channel, i))
		resp := doRequest(t, srv, "POST", "/api/v1/publish", body, authHeader)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("publish %d: expected 200, got %d", i, resp.StatusCode)
		}
	}

	resp := doRequest(t, srv, "GET", "/api/v1/history?channel="+channel, nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	messages, ok := result["messages"].([]interface{})
	if !ok {
		t.Fatal("expected messages array")
	}
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
}

func TestIntegration_History_WithAfterSeq(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-after-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	for i := 1; i <= 5; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"msg-%d"}`, channel, i))
		doRequest(t, srv, "POST", "/api/v1/publish", body, authHeader)
	}

	resp := doRequest(t, srv, "GET", fmt.Sprintf("/api/v1/history?channel=%s&after_seq=2", channel), nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	messages := result["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages (after seq 2), got %d", len(messages))
	}
}

func TestIntegration_History_WithLimit(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-limit-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	for i := 1; i <= 5; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"msg-%d"}`, channel, i))
		doRequest(t, srv, "POST", "/api/v1/publish", body, authHeader)
	}

	resp := doRequest(t, srv, "GET", fmt.Sprintf("/api/v1/history?channel=%s&limit=2", channel), nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	messages := result["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if result["has_more"] != true {
		t.Fatal("expected has_more=true")
	}
}

func TestIntegration_History_DefaultLimit(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-deflimit-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	for i := 1; i <= 50; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"msg-%d"}`, channel, i))
		doRequest(t, srv, "POST", "/api/v1/publish", body, authHeader)
	}

	resp := doRequest(t, srv, "GET", "/api/v1/history?channel="+channel, nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	messages := result["messages"].([]interface{})
	if len(messages) != 50 {
		t.Fatalf("expected 50 messages (default limit 100), got %d", len(messages))
	}
}

func TestIntegration_History_HasMore(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-hasmore-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	for i := 1; i <= 10; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"msg-%d"}`, channel, i))
		doRequest(t, srv, "POST", "/api/v1/publish", body, authHeader)
	}

	resp1 := doRequest(t, srv, "GET", fmt.Sprintf("/api/v1/history?channel=%s&limit=10", channel), nil, authHeader)
	result1 := readJSON(t, resp1)
	if result1["has_more"] != true {
		t.Fatal("limit=10 with 10 messages: expected has_more=true")
	}

	resp2 := doRequest(t, srv, "GET", fmt.Sprintf("/api/v1/history?channel=%s&limit=20", channel), nil, authHeader)
	result2 := readJSON(t, resp2)
	if result2["has_more"] != false {
		t.Fatal("limit=20 with 10 messages: expected has_more=false")
	}
}

func TestIntegration_History_EmptyChannel(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-empty-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	resp := doRequest(t, srv, "GET", "/api/v1/history?channel="+channel, nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	messages := result["messages"].([]interface{})
	if len(messages) != 0 {
		t.Fatalf("expected 0 messages for empty channel, got %d", len(messages))
	}
}

func TestIntegration_History_CrossChannelIsolation(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	chA := "int-iso-a-" + t.Name()
	chB := "int-iso-b-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	doRequest(t, srv, "POST", "/api/v1/publish", strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"a"}`, chA)), authHeader)
	doRequest(t, srv, "POST", "/api/v1/publish", strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"b"}`, chB)), authHeader)

	respA := doRequest(t, srv, "GET", "/api/v1/history?channel="+chA, nil, authHeader)
	respB := doRequest(t, srv, "GET", "/api/v1/history?channel="+chB, nil, authHeader)

	resultA := readJSON(t, respA)
	resultB := readJSON(t, respB)

	if len(resultA["messages"].([]interface{})) != 1 {
		t.Fatal("channel-a should have 1 message")
	}
	if len(resultB["messages"].([]interface{})) != 1 {
		t.Fatal("channel-b should have 1 message")
	}
}

func TestIntegration_History_Unauthorized(t *testing.T) {
	srv, _, _ := integNewTestServer(t)

	resp := doRequest(t, srv, "GET", "/api/v1/history?channel=test.chan", nil, map[string]string{
		"Authorization": "Bearer invalid-key",
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestIntegration_History_InvalidParameters(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	// Missing channel
	resp := doRequest(t, srv, "GET", "/api/v1/history", nil, authHeader)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing channel: expected 400, got %d", resp.StatusCode)
	}

	// Negative after_seq
	resp = doRequest(t, srv, "GET", "/api/v1/history?channel=test.chan&after_seq=-1", nil, authHeader)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative after_seq: expected 400, got %d", resp.StatusCode)
	}

	// Zero limit
	resp = doRequest(t, srv, "GET", "/api/v1/history?channel=test.chan&limit=0", nil, authHeader)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero limit: expected 400, got %d", resp.StatusCode)
	}
}

// --- ops ---

func TestIntegration_Healthz_OK(t *testing.T) {
	srv, _, _ := integNewTestServer(t)

	resp := doRequest(t, srv, "GET", "/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", result["status"])
	}
}

func TestIntegration_Readyz_Ready(t *testing.T) {
	srv, _, _ := integNewTestServer(t)
	srv.ready.Store(true)

	resp := doRequest(t, srv, "GET", "/readyz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", result["status"])
	}
}

func TestIntegration_Readyz_NotReady(t *testing.T) {
	srv, _, _ := integNewTestServer(t)
	// ready defaults to false

	resp := doRequest(t, srv, "GET", "/readyz", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

// --- v2 key management ---

func TestIntegration_CreateKey_Success(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	ks := st.(store.KeyStore)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	name := "int-create-key-" + t.Name()
	body := strings.NewReader(fmt.Sprintf(`{"name":"%s","publish":["orders.*"],"subscribe":["*"],"admin":false}`, name))
	resp := doRequest(t, srv, "POST", "/api/v2/keys", body, authHeader)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	keyVal, ok := result["key"].(string)
	if !ok || len(keyVal) < 43 {
		t.Fatalf("expected key with length >= 43, got %q", result["key"])
	}
	if keyVal[:4] != "aek_" {
		t.Fatalf("expected key prefix aek_, got %v", keyVal[:4])
	}
	if result["meta"] == nil {
		t.Fatal("expected non-nil meta")
	}

	hash := sha256Hex(keyVal)
	dbKey, err := ks.GetAPIKeyByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if dbKey.Name != name {
		t.Fatalf("expected name int-create-key, got %s", dbKey.Name)
	}
}

func TestIntegration_CreateKey_DuplicateName(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	body := strings.NewReader(fmt.Sprintf(`{"name":"int-dup-via-http-%s"}`, t.Name()))
	resp1 := doRequest(t, srv, "POST", "/api/v2/keys", body, authHeader)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first create: expected 200, got %d", resp1.StatusCode)
	}

	resp := doRequest(t, srv, "POST", "/api/v2/keys", body, authHeader)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	result := readJSON(t, resp)
	errObj := result["error"].(map[string]any)
	if code, ok := errObj["code"].(float64); !ok || int(code) != ErrCodeKeyNameConflict {
		t.Fatalf("expected error code %d, got %v", ErrCodeKeyNameConflict, errObj["code"])
	}
}

func TestIntegration_CreateKey_MissingName(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	body := strings.NewReader(`{"publish":["*"]}`)
	resp := doRequest(t, srv, "POST", "/api/v2/keys", body, authHeader)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestIntegration_CreateKey_NotAdmin(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	ks := st.(store.KeyStore)

	nonAdminKey := integCreateTestAPIKey(t, ks, "nonadmin", store.KeyPermissions{
		Publish: []string{"*"}, Subscribe: []string{"*"}, Admin: false,
	})

	body := strings.NewReader(`{"name":"should-fail"}`)
	resp := doRequest(t, srv, "POST", "/api/v2/keys", body, map[string]string{
		"Authorization": "Bearer " + nonAdminKey,
	})

	_ = adminKey // only used to set up the server

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	result := readJSON(t, resp)
	errObj := result["error"].(map[string]any)
	if code, ok := errObj["code"].(float64); !ok || int(code) != ErrCodeNotAdmin {
		t.Fatalf("expected error code %d, got %v", ErrCodeNotAdmin, errObj["code"])
	}
}

func TestIntegration_CreateKey_Unauthorized(t *testing.T) {
	srv, _, _ := integNewTestServer(t)

	body := strings.NewReader(`{"name":"test"}`)
	resp := doRequest(t, srv, "POST", "/api/v2/keys", body, map[string]string{
		"Authorization": "Bearer invalid-key",
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestIntegration_CreateKey_WithExpiresIn(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	ks := st.(store.KeyStore)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	body := strings.NewReader(fmt.Sprintf(`{"name":"int-expires-key-%s","expires_in":"1h"}`, t.Name()))
	resp := doRequest(t, srv, "POST", "/api/v2/keys", body, authHeader)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	meta, ok := result["meta"].(map[string]any)
	if !ok {
		t.Fatal("expected meta object in response")
	}
	if _, ok := meta["expires_at"].(string); !ok || meta["expires_at"].(string) == "" {
		t.Fatal("expected non-empty expires_at")
	}

	// Verify the key exists in the store.
	keyVal := result["key"].(string)
	hash := sha256Hex(keyVal)
	dbKey, err := ks.GetAPIKeyByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if dbKey.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set in DB")
	}
}

func TestIntegration_ListKeys_Success(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	// Create a couple of keys via the HTTP API.
	for i := 0; i < 2; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"name":"list-key-%s-%d"}`, t.Name(), i))
		resp := doRequest(t, srv, "POST", "/api/v2/keys", body, authHeader)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create key %d: expected 200, got %d", i, resp.StatusCode)
		}
	}

	resp := doRequest(t, srv, "GET", "/api/v2/keys", nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	keys, ok := result["keys"].([]interface{})
	if !ok {
		t.Fatal("expected keys array")
	}
	if len(keys) < 2 {
		t.Fatalf("expected at least 2 keys, got %d", len(keys))
	}
}

func TestIntegration_ListKeys_ContainsAdminKey(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	resp := doRequest(t, srv, "GET", "/api/v2/keys", nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	keys := result["keys"].([]interface{})
	if len(keys) < 1 {
		t.Fatalf("expected at least 1 key (admin), got %d", len(keys))
	}
}

func TestIntegration_GetKey_Success(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	// Create key via HTTP
	createBody := strings.NewReader(fmt.Sprintf(`{"name":"get-key-test-%s"}`, t.Name()))
	createResp := doRequest(t, srv, "POST", "/api/v2/keys", createBody, authHeader)
	createResult := readJSON(t, createResp)
	meta := createResult["meta"].(map[string]any)
	keyID := meta["id"].(string)

	// Get key by ID
	resp := doRequest(t, srv, "GET", "/api/v2/keys/"+keyID, nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	if result["meta"] == nil {
		t.Fatal("expected non-nil meta")
	}
}

func TestIntegration_GetKey_NotFound(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	resp := doRequest(t, srv, "GET", "/api/v2/keys/nonexistent-id", nil, authHeader)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	errObj := result["error"].(map[string]any)
	if code, ok := errObj["code"].(float64); !ok || int(code) != ErrCodeKeyNotFound {
		t.Fatalf("expected error code %d, got %v", ErrCodeKeyNotFound, errObj["code"])
	}
}

func TestIntegration_RevokeKey_Success(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	ks := st.(store.KeyStore)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	createBody := strings.NewReader(fmt.Sprintf(`{"name":"revoke-key-test-%s"}`, t.Name()))
	createResp := doRequest(t, srv, "POST", "/api/v2/keys", createBody, authHeader)
	createResult := readJSON(t, createResp)
	meta := createResult["meta"].(map[string]any)
	keyID := meta["id"].(string)

	resp := doRequest(t, srv, "DELETE", "/api/v2/keys/"+keyID, nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	dbKey, err := ks.GetAPIKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if dbKey.RevokedAt == nil {
		t.Fatal("expected RevokedAt to be set")
	}
}

func TestIntegration_RevokeKey_NotFound(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	resp := doRequest(t, srv, "DELETE", "/api/v2/keys/nonexistent-id", nil, authHeader)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestIntegration_RotateKey_Success(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	ks := st.(store.KeyStore)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	createBody := strings.NewReader(fmt.Sprintf(`{"name":"rotate-key-test-%s"}`, t.Name()))
	createResp := doRequest(t, srv, "POST", "/api/v2/keys", createBody, authHeader)
	createResult := readJSON(t, createResp)
	meta := createResult["meta"].(map[string]any)
	keyID := meta["id"].(string)

	resp := doRequest(t, srv, "POST", "/api/v2/keys/"+keyID+"/rotate", nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %v", result["ok"])
	}
	newKey, ok := result["key"].(string)
	if !ok || len(newKey) < 43 {
		t.Fatalf("expected new key with length >= 43, got %q", result["key"])
	}

	// Old hash should no longer work.
	oldKey := createResult["key"].(string)
	oldHash := sha256Hex(oldKey)
	_, err := ks.GetAPIKeyByHash(context.Background(), oldHash)
	if err == nil {
		t.Fatal("expected old hash to be gone after rotation")
	}

	// New hash should work.
	newHash := sha256Hex(newKey)
	dbKey, err := ks.GetAPIKeyByHash(context.Background(), newHash)
	if err != nil {
		t.Fatalf("GetAPIKeyByHash for new key: %v", err)
	}
	if dbKey.ID != keyID {
		t.Fatalf("expected same ID after rotation, got %s", dbKey.ID)
	}
}

func TestIntegration_RotateKey_NotFound(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	resp := doRequest(t, srv, "POST", "/api/v2/keys/nonexistent-id/rotate", nil, authHeader)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- end to end ---

func TestIntegration_PublishThenHistoryRoundTrip(t *testing.T) {
	srv, _, adminKey := integNewTestServer(t)
	channel := "int-e2e-" + t.Name()
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	payloads := []string{`{"msg":"first"}`, `{"msg":"second"}`, `{"msg":"third"}`}
	for _, p := range payloads {
		body := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":%s}`, channel, p))
		resp := doRequest(t, srv, "POST", "/api/v1/publish", body, authHeader)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("publish: expected 200, got %d", resp.StatusCode)
		}
	}

	resp := doRequest(t, srv, "GET", "/api/v1/history?channel="+channel, nil, authHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history: expected 200, got %d", resp.StatusCode)
	}

	result := readJSON(t, resp)
	messages := result["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	for i, p := range payloads {
		msg := messages[i].(map[string]any)
		gotJSON, err := json.Marshal(msg["payload"])
		if err != nil {
			t.Fatalf("marshal payload %d: %v", i, err)
		}
		var expected any
		if err := json.Unmarshal([]byte(p), &expected); err != nil {
			t.Fatalf("unmarshal expected %d: %v", i, err)
		}
		expJSON, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("marshal expected %d: %v", i, err)
		}
		if string(gotJSON) != string(expJSON) {
			t.Fatalf("message %d: expected payload %s, got %s", i, expJSON, gotJSON)
		}
	}
}

func TestIntegration_KeyLifecycleViaAPI(t *testing.T) {
	srv, st, adminKey := integNewTestServer(t)
	ks := st.(store.KeyStore)
	authHeader := map[string]string{"Authorization": "Bearer " + adminKey}

	// Create key
	createBody := strings.NewReader(fmt.Sprintf(`{"name":"lifecycle-key-%s","publish":["*"],"admin":false}`, t.Name()))
	createResp := doRequest(t, srv, "POST", "/api/v2/keys", createBody, authHeader)
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("create: expected 200, got %d", createResp.StatusCode)
	}
	createResult := readJSON(t, createResp)
	newKey := createResult["key"].(string)
	meta := createResult["meta"].(map[string]any)
	keyID := meta["id"].(string)

	// Use the new key to publish
	channel := "int-lifecycle-" + t.Name()
	publishBody := strings.NewReader(fmt.Sprintf(`{"channel":"%s","payload":"hello"}`, channel))
	pubResp := doRequest(t, srv, "POST", "/api/v1/publish", publishBody, map[string]string{
		"Authorization": "Bearer " + newKey,
	})
	if pubResp.StatusCode != http.StatusOK {
		t.Fatalf("publish with new key: expected 200, got %d", pubResp.StatusCode)
	}

	// List keys
	listResp := doRequest(t, srv, "GET", "/api/v2/keys", nil, authHeader)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", listResp.StatusCode)
	}

	// Get key
	getResp := doRequest(t, srv, "GET", "/api/v2/keys/"+keyID, nil, authHeader)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", getResp.StatusCode)
	}

	// Verify in DB
	dbKey, err := ks.GetAPIKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if dbKey.Name != "lifecycle-key-"+t.Name() {
		t.Fatalf("expected name lifecycle-key, got %s", dbKey.Name)
	}

	// Revoke key
	revokeResp := doRequest(t, srv, "DELETE", "/api/v2/keys/"+keyID, nil, authHeader)
	if revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d", revokeResp.StatusCode)
	}

	// After revoke, the key should no longer be usable
	pubResp2 := doRequest(t, srv, "POST", "/api/v1/publish", publishBody, map[string]string{
		"Authorization": "Bearer " + newKey,
	})
	if pubResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("publish after revoke: expected 401, got %d", pubResp2.StatusCode)
	}

	// Get key after revoke should show revoked_at
	getResp2 := doRequest(t, srv, "GET", "/api/v2/keys/"+keyID, nil, authHeader)
	if getResp2.StatusCode != http.StatusOK {
		t.Fatalf("get after revoke: expected 200, got %d", getResp2.StatusCode)
	}
}

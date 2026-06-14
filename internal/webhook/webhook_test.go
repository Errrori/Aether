package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// --- template engine tests ---

func TestResolveChannel_NoPlaceholders(t *testing.T) {
	result, err := ResolveChannel("system.alerts", []byte(`{"unused": true}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "system.alerts" {
		t.Fatalf("expected 'system.alerts', got %q", result)
	}
}

func TestResolveChannel_NoPlaceholders_InvalidChannel(t *testing.T) {
	_, err := ResolveChannel("bad*channel", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for invalid channel name")
	}
}

func TestResolveChannel_Simple(t *testing.T) {
	result, err := ResolveChannel("{order_id}", []byte(`{"order_id": 123}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "123" {
		t.Fatalf("expected '123', got %q", result)
	}
}

func TestResolveChannel_Nested(t *testing.T) {
	payload := []byte(`{"event":{"repository":{"full_name":"foo/bar"}}}`)
	result, err := ResolveChannel("{event.repository.full_name}", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "foo/bar" {
		t.Fatalf("expected 'foo/bar', got %q", result)
	}
}

func TestResolveChannel_MultiplePlaceholders(t *testing.T) {
	payload := []byte(`{"org":"acme","repo":"aether"}`)
	result, err := ResolveChannel("{org}.{repo}", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "acme.aether" {
		t.Fatalf("expected 'acme.aether', got %q", result)
	}
}

func TestResolveChannel_StringValue(t *testing.T) {
	result, err := ResolveChannel("{tag}", []byte(`{"tag":"v1.0"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "v1.0" {
		t.Fatalf("expected 'v1.0', got %q", result)
	}
}

func TestResolveChannel_NumberValue(t *testing.T) {
	result, err := ResolveChannel("order.{id}", []byte(`{"id":42}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "order.42" {
		t.Fatalf("expected 'order.42', got %q", result)
	}
}

func TestResolveChannel_BoolValue(t *testing.T) {
	result, err := ResolveChannel("flag.{val}", []byte(`{"val":true}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "flag.true" {
		t.Fatalf("expected 'flag.true', got %q", result)
	}
}

func TestResolveChannel_MissingPath(t *testing.T) {
	_, err := ResolveChannel("{event.author.name}", []byte(`{"event":{"type":"push"}}`))
	if err == nil {
		t.Fatal("expected error for missing field")
	}
}

func TestResolveChannel_InvalidJSON(t *testing.T) {
	_, err := ResolveChannel("{field}", []byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestResolveChannel_ObjectValue(t *testing.T) {
	_, err := ResolveChannel("{obj}", []byte(`{"obj":{"nested":true}}`))
	if err == nil {
		t.Fatal("expected error for non-scalar (object) value")
	}
}

func TestResolveChannel_ArrayValue(t *testing.T) {
	_, err := ResolveChannel("{arr}", []byte(`{"arr":[1,2,3]}`))
	if err == nil {
		t.Fatal("expected error for non-scalar (array) value")
	}
}

func TestResolveChannel_EmptyPlaceholder(t *testing.T) {
	_, err := ResolveChannel("channel.{}", []byte(`{"":"val"}`))
	if err == nil {
		t.Fatal("expected error for empty placeholder")
	}
}

func TestResolveChannel_WhitespacePlaceholder(t *testing.T) {
	_, err := ResolveChannel("channel.{ }", []byte(`{"":"val"}`))
	if err == nil {
		t.Fatal("expected error for whitespace-only placeholder")
	}
}

func TestResolveChannel_ResultInvalidChannel(t *testing.T) {
	_, err := ResolveChannel("{field}", []byte(`{"field":"bad*name"}`))
	if err == nil {
		t.Fatal("expected error for invalid resolved channel")
	}
}

func TestResolveChannel_LiteralWithPlaceholders(t *testing.T) {
	result, err := ResolveChannel("events.{type}.{action}", []byte(`{"type":"order","action":"created"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "events.order.created" {
		t.Fatalf("expected 'events.order.created', got %q", result)
	}
}

// --- HMAC signature verification tests ---

func TestVerifySignature_Valid(t *testing.T) {
	secret := []byte("my-secret-key")
	body := []byte(`{"hello":"world"}`)

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !VerifySignature(secret, body, sig) {
		t.Fatal("expected signature verification to succeed")
	}
}

func TestVerifySignature_Invalid(t *testing.T) {
	secret := []byte("my-secret-key")
	body := []byte(`{"hello":"world"}`)

	mac := hmac.New(sha256.New, []byte("wrong-secret"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if VerifySignature(secret, body, sig) {
		t.Fatal("expected signature verification to fail")
	}
}

func TestVerifySignature_Sha256Prefix(t *testing.T) {
	secret := []byte("my-secret-key")
	body := []byte(`{"hello":"world"}`)

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !VerifySignature(secret, body, sig) {
		t.Fatal("expected sha256= prefixed signature to pass")
	}
}

func TestVerifySignature_InvalidHex(t *testing.T) {
	secret := []byte("my-secret-key")
	body := []byte(`{}`)

	if VerifySignature(secret, body, "not-hex") {
		t.Fatal("expected non-hex signature to fail")
	}
}

func TestVerifySignature_EmptySignature(t *testing.T) {
	secret := []byte("my-secret-key")

	if VerifySignature(secret, []byte(`{}`), "") {
		t.Fatal("expected empty signature to fail")
	}
}

// --- JSON scalar coercion ---

func TestScalarToString_Variants(t *testing.T) {
	tests := []struct {
		input    any
		expected string
		valid    bool
	}{
		{"hello", "hello", true},
		{42.0, "42", true},
		{3.14, "3.14", true},
		{true, "true", true},
		{false, "false", true},
		{nil, "null", true},
		{map[string]any{}, "", false},
		{[]any{}, "", false},
	}

	for _, tt := range tests {
		result, ok := scalarToString(tt.input)
		if ok != tt.valid {
			t.Errorf("scalarToString(%v): expected valid=%v, got %v", tt.input, tt.valid, ok)
		}
		if ok && result != tt.expected {
			t.Errorf("scalarToString(%v): expected %q, got %q", tt.input, tt.expected, result)
		}
	}
}

// --- navigateJSON ---

func TestNavigateJSON_EmptySegments(t *testing.T) {
	_, err := navigateJSON(map[string]any{}, nil)
	if err != nil {
		t.Fatalf("empty segments should return the value itself, got: %v", err)
	}
}

func TestNavigateJSON_EmptySegmentInPath(t *testing.T) {
	_, err := navigateJSON(map[string]any{"a": 1}, []string{"a", ""})
	if err == nil {
		t.Fatal("expected error for empty segment in path")
	}
}

func TestNavigateJSON_IndexIntoNonObject(t *testing.T) {
	_, err := navigateJSON("string", []string{"field"})
	if err == nil {
		t.Fatal("expected error when indexing into non-object")
	}
}

// --- round-trip: secret generation and verification ---

func TestSecretRoundTrip(t *testing.T) {
	secret, err := newSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	if len(secret) != 64 { // 32 bytes = 64 hex chars
		t.Fatalf("expected 64 chars, got %d", len(secret))
	}

	body := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !VerifySignature([]byte(secret), body, sig) {
		t.Fatal("round-trip verification failed")
	}
}

// --- JSON unmarshal payload handling ---

func TestResolveChannel_NullValue(t *testing.T) {
	result, err := ResolveChannel("channel.{field}", []byte(`{"field":null}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "channel.null" {
		t.Fatalf("expected 'channel.null', got %q", result)
	}
}

// Ensure JSON numbers unmarshal to float64 in Go.
func TestResolveChannel_JSONNumberFormatting(t *testing.T) {
	type testCase struct {
		payload   string
		expectOK  bool
	}
	tests := []testCase{
		{`{"n": 1}`, true},
		{`{"n": -5}`, true},
		{`{"n": 0}`, true},
	}

	for _, tc := range tests {
		var data map[string]any
		if err := json.Unmarshal([]byte(tc.payload), &data); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.payload, err)
		}
		if _, ok := data["n"].(float64); !ok && tc.expectOK {
			t.Errorf("expected float64 for payload %s, got %T", tc.payload, data["n"])
		}
	}
}

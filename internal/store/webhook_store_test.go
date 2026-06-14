//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
)

func TestCreateWebhook(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID:              "550e8400-e29b-41d4-a716-446655440001",
		Name:            "test-webhook",
		URLToken:        "abc123token",
		ChannelTemplate: "{event.repo}",
		Secret:          "my-secret",
		KeyID:           "some-key-id",
		Active:          true,
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if wh.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be populated")
	}
}

func TestCreateWebhook_DuplicateName(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh1 := &Webhook{
		ID: "w1-id", Name: "dup-name", URLToken: "tok1",
		ChannelTemplate: "{x}", Secret: "s1", KeyID: "k1",
	}
	if err := s.CreateWebhook(context.Background(), wh1); err != nil {
		t.Fatalf("CreateWebhook w1: %v", err)
	}

	wh2 := &Webhook{
		ID: "w2-id", Name: "dup-name", URLToken: "tok2",
		ChannelTemplate: "{y}", Secret: "s2", KeyID: "k2",
	}
	err := s.CreateWebhook(context.Background(), wh2)
	if !errors.Is(err, ErrWebhookNameConflict) {
		t.Fatalf("expected ErrWebhookNameConflict, got: %v", err)
	}
}

func TestGetWebhook(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID: "get-wh-id", Name: "get-wh", URLToken: "get-tok",
		ChannelTemplate: "{repo}", Secret: "sec", KeyID: "k1",
		Active: true,
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	got, err := s.GetWebhook(context.Background(), "get-wh-id")
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.Name != "get-wh" {
		t.Fatalf("expected name 'get-wh', got %q", got.Name)
	}
	if got.ChannelTemplate != "{repo}" {
		t.Fatalf("expected template '{repo}', got %q", got.ChannelTemplate)
	}
	if got.Secret != "sec" {
		t.Fatalf("expected secret 'sec', got %q", got.Secret)
	}
}

func TestGetWebhook_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	_, err := s.GetWebhook(context.Background(), "no-such-id")
	if !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("expected ErrWebhookNotFound, got: %v", err)
	}
}

func TestGetWebhookByURLToken(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID: "bytok-id", Name: "bytok", URLToken: "my-url-token",
		ChannelTemplate: "{x}", Secret: "s", KeyID: "k",
		Active: true,
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	got, err := s.GetWebhookByURLToken(context.Background(), "my-url-token")
	if err != nil {
		t.Fatalf("GetWebhookByURLToken: %v", err)
	}
	if got.ID != "bytok-id" {
		t.Fatalf("expected ID 'bytok-id', got %q", got.ID)
	}
}

func TestGetWebhookByURLToken_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	_, err := s.GetWebhookByURLToken(context.Background(), "no-such-token")
	if !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("expected ErrWebhookNotFound, got: %v", err)
	}
}

func TestListWebhooks(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	for i := 0; i < 3; i++ {
		wh := &Webhook{
			ID:              "list-" + string(rune('a'+i)),
			Name:            "webhook-" + string(rune('0'+i)),
			URLToken:        "tok-" + string(rune('a'+i)),
			ChannelTemplate: "{x}",
			Secret:          "sec",
			KeyID:           "k",
			Active:          true,
		}
		if err := s.CreateWebhook(context.Background(), wh); err != nil {
			t.Fatalf("CreateWebhook %d: %v", i, err)
		}
	}

	list, err := s.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 webhooks, got %d", len(list))
	}
}

func TestListWebhooks_Empty(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	list, err := s.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 webhooks, got %d", len(list))
	}
}

func TestDeleteWebhook(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID: "del-id", Name: "del-wh", URLToken: "del-tok",
		ChannelTemplate: "{x}", Secret: "s", KeyID: "k",
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	if err := s.DeleteWebhook(context.Background(), "del-id"); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}

	_, err := s.GetWebhook(context.Background(), "del-id")
	if !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("expected ErrWebhookNotFound after delete, got: %v", err)
	}
}

func TestDeleteWebhook_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	err := s.DeleteWebhook(context.Background(), "no-such-id")
	if !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("expected ErrWebhookNotFound, got: %v", err)
	}
}

func TestUpdateWebhookSecret(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID: "secret-id", Name: "secret-wh", URLToken: "secret-tok",
		ChannelTemplate: "{x}", Secret: "old-secret", KeyID: "k",
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	if err := s.UpdateWebhookSecret(context.Background(), "secret-id", "new-secret"); err != nil {
		t.Fatalf("UpdateWebhookSecret: %v", err)
	}

	got, err := s.GetWebhook(context.Background(), "secret-id")
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.Secret != "new-secret" {
		t.Fatalf("expected secret 'new-secret', got %q", got.Secret)
	}
}

func TestUpdateWebhookSecret_NotFound(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	err := s.UpdateWebhookSecret(context.Background(), "no-such-id", "hash")
	if !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("expected ErrWebhookNotFound, got: %v", err)
	}
}

func TestCreateDelivery(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID: "delivery-wh-id", Name: "delivery-wh", URLToken: "delivery-tok",
		ChannelTemplate: "{x}", Secret: "sec", KeyID: "k",
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	channel := "test.channel"
	seqID := int64(42)
	d := &WebhookDelivery{
		WebhookID:  "delivery-wh-id",
		Status:     "success",
		DurationMs: 150,
		SeqID:      &seqID,
		Channel:    &channel,
	}
	if err := s.CreateDelivery(context.Background(), d); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}
	if d.ID == 0 {
		t.Fatal("expected delivery ID to be populated")
	}
	if d.CreatedAt.IsZero() {
		t.Fatal("expected delivery CreatedAt to be populated")
	}
}

func TestCreateDelivery_Failed(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID: "fail-wh-id", Name: "fail-wh", URLToken: "fail-tok",
		ChannelTemplate: "{x}", Secret: "s", KeyID: "k",
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	errMsg := "invalid signature"
	respCode := 401
	d := &WebhookDelivery{
		WebhookID:    "fail-wh-id",
		Status:       "failed",
		ResponseCode: &respCode,
		ErrorMessage: &errMsg,
		DurationMs:   50,
	}
	if err := s.CreateDelivery(context.Background(), d); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}
	if d.Status != "failed" {
		t.Fatalf("expected status 'failed', got %q", d.Status)
	}
}

func TestListDeliveries(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	wh := &Webhook{
		ID: "ld-wh-id", Name: "ld-wh", URLToken: "ld-tok",
		ChannelTemplate: "{x}", Secret: "s", KeyID: "k",
	}
	if err := s.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	for i := 0; i < 5; i++ {
		d := &WebhookDelivery{
			WebhookID: "ld-wh-id", Status: "success", DurationMs: 100 + i,
		}
		if err := s.CreateDelivery(context.Background(), d); err != nil {
			t.Fatalf("CreateDelivery %d: %v", i, err)
		}
	}

	list, err := s.ListDeliveries(context.Background(), "ld-wh-id", 3)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 deliveries with limit=3, got %d", len(list))
	}
}

func TestListDeliveries_Empty(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s)

	list, err := s.ListDeliveries(context.Background(), "no-such-wh", 10)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 deliveries, got %d", len(list))
	}
}

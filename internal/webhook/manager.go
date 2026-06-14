package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aether-mq/aether/internal/hub"
	"github.com/aether-mq/aether/internal/store"
)

// WebhookManager manages webhook lifecycle and processing.
type WebhookManager interface {
	CreateWebhook(ctx context.Context, name, channelTemplate, keyID string) (*CreatedWebhook, error)
	ListWebhooks(ctx context.Context) ([]WebhookMeta, error)
	GetWebhook(ctx context.Context, id string) (*WebhookMeta, error)
	DeleteWebhook(ctx context.Context, id string) error
	RotateSecret(ctx context.Context, id string) (*CreatedWebhook, error)
	ReceiveWebhook(ctx context.Context, urlToken string, body []byte, signature string) (*DeliveryResult, error)
}

type manager struct {
	whStore store.WebhookStore
	hub     hub.Hub
	logger  *slog.Logger
}

// New creates a WebhookManager backed by the given store and hub.
func New(whStore store.WebhookStore, h hub.Hub, logger *slog.Logger) WebhookManager {
	return &manager{whStore: whStore, hub: h, logger: logger}
}

// CreateWebhook generates a new webhook with random secret and URL token.
func (m *manager) CreateWebhook(ctx context.Context, name, channelTemplate, keyID string) (*CreatedWebhook, error) {
	id, err := newUUID()
	if err != nil {
		return nil, fmt.Errorf("create webhook: generate id: %w", err)
	}
	urlToken, err := newURLToken()
	if err != nil {
		return nil, fmt.Errorf("create webhook: generate url token: %w", err)
	}
	secret, err := newSecret()
	if err != nil {
		return nil, fmt.Errorf("create webhook: generate secret: %w", err)
	}

	wh := &store.Webhook{
		ID:              id,
		Name:            name,
		URLToken:        urlToken,
		ChannelTemplate: channelTemplate,
		Secret:          secret,
		KeyID:           keyID,
		Active:          true,
	}
	if err := m.whStore.CreateWebhook(ctx, wh); err != nil {
		return nil, err
	}

	return &CreatedWebhook{
		Webhook: WebhookMeta{
			ID:              wh.ID,
			Name:            wh.Name,
			ChannelTemplate: wh.ChannelTemplate,
			KeyID:           wh.KeyID,
			Active:          wh.Active,
			CreatedAt:       wh.CreatedAt,
			UpdatedAt:       wh.UpdatedAt,
		},
		Secret: secret,
	}, nil
}

// ListWebhooks returns all registered webhooks.
func (m *manager) ListWebhooks(ctx context.Context) ([]WebhookMeta, error) {
	whs, err := m.whStore.ListWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]WebhookMeta, len(whs))
	for i, wh := range whs {
		result[i] = WebhookMeta{
			ID:              wh.ID,
			Name:            wh.Name,
			ChannelTemplate: wh.ChannelTemplate,
			KeyID:           wh.KeyID,
			Active:          wh.Active,
			CreatedAt:       wh.CreatedAt,
			UpdatedAt:       wh.UpdatedAt,
		}
	}
	return result, nil
}

// GetWebhook returns a single webhook by its ID.
func (m *manager) GetWebhook(ctx context.Context, id string) (*WebhookMeta, error) {
	wh, err := m.whStore.GetWebhook(ctx, id)
	if err != nil {
		return nil, err
	}
	return &WebhookMeta{
		ID:              wh.ID,
		Name:            wh.Name,
		ChannelTemplate: wh.ChannelTemplate,
		KeyID:           wh.KeyID,
		Active:          wh.Active,
		CreatedAt:       wh.CreatedAt,
		UpdatedAt:       wh.UpdatedAt,
	}, nil
}

// DeleteWebhook removes a webhook.
func (m *manager) DeleteWebhook(ctx context.Context, id string) error {
	return m.whStore.DeleteWebhook(ctx, id)
}

// RotateSecret generates a new secret for a webhook and returns the plaintext.
func (m *manager) RotateSecret(ctx context.Context, id string) (*CreatedWebhook, error) {
	wh, err := m.whStore.GetWebhook(ctx, id)
	if err != nil {
		return nil, err
	}

	secret, err := newSecret()
	if err != nil {
		return nil, fmt.Errorf("rotate secret: %w", err)
	}

	if err := m.whStore.UpdateWebhookSecret(ctx, id, secret); err != nil {
		return nil, err
	}

	return &CreatedWebhook{
		Webhook: WebhookMeta{
			ID:              wh.ID,
			Name:            wh.Name,
			ChannelTemplate: wh.ChannelTemplate,
			KeyID:           wh.KeyID,
			Active:          wh.Active,
			CreatedAt:       wh.CreatedAt,
			UpdatedAt:       wh.UpdatedAt,
		},
		Secret: secret,
	}, nil
}

// ReceiveWebhook processes an incoming webhook call.
func (m *manager) ReceiveWebhook(ctx context.Context, urlToken string, body []byte, signature string) (*DeliveryResult, error) {
	start := time.Now()

	wh, err := m.whStore.GetWebhookByURLToken(ctx, urlToken)
	if err != nil {
		// Webhook not found; cannot record delivery without a valid webhook_id (FK constraint).
		return nil, err
	}

	if !wh.Active {
		m.recordDelivery(ctx, wh.ID, "", 0, "failed", ErrWebhookInactive, time.Since(start))
		return nil, ErrWebhookInactive
	}

	if !VerifySignature([]byte(wh.Secret), body, signature) {
		m.recordDelivery(ctx, wh.ID, "", 0, "failed", ErrWebhookInvalidSig, time.Since(start))
		return nil, ErrWebhookInvalidSig
	}

	channel, err := ResolveChannel(wh.ChannelTemplate, body)
	if err != nil {
		m.recordDelivery(ctx, wh.ID, "", 0, "failed", err, time.Since(start))
		return nil, ErrWebhookChannelFail
	}

	seqID, timestamp, err := m.hub.Publish(ctx, channel, json.RawMessage(body), nil)
	duration := time.Since(start)
	if err != nil {
		m.recordDelivery(ctx, wh.ID, channel, 0, "failed", err, duration)
		return nil, err
	}

	m.recordDelivery(ctx, wh.ID, channel, seqID, "success", nil, duration)
	return &DeliveryResult{
		SeqID:     seqID,
		Channel:   channel,
		Timestamp: timestamp,
	}, nil
}

func (m *manager) recordDelivery(ctx context.Context, webhookID, channel string, seqID int64, status string, err error, duration time.Duration) {
	d := &store.WebhookDelivery{
		WebhookID:  webhookID,
		Status:     status,
		DurationMs: int(duration.Milliseconds()),
	}
	if channel != "" {
		d.Channel = &channel
	}
	if seqID != 0 {
		d.SeqID = &seqID
	}
	if err != nil {
		msg := err.Error()
		d.ErrorMessage = &msg
	}

	if derr := m.whStore.CreateDelivery(ctx, d); derr != nil {
		m.logger.Warn("failed to record webhook delivery", "err", derr)
	}
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func newURLToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

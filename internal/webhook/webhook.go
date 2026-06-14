package webhook

import (
	"errors"
	"time"
)

var (
	ErrWebhookNotFound    = errors.New("webhook not found")
	ErrWebhookInactive    = errors.New("webhook is inactive")
	ErrWebhookInvalidSig  = errors.New("invalid webhook signature")
	ErrWebhookChannelFail = errors.New("failed to resolve channel from template")
)

// WebhookMeta is the public metadata for a webhook (no secret).
type WebhookMeta struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	ChannelTemplate string    `json:"channel_template"`
	KeyID           string    `json:"key_id"`
	Active          bool      `json:"active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// CreatedWebhook is returned when a webhook is created or its secret is rotated.
type CreatedWebhook struct {
	Webhook WebhookMeta `json:"webhook"`
	Secret  string      `json:"secret"`
}

// DeliveryResult is returned by ReceiveWebhook.
type DeliveryResult struct {
	SeqID     int64     `json:"seq_id"`
	Channel   string    `json:"channel"`
	Timestamp time.Time `json:"timestamp"`
}

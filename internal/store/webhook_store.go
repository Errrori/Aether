package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrWebhookNotFound     = errors.New("webhook not found")
	ErrWebhookNameConflict = errors.New("webhook name already exists")
)

// Webhook represents a row in the webhooks table.
type Webhook struct {
	ID              string
	Name            string
	URLToken        string
	ChannelTemplate string
	Secret      string
	KeyID           string
	Active          bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// WebhookDelivery represents a single delivery attempt.
type WebhookDelivery struct {
	ID             int64
	WebhookID      string
	Status         string // "success" | "failed"
	ResponseCode   *int
	ErrorMessage   *string
	DurationMs     int
	SeqID          *int64
	Channel        *string
	IdempotencyKey *string
	CreatedAt      time.Time
}

// WebhookStore defines persistence operations for webhooks.
type WebhookStore interface {
	CreateWebhook(ctx context.Context, wh *Webhook) error
	GetWebhook(ctx context.Context, id string) (*Webhook, error)
	GetWebhookByURLToken(ctx context.Context, urlToken string) (*Webhook, error)
	ListWebhooks(ctx context.Context) ([]Webhook, error)
	DeleteWebhook(ctx context.Context, id string) error
	UpdateWebhookSecret(ctx context.Context, id, hash string) error
	CreateDelivery(ctx context.Context, d *WebhookDelivery) error
	ListDeliveries(ctx context.Context, webhookID string, limit int) ([]WebhookDelivery, error)
}

// CreateWebhook inserts a new webhook row.
func (s *pgStore) CreateWebhook(ctx context.Context, wh *Webhook) error {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO webhooks (id, name, url_token, channel_template, secret, key_id, active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (name) DO NOTHING
		 RETURNING created_at, updated_at`,
		wh.ID, wh.Name, wh.URLToken, wh.ChannelTemplate, wh.Secret, wh.KeyID, wh.Active,
	).Scan(&wh.CreatedAt, &wh.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("create webhook: %w", ErrWebhookNameConflict)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("create webhook: %w", ErrWebhookNameConflict)
		}
		return fmt.Errorf("create webhook: %w", err)
	}
	return nil
}

// GetWebhook retrieves a webhook by its ID.
func (s *pgStore) GetWebhook(ctx context.Context, id string) (*Webhook, error) {
	wh, err := s.scanWebhook(ctx,
		`SELECT id, name, url_token, channel_template, secret, key_id, active, created_at, updated_at
		 FROM webhooks WHERE id = $1`, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get webhook: %w", ErrWebhookNotFound)
		}
		return nil, fmt.Errorf("get webhook: %w", err)
	}
	return wh, nil
}

// GetWebhookByURLToken retrieves a webhook by its URL token.
func (s *pgStore) GetWebhookByURLToken(ctx context.Context, urlToken string) (*Webhook, error) {
	wh, err := s.scanWebhook(ctx,
		`SELECT id, name, url_token, channel_template, secret, key_id, active, created_at, updated_at
		 FROM webhooks WHERE url_token = $1`, urlToken)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get webhook by url token: %w", ErrWebhookNotFound)
		}
		return nil, fmt.Errorf("get webhook by url token: %w", err)
	}
	return wh, nil
}

// ListWebhooks returns all webhooks ordered by creation time descending.
func (s *pgStore) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, url_token, channel_template, secret, key_id, active, created_at, updated_at
		 FROM webhooks ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer rows.Close()

	var whs []Webhook
	for rows.Next() {
		var wh Webhook
		if err := s.scanWebhookRow(rows, &wh); err != nil {
			return nil, fmt.Errorf("list webhooks: %w", err)
		}
		whs = append(whs, wh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	if whs == nil {
		whs = []Webhook{}
	}
	return whs, nil
}

// DeleteWebhook removes a webhook by its ID.
func (s *pgStore) DeleteWebhook(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhooks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete webhook: %w", ErrWebhookNotFound)
	}
	return nil
}

// UpdateWebhookSecret updates the secret and updated_at for a webhook.
func (s *pgStore) UpdateWebhookSecret(ctx context.Context, id, hash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhooks SET secret = $2, updated_at = now() WHERE id = $1`,
		id, hash)
	if err != nil {
		return fmt.Errorf("update webhook secret: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update webhook secret: %w", ErrWebhookNotFound)
	}
	return nil
}

// CreateDelivery records a webhook delivery attempt.
func (s *pgStore) CreateDelivery(ctx context.Context, d *WebhookDelivery) error {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO webhook_deliveries (webhook_id, status, response_code, error_message, duration_ms, seq_id, channel, idempotency_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at`,
		d.WebhookID, d.Status, d.ResponseCode, d.ErrorMessage, d.DurationMs,
		d.SeqID, d.Channel, d.IdempotencyKey,
	).Scan(&d.ID, &d.CreatedAt)
	if err != nil {
		return fmt.Errorf("create delivery: %w", err)
	}
	return nil
}

// ListDeliveries returns recent deliveries for a webhook.
func (s *pgStore) ListDeliveries(ctx context.Context, webhookID string, limit int) ([]WebhookDelivery, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, webhook_id, status, response_code, error_message, duration_ms, seq_id, channel, idempotency_key, created_at
		 FROM webhook_deliveries WHERE webhook_id = $1
		 ORDER BY created_at DESC LIMIT $2`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()

	var ds []WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := s.scanDeliveryRow(rows, &d); err != nil {
			return nil, fmt.Errorf("list deliveries: %w", err)
		}
		ds = append(ds, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	if ds == nil {
		ds = []WebhookDelivery{}
	}
	return ds, nil
}

func (s *pgStore) scanWebhook(ctx context.Context, query string, args ...any) (*Webhook, error) {
	var wh Webhook
	if err := s.scanWebhookRow(s.pool.QueryRow(ctx, query, args...), &wh); err != nil {
		return nil, err
	}
	return &wh, nil
}

func (s *pgStore) scanWebhookRow(row dbScanner, wh *Webhook) error {
	return row.Scan(&wh.ID, &wh.Name, &wh.URLToken, &wh.ChannelTemplate,
		&wh.Secret, &wh.KeyID, &wh.Active, &wh.CreatedAt, &wh.UpdatedAt)
}

func (s *pgStore) scanDeliveryRow(row dbScanner, d *WebhookDelivery) error {
	return row.Scan(&d.ID, &d.WebhookID, &d.Status, &d.ResponseCode, &d.ErrorMessage,
		&d.DurationMs, &d.SeqID, &d.Channel, &d.IdempotencyKey, &d.CreatedAt)
}

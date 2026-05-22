package keymgmt

import (
	"context"
	"time"

	"github.com/aether-mq/aether/internal/store"
)

// KeyMeta is the public metadata for an API key (no hash, no plaintext).
type KeyMeta struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	KeyPrefix   string               `json:"key_prefix"`
	Permissions store.KeyPermissions `json:"permissions"`
	CreatedAt   time.Time            `json:"created_at"`
	ExpiresAt   *time.Time           `json:"expires_at,omitempty"`
	RevokedAt   *time.Time           `json:"revoked_at,omitempty"`
}

// CreatedKey is returned when a new key is created or rotated.
// The Key field contains the full plaintext key — shown only once.
type CreatedKey struct {
	Key  string  `json:"key"`
	Meta KeyMeta `json:"meta"`
}

// KeyManager manages the lifecycle of API keys.
type KeyManager interface {
	CreateKey(ctx context.Context, name string, perms store.KeyPermissions, expiresIn *time.Duration) (*CreatedKey, error)
	ListKeys(ctx context.Context) ([]KeyMeta, error)
	GetKey(ctx context.Context, id string) (*KeyMeta, error)
	RotateKey(ctx context.Context, id string) (*CreatedKey, error)
	RevokeKey(ctx context.Context, id string) error
}

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrAPIKeyNotFound      = errors.New("api key not found")
	ErrAPIKeyDuplicateName = errors.New("api key name already exists")
)

// CreateAPIKey inserts a new API key row. The CreatedAt field is populated from the server.
func (s *pgStore) CreateAPIKey(ctx context.Context, key *APIKey) error {
	permBytes, err := json.Marshal(key.Permissions)
	if err != nil {
		return fmt.Errorf("create api key: marshal permissions: %w", err)
	}

	err = s.pool.QueryRow(ctx,
		`INSERT INTO api_keys (id, name, key_hash, key_prefix, permissions, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING created_at`,
		key.ID, key.Name, key.KeyHash, key.KeyPrefix, permBytes, key.ExpiresAt,
	).Scan(&key.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("create api key: %w", ErrAPIKeyDuplicateName)
		}
		return fmt.Errorf("create api key: %w", err)
	}

	return nil
}

// GetAPIKey retrieves a single API key by its ID.
func (s *pgStore) GetAPIKey(ctx context.Context, id string) (*APIKey, error) {
	key, err := s.scanAPIKey(ctx,
		`SELECT id, name, key_hash, key_prefix, permissions, created_at, expires_at, revoked_at
		 FROM api_keys WHERE id = $1`, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get api key: %w", ErrAPIKeyNotFound)
		}
		return nil, fmt.Errorf("get api key: %w", err)
	}
	return key, nil
}

// ListAPIKeys returns all API keys ordered by creation time descending.
func (s *pgStore) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, key_hash, key_prefix, permissions, created_at, expires_at, revoked_at
		 FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := s.scanAPIKeyRow(rows, &k); err != nil {
			return nil, fmt.Errorf("list api keys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	if keys == nil {
		keys = []APIKey{}
	}
	return keys, nil
}

// GetAPIKeyByHash retrieves an API key by its SHA-256 hash.
func (s *pgStore) GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	key, err := s.scanAPIKey(ctx,
		`SELECT id, name, key_hash, key_prefix, permissions, created_at, expires_at, revoked_at
		 FROM api_keys WHERE key_hash = $1`, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get api key by hash: %w", ErrAPIKeyNotFound)
		}
		return nil, fmt.Errorf("get api key by hash: %w", err)
	}
	return key, nil
}

// RevokeAPIKey sets the revoked_at timestamp for the given key.
func (s *pgStore) RevokeAPIKey(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("revoke api key: %w", ErrAPIKeyNotFound)
	}
	return nil
}

// RotateAPIKey updates the key_hash and key_prefix for the given key.
func (s *pgStore) RotateAPIKey(ctx context.Context, id string, newHash, newPrefix string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET key_hash = $2, key_prefix = $3 WHERE id = $1`,
		id, newHash, newPrefix)
	if err != nil {
		return fmt.Errorf("rotate api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("rotate api key: %w", ErrAPIKeyNotFound)
	}
	return nil
}

// scanAPIKey executes a QueryRow and scans the result into an APIKey.
func (s *pgStore) scanAPIKey(ctx context.Context, query string, args ...any) (*APIKey, error) {
	var k APIKey
	if err := s.scanAPIKeyRow(s.pool.QueryRow(ctx, query, args...), &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// dbScanner abstracts the Scan method shared by pgx.Row and pgx.Rows.
type dbScanner interface {
	Scan(dest ...any) error
}

// scanAPIKeyRow scans a row into an APIKey struct.
func (s *pgStore) scanAPIKeyRow(row dbScanner, k *APIKey) error {
	var permBytes []byte
	err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &permBytes,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt)
	if err != nil {
		return err
	}

	if len(permBytes) > 0 {
		if err := json.Unmarshal(permBytes, &k.Permissions); err != nil {
			return fmt.Errorf("unmarshal permissions: %w", err)
		}
	} else {
		k.Permissions = KeyPermissions{}
	}
	return nil
}

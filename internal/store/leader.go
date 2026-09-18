package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// evictionLockKey is the fixed advisory-lock key (derived from "Aether")
// electing the single node that runs the eviction loop per database.
const evictionLockKey int64 = 0x416574686572

// LeaderStore is the optional interface for electing a single eviction leader
// across nodes. It follows the same optional-interface pattern as KeyStore and
// WebhookStore: main type-asserts the store to decide whether to wire it.
type LeaderStore interface {
	// TryEvictionLock acquires the session-level advisory lock on a dedicated
	// connection. It returns (nil, nil) when another node currently holds the
	// lock, and (nil, err) on connection or query failure.
	TryEvictionLock(ctx context.Context) (*EvictionLock, error)
}

// EvictionLock is a held advisory lock bound to a dedicated connection.
// Session locks are released by unlocking explicitly or by closing the
// connection, so a crashed leader never deadlocks the other nodes.
type EvictionLock struct {
	conn *pgx.Conn
	once sync.Once
	// relErr caches the first release error so a repeated Release reports the
	// same outcome instead of falsely reporting success.
	relErr error
}

// TryEvictionLock dials a dedicated connection outside the pool: pooled
// connections may be recycled by the pool at any time, which would silently
// release a session-level lock mid-cycle.
func (s *pgStore) TryEvictionLock(ctx context.Context) (*EvictionLock, error) {
	connCfg := s.connCfg.Copy()
	if connCfg.RuntimeParams == nil {
		connCfg.RuntimeParams = make(map[string]string)
	}
	connCfg.RuntimeParams["application_name"] = "aether-eviction-lock"

	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		return nil, fmt.Errorf("connect for eviction lock: %w", err)
	}

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, evictionLockKey).Scan(&acquired); err != nil {
		if closeErr := conn.Close(context.Background()); closeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("try advisory lock: %w", err),
				fmt.Errorf("close lock connection: %w", closeErr),
			)
		}
		return nil, fmt.Errorf("try advisory lock: %w", err)
	}
	if !acquired {
		if err := conn.Close(context.Background()); err != nil {
			return nil, fmt.Errorf("close contended lock connection: %w", err)
		}
		return nil, nil
	}

	return &EvictionLock{conn: conn}, nil
}

// Release unlocks the advisory lock and closes the connection. Closing the
// connection releases the session lock even if the explicit unlock fails.
func (l *EvictionLock) Release(ctx context.Context) error {
	l.once.Do(func() {
		// Shutdown paths may pass an already-cancelled context, but the lock
		// still has to be released before giving up.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		var unlocked bool
		if err := l.conn.QueryRow(releaseCtx, `SELECT pg_advisory_unlock($1)`, evictionLockKey).Scan(&unlocked); err != nil {
			l.relErr = fmt.Errorf("advisory unlock: %w", err)
		} else if !unlocked {
			l.relErr = fmt.Errorf("advisory unlock: lock was not held")
		}
		if err := l.conn.Close(releaseCtx); err != nil && l.relErr == nil {
			l.relErr = fmt.Errorf("close eviction lock connection: %w", err)
		}
	})
	return l.relErr
}

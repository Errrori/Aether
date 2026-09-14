package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTruncateAll_InvalidDSN(t *testing.T) {
	err := TruncateAll(context.Background(), "postgres://user:pass@host:notaport/db")
	var parseErr *pgconn.ParseConfigError
	if !errors.As(err, &parseErr) {
		t.Fatalf("expected ParseConfigError from connect, got %v", err)
	}
}

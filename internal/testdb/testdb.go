// Package testdb gives each test its own freshly migrated Postgres schema, so
// integration tests can run in parallel against one database without
// stepping on each other.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"

	"github.com/vaibenz/custody-ledger/internal/db"
)

// New returns a pool bound to a new schema, or skips the test when
// DATABASE_URL is not set.
func New(t testing.TB) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := db.Open(ctx, dsn, 1)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := fmt.Sprintf("test_%d", rand.Uint64())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	pool, err := db.Open(ctx, u.String(), 32)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return pool
}

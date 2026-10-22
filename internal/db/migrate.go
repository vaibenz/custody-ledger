// Package db holds the schema and a small forward only migration runner.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	_ "github.com/lib/pq" // Postgres driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, dsn string, maxConns int) (*sql.DB, error) {
	pool, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(maxConns)
	pool.SetMaxIdleConns(maxConns)
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Migrate applies every migration that has not been applied yet, each in its
// own transaction. An advisory lock stops two instances migrating at once.
func Migrate(ctx context.Context, pool *sql.DB) error {
	conn, err := pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	const lockID = 727274
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	for _, name := range names {
		var done bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name,
		).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

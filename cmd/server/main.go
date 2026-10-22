// Command server runs the custody ledger HTTP API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/vaibenz/custody-ledger/internal/api"
	"github.com/vaibenz/custody-ledger/internal/db"
	"github.com/vaibenz/custody-ledger/internal/ledger"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	addr := ":" + envOr("PORT", "8080")
	maxConns, _ := strconv.Atoi(envOr("DB_MAX_CONNS", "20"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connectWithRetry(ctx, log, dsn, maxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		log.Warn("ADMIN_TOKEN not set; review endpoints are disabled")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(ledger.NewStore(pool), adminToken, log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// connectWithRetry waits for the database, which may start after this
// container under docker compose or on a fresh deploy.
func connectWithRetry(ctx context.Context, log *slog.Logger, dsn string, maxConns int) (*sql.DB, error) {
	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		pool, err := db.Open(ctx, dsn, maxConns)
		if err == nil {
			return pool, nil
		}
		if attempt == 10 {
			return nil, err
		}
		log.Warn("database not ready", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Command relay delivers outbox events. Here the handler only logs each
// withdrawal to send; in a real custodian it would call the signing service,
// passing the event key so the signer can ignore a repeated delivery.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vaibenz/custody-ledger/internal/db"
	"github.com/vaibenz/custody-ledger/internal/outbox"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, dsn, 4)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	r := &outbox.Relay{
		DB:  pool,
		Log: log,
		Handler: func(_ context.Context, e outbox.Event) error {
			log.Info("send withdrawal", "topic", e.Topic, "key", e.Key, "payload", e.Payload, "attempt", e.Attempts+1)
			return nil
		},
	}
	log.Info("relay started")
	return r.Run(ctx, time.Second)
}

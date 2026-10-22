// Package outbox implements the transactional outbox pattern.
//
// Producers call Enqueue inside the database transaction that makes the
// change, so the event commits or rolls back with it. A Relay polls for due
// events and hands them to a handler (the signer, a queue, a webhook).
//
// Delivery is at least once: if the relay crashes after the handler succeeds
// but before the event is marked published, the event is delivered again.
// Every event carries a stable Key, which consumers use to ignore repeats.
// Several relays can run at once; FOR UPDATE SKIP LOCKED gives each event to
// exactly one of them per pass.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type Event struct {
	ID        int64           `json:"id"`
	Topic     string          `json:"topic"`
	Key       string          `json:"key"`
	Payload   json.RawMessage `json:"payload"`
	Attempts  int             `json:"attempts"`
	CreatedAt time.Time       `json:"created_at"`
}

// Enqueue adds an event inside the caller's transaction. Enqueueing the same
// topic and key twice is a no op.
func Enqueue(ctx context.Context, q Execer, topic, key string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO outbox (topic, key, payload) VALUES ($1, $2, $3)
		ON CONFLICT (topic, key) DO NOTHING`, topic, key, data)
	return err
}

type Handler func(ctx context.Context, e Event) error

type Relay struct {
	DB      *sql.DB
	Handler Handler
	// Batch is how many events one pass claims. Default 50.
	Batch int
	Log   *slog.Logger
}

// Drain makes one pass over due events and returns how many were delivered.
// A failed event is retried later with exponential backoff, capped at five
// minutes, and does not block the events behind it.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	batch := r.Batch
	if batch <= 0 {
		batch = 50
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, topic, key, payload, attempts, created_at
		  FROM outbox
		 WHERE published_at IS NULL AND next_attempt_at <= now()
		 ORDER BY id
		 LIMIT $1
		   FOR UPDATE SKIP LOCKED`, batch)
	if err != nil {
		return 0, err
	}
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Topic, &e.Key, &e.Payload, &e.Attempts, &e.CreatedAt); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	delivered := 0
	for _, e := range events {
		if herr := r.Handler(ctx, e); herr != nil {
			if r.Log != nil {
				r.Log.Warn("outbox delivery failed", "id", e.ID, "topic", e.Topic, "attempt", e.Attempts+1, "err", herr)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE outbox
				   SET attempts = attempts + 1,
				       last_error = $2,
				       next_attempt_at = now() + least(interval '1 second' * power(2, attempts), interval '5 minutes')
				 WHERE id = $1`, e.ID, herr.Error()); err != nil {
				return delivered, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbox SET published_at = now(), attempts = attempts + 1 WHERE id = $1`, e.ID); err != nil {
			return delivered, err
		}
		delivered++
	}
	return delivered, tx.Commit()
}

// Run drains continuously until ctx is cancelled, sleeping for interval
// whenever there is nothing due.
func (r *Relay) Run(ctx context.Context, interval time.Duration) error {
	for {
		n, err := r.Drain(ctx)
		if err != nil && ctx.Err() == nil && r.Log != nil {
			r.Log.Error("outbox drain", "err", err)
		}
		if n > 0 && err == nil {
			continue // more may be waiting
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vaibenz/custody-ledger/internal/outbox"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

func TestDeliverOnceAndMarkPublished(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := outbox.Enqueue(ctx, pool, "t", fmt.Sprint(i), map[string]int{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	// Same topic and key again is ignored.
	outbox.Enqueue(ctx, pool, "t", "0", map[string]int{"n": 99})

	var got []string
	r := outbox.Relay{DB: pool, Handler: func(_ context.Context, e outbox.Event) error {
		got = append(got, e.Key+":"+string(e.Payload))
		return nil
	}}
	n, err := r.Drain(ctx)
	if err != nil || n != 3 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if fmt.Sprint(got) != `[0:{"n": 0} 1:{"n": 1} 2:{"n": 2}]` {
		t.Errorf("delivered %v", got)
	}
	if n, _ := r.Drain(ctx); n != 0 {
		t.Errorf("second drain delivered %d", n)
	}
}

func TestFailedDeliveryBacksOffWithoutBlocking(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	outbox.Enqueue(ctx, pool, "t", "bad", nil)
	outbox.Enqueue(ctx, pool, "t", "good", nil)

	var delivered []string
	r := outbox.Relay{DB: pool, Handler: func(_ context.Context, e outbox.Event) error {
		if e.Key == "bad" {
			return errors.New("signer unavailable")
		}
		delivered = append(delivered, e.Key)
		return nil
	}}
	if n, err := r.Drain(ctx); err != nil || n != 1 || fmt.Sprint(delivered) != "[good]" {
		t.Fatalf("drain = %d, %v, delivered %v", n, err, delivered)
	}

	var attempts int
	var lastErr string
	pool.QueryRowContext(ctx, `SELECT attempts, last_error FROM outbox WHERE key = 'bad'`).Scan(&attempts, &lastErr)
	if attempts != 1 || lastErr != "signer unavailable" {
		t.Errorf("attempts %d, last error %q", attempts, lastErr)
	}
	// Backed off, so not due yet.
	if n, _ := r.Drain(ctx); n != 0 {
		t.Errorf("retried before its backoff: %d", n)
	}

	// Once due and the signer is back, it goes through.
	pool.ExecContext(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE key = 'bad'`)
	r.Handler = func(context.Context, outbox.Event) error { return nil }
	if n, _ := r.Drain(ctx); n != 1 {
		t.Errorf("retry delivered %d", n)
	}
}

// Several relays draining one queue must never hand the same event to two
// of them in a pass.
func TestConcurrentRelaysDeliverEachEventOnce(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	const events = 300
	for i := 0; i < events; i++ {
		outbox.Enqueue(ctx, pool, "t", fmt.Sprint(i), nil)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	handler := func(_ context.Context, e outbox.Event) error {
		mu.Lock()
		seen[e.Key]++
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := outbox.Relay{DB: pool, Handler: handler, Batch: 20}
			for {
				n, err := r.Drain(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != events {
		t.Errorf("delivered %d distinct events, want %d", len(seen), events)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("event %s delivered %d times", k, n)
		}
	}
}

func TestRunDeliversUntilCancelled(t *testing.T) {
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	outbox.Enqueue(ctx, pool, "t", "a", nil)

	got := make(chan string, 1)
	r := outbox.Relay{DB: pool, Handler: func(_ context.Context, e outbox.Event) error {
		got <- e.Key
		return nil
	}}
	done := make(chan error)
	go func() { done <- r.Run(ctx, 10*time.Millisecond) }()
	if k := <-got; k != "a" {
		t.Errorf("delivered %q", k)
	}
	// Enqueued while running: picked up on a later poll.
	outbox.Enqueue(context.Background(), pool, "t", "b", nil)
	if k := <-got; k != "b" {
		t.Errorf("delivered %q", k)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v", err)
	}
}

package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/ledger"
)

func TestTransferMovesFunds(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustAccount(t, s, "alice", "BTC")
	b := mustAccount(t, s, "bob", "BTC")
	mustDeposit(t, s, a.ID, 1_000)

	if _, err := s.Transfer(ctx, a.ID, b.ID, 400, "t-1"); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, s, a.ID); got != 600 {
		t.Errorf("alice = %d, want 600", got)
	}
	if got := balance(t, s, b.ID); got != 400 {
		t.Errorf("bob = %d, want 400", got)
	}
	assertInvariants(t, pool)
}

func TestTransferRejections(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustAccount(t, s, "alice", "BTC")
	b := mustAccount(t, s, "bob", "BTC")
	e := mustAccount(t, s, "erin", "ETH")
	mustDeposit(t, s, a.ID, 100)

	cases := []struct {
		name     string
		from, to uuid.UUID
		amount   int64
		key      string
		want     error
	}{
		{"overdraw", a.ID, b.ID, 101, "k1", ledger.ErrInsufficientFunds},
		{"zero", a.ID, b.ID, 0, "k2", ledger.ErrInvalidAmount},
		{"negative", a.ID, b.ID, -5, "k3", ledger.ErrInvalidAmount},
		{"self", a.ID, a.ID, 10, "k4", ledger.ErrSameAccount},
		{"cross asset", a.ID, e.ID, 10, "k5", ledger.ErrAssetMismatch},
		{"unknown", a.ID, uuid.New(), 10, "k6", ledger.ErrAccountNotFound},
		{"no key", a.ID, b.ID, 10, "", ledger.ErrMissingIdempotencyKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Transfer(ctx, tc.from, tc.to, tc.amount, tc.key)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
	if got := balance(t, s, a.ID); got != 100 {
		t.Errorf("failed transfers changed balance to %d", got)
	}
	// A failed transfer rolls back its transaction row, so the key is free.
	if _, err := s.Transfer(ctx, a.ID, b.ID, 50, "k1"); err != nil {
		t.Errorf("key from a failed attempt should be reusable: %v", err)
	}
	assertInvariants(t, pool)
}

func TestTransferIsIdempotent(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustAccount(t, s, "alice", "BTC")
	b := mustAccount(t, s, "bob", "BTC")
	mustDeposit(t, s, a.ID, 1_000)

	first, err := s.Transfer(ctx, a.ID, b.ID, 300, "pay-42")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Transfer(ctx, a.ID, b.ID, 300, "pay-42")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.ID != first.ID {
		t.Errorf("retry should replay %s, got %+v", first.ID, again)
	}
	if got := balance(t, s, a.ID); got != 700 {
		t.Errorf("retry moved funds again: alice = %d", got)
	}

	_, err = s.Transfer(ctx, a.ID, b.ID, 999, "pay-42")
	if !errors.Is(err, ledger.ErrIdempotencyKeyConflict) {
		t.Errorf("same key, different amount: got %v", err)
	}
	assertInvariants(t, pool)
}

// Many clients retry the same request at the same moment, as happens when a
// mobile app times out and resends. Exactly one must apply.
func TestConcurrentRetriesApplyOnce(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustAccount(t, s, "alice", "BTC")
	b := mustAccount(t, s, "bob", "BTC")
	mustDeposit(t, s, a.ID, 1_000)

	const n = 50
	var applied atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			txn, err := s.Transfer(ctx, a.ID, b.ID, 100, "same-key")
			if err != nil {
				t.Errorf("transfer: %v", err)
				return
			}
			if !txn.Replayed {
				applied.Add(1)
			}
		}()
	}
	wg.Wait()

	if applied.Load() != 1 {
		t.Errorf("applied %d times, want 1", applied.Load())
	}
	if got := balance(t, s, b.ID); got != 100 {
		t.Errorf("bob = %d, want 100", got)
	}
	assertInvariants(t, pool)
}

// Concurrent spends that together exceed the balance: the row lock must stop
// the account going negative, so exactly balance/amount of them succeed.
func TestConcurrentSpendsCannotDoubleSpend(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustAccount(t, s, "alice", "BTC")
	b := mustAccount(t, s, "bob", "BTC")
	mustDeposit(t, s, a.ID, 1_000)

	const n = 40 // 40 x 100 against a balance of 1000
	var ok, insufficient atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Transfer(ctx, a.ID, b.ID, 100, fmt.Sprintf("spend-%d", i))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrInsufficientFunds):
				insufficient.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if ok.Load() != 10 || insufficient.Load() != 30 {
		t.Errorf("succeeded %d, insufficient %d; want 10 and 30", ok.Load(), insufficient.Load())
	}
	if got := balance(t, s, a.ID); got != 0 {
		t.Errorf("alice = %d, want 0", got)
	}
	assertInvariants(t, pool)
}

// Random transfers in both directions across a small set of accounts, so
// the same pairs are contended from opposite sides. Lock ordering means no
// deadlocks, and total value is conserved.
func TestConcurrentRandomTransfersConserveValue(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	const accounts, perAccount, transfers, workers = 10, 10_000, 1_000, 16
	ids := make([]uuid.UUID, accounts)
	for i := range ids {
		ids[i] = mustAccount(t, s, fmt.Sprintf("user-%d", i), "BTC").ID
		mustDeposit(t, s, ids[i], perAccount)
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				from := ids[rand.IntN(accounts)]
				to := ids[rand.IntN(accounts)]
				if from == to {
					continue
				}
				_, err := s.Transfer(ctx, from, to, int64(rand.IntN(500)+1), fmt.Sprintf("r-%d", j))
				if err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
					t.Errorf("transfer %d: %v", j, err)
				}
			}
		}()
	}
	for j := 0; j < transfers; j++ {
		jobs <- j
	}
	close(jobs)
	wg.Wait()

	var total int64
	for _, id := range ids {
		total += balance(t, s, id)
	}
	if total != accounts*perAccount {
		t.Errorf("total = %d, want %d", total, accounts*perAccount)
	}
	assertInvariants(t, pool)
}

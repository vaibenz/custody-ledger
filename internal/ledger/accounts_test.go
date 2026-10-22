package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/ledger"
)

func TestCreateAccountValidates(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	if _, err := s.CreateAccount(ctx, "", "BTC"); !errors.Is(err, ledger.ErrInvalidOwner) {
		t.Errorf("empty owner: got %v", err)
	}
	for _, asset := range []string{"btc", "B", "BTC1", "TOOLONGASSET"} {
		if _, err := s.CreateAccount(ctx, "alice", asset); !errors.Is(err, ledger.ErrInvalidAsset) {
			t.Errorf("asset %q: got %v", asset, err)
		}
	}
}

func TestDepositCreditsAccount(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustAccount(t, s, "alice", "BTC")

	txn, err := s.Deposit(ctx, a.ID, 50_000, "dep-1")
	if err != nil {
		t.Fatal(err)
	}
	if txn.Replayed {
		t.Error("first deposit reported as replayed")
	}
	if got := balance(t, s, a.ID); got != 50_000 {
		t.Errorf("balance = %d, want 50000", got)
	}

	entries, err := s.Entries(ctx, a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Amount != 50_000 || entries[0].TransactionID != txn.ID {
		t.Errorf("unexpected entries %+v", entries)
	}
	assertInvariants(t, pool)
}

func TestDepositRejectsBadInput(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a := mustAccount(t, s, "alice", "BTC")

	if _, err := s.Deposit(ctx, a.ID, 0, "k"); !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Errorf("zero amount: got %v", err)
	}
	if _, err := s.Deposit(ctx, a.ID, 10, ""); !errors.Is(err, ledger.ErrMissingIdempotencyKey) {
		t.Errorf("missing key: got %v", err)
	}
	if _, err := s.Deposit(ctx, uuid.New(), 10, "k"); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("unknown account: got %v", err)
	}
}

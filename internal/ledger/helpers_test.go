package ledger_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/ledger"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

func newStore(t *testing.T) (*ledger.Store, *sql.DB) {
	t.Helper()
	pool := testdb.New(t)
	return ledger.NewStore(pool), pool
}

func mustAccount(t *testing.T, s *ledger.Store, owner, asset string) ledger.Account {
	t.Helper()
	a, err := s.CreateAccount(context.Background(), owner, asset)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a
}

func mustDeposit(t *testing.T, s *ledger.Store, id uuid.UUID, amount int64) {
	t.Helper()
	if _, err := s.Deposit(context.Background(), id, amount, uuid.NewString()); err != nil {
		t.Fatalf("deposit: %v", err)
	}
}

func balance(t *testing.T, s *ledger.Store, id uuid.UUID) int64 {
	t.Helper()
	a, err := s.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return a.Balance
}

// assertInvariants checks the properties that must hold after any sequence
// of operations: every transaction sums to zero, cached balances equal the
// sum of their entries, and only external accounts are negative.
func assertInvariants(t *testing.T, pool *sql.DB) {
	t.Helper()
	ctx := context.Background()

	var unbalanced int
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT transaction_id FROM entries GROUP BY transaction_id HAVING sum(amount) <> 0
		) x`).Scan(&unbalanced); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 {
		t.Errorf("%d transactions do not sum to zero", unbalanced)
	}

	var drift int
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM accounts a
		 WHERE a.balance <> COALESCE((SELECT sum(amount) FROM entries e WHERE e.account_id = a.id), 0)`,
	).Scan(&drift); err != nil {
		t.Fatal(err)
	}
	if drift != 0 {
		t.Errorf("%d accounts have a cached balance that differs from their entries", drift)
	}

	var negative int
	if err := pool.QueryRowContext(ctx,
		`SELECT count(*) FROM accounts WHERE kind <> 'external' AND balance < 0`).Scan(&negative); err != nil {
		t.Fatal(err)
	}
	if negative != 0 {
		t.Errorf("%d non external accounts are negative", negative)
	}
}

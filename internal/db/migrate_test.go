package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/vaibenz/custody-ledger/internal/db"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestUnbalancedTransactionIsRejectedAtCommit(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO accounts (id, owner, asset) VALUES ('00000000-0000-0000-0000-000000000001', 'a', 'BTC')`)
	mustExec(`INSERT INTO transactions (id, kind, idempotency_key, request_hash)
	          VALUES ('00000000-0000-0000-0000-0000000000aa', 'test', 'k', 'h')`)
	// A single leg of +5 with nothing to balance it.
	mustExec(`INSERT INTO entries (transaction_id, account_id, amount)
	          VALUES ('00000000-0000-0000-0000-0000000000aa', '00000000-0000-0000-0000-000000000001', 5)`)

	err = tx.Commit()
	if err == nil || !strings.Contains(err.Error(), "not balanced") {
		t.Fatalf("expected commit to fail with unbalanced error, got %v", err)
	}
}

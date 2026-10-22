package audit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/vaibenz/custody-ledger/internal/audit"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if err := audit.Write(ctx, pool, "operator:bob", "deposit", "acct-1", map[string]any{"amount": 5}); err != nil {
		t.Fatal(err)
	}
	if err := audit.Write(ctx, pool, "customer:alice", "transfer", "acct-1", nil); err != nil {
		t.Fatal(err)
	}

	got, err := audit.ForSubject(ctx, pool, "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "deposit" || string(got[0].Detail) != `{"amount": 5}` {
		t.Fatalf("entries = %+v", got)
	}

	for _, q := range []string{
		`UPDATE audit_log SET actor = 'someone-else'`,
		`DELETE FROM audit_log`,
	} {
		_, err := pool.ExecContext(ctx, q)
		if err == nil || !strings.Contains(err.Error(), "append only") {
			t.Errorf("%s: expected refusal, got %v", q, err)
		}
	}
}

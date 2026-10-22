package ledger_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/ledger"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

// BenchmarkTransfer measures end to end transfers (one database transaction
// each, with row locks, entries and balance updates) from parallel clients
// over 100 funded accounts.
//
//	go test ./internal/ledger -run '^$' -bench Transfer -cpu 1,8,16
func BenchmarkTransfer(b *testing.B) {
	s := ledger.NewStore(testdb.New(b))
	ctx := context.Background()

	ids := make([]uuid.UUID, 100)
	for i := range ids {
		a, err := s.CreateAccount(ctx, fmt.Sprintf("u%d", i), "BTC")
		if err != nil {
			b.Fatal(err)
		}
		ids[i] = a.ID
		if _, err := s.Deposit(ctx, a.ID, 1<<40, uuid.NewString()); err != nil {
			b.Fatal(err)
		}
	}

	var seq atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			from := rand.IntN(len(ids))
			to := (from + 1 + rand.IntN(len(ids)-1)) % len(ids)
			key := fmt.Sprintf("b-%d", seq.Add(1))
			if _, err := s.Transfer(ctx, ids[from], ids[to], 1, key); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "transfers/s")
}

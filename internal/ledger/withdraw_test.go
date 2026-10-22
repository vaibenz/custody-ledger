package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/ledger"
	"github.com/vaibenz/custody-ledger/internal/risk"
)

const addr = "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// setup returns a store with a BTC policy, a funded account, and a saved
// address that is already past its hold.
func setup(t *testing.T) (*ledger.Store, *clock, ledger.Account) {
	t.Helper()
	s, _ := newStore(t)
	c := &clock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	s.Now = c.Now
	s.SetPolicy("BTC", risk.Policy{
		DailyLimit:     10_000,
		MaxPerHour:     3,
		ReviewAbove:    5_000,
		NewAddressHold: 24 * time.Hour,
	})
	a := mustAccount(t, s, "alice", "BTC")
	mustDeposit(t, s, a.ID, 50_000)
	if _, err := s.AddAddress(context.Background(), a.ID, addr); err != nil {
		t.Fatal(err)
	}
	c.Add(48 * time.Hour)
	return s, c, a
}

func TestSmallWithdrawalToTrustedAddressIsApproved(t *testing.T) {
	s, _, a := setup(t)
	w, err := s.RequestWithdrawal(context.Background(), a.ID, addr, 1_000, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != ledger.StatusApproved {
		t.Fatalf("status = %s, reasons %v", w.Status, w.Reasons)
	}
	if got := balance(t, s, a.ID); got != 49_000 {
		t.Errorf("balance = %d, want 49000", got)
	}
}

func TestNewAddressIsHeldForReview(t *testing.T) {
	s, _, a := setup(t)
	ctx := context.Background()
	fresh := "bc1qnewaddressaddedjustnow000000000000000"
	if _, err := s.AddAddress(ctx, a.ID, fresh); err != nil {
		t.Fatal(err)
	}

	w, err := s.RequestWithdrawal(ctx, a.ID, fresh, 1_000, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != ledger.StatusPendingReview {
		t.Fatalf("status = %s, want pending_review", w.Status)
	}
	// Held funds leave the customer's spendable balance.
	if got := balance(t, s, a.ID); got != 49_000 {
		t.Errorf("balance = %d, want 49000", got)
	}

	pending, err := s.PendingReviews(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != w.ID {
		t.Errorf("pending = %+v", pending)
	}
}

func TestReviewApproveAndReject(t *testing.T) {
	s, _, a := setup(t)
	ctx := context.Background()

	big, err := s.RequestWithdrawal(ctx, a.ID, addr, 6_000, "big")
	if err != nil || big.Status != ledger.StatusPendingReview {
		t.Fatalf("big: %+v %v", big, err)
	}
	other, err := s.RequestWithdrawal(ctx, a.ID, addr, 7_000, "other")
	if err == nil && other.Status != ledger.StatusRejected {
		// 6000 + 7000 is over the 10000 daily limit.
		t.Fatalf("second big withdrawal should hit the daily limit: %+v", other)
	}

	approved, err := s.ReviewWithdrawal(ctx, big.ID, true, "ops@custody")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != ledger.StatusApproved || approved.ReviewedBy != "ops@custody" {
		t.Errorf("approved = %+v", approved)
	}
	if _, err := s.ReviewWithdrawal(ctx, big.ID, false, "ops@custody"); !errors.Is(err, ledger.ErrNotPendingReview) {
		t.Errorf("second decision: got %v", err)
	}
	if got := balance(t, s, a.ID); got != 44_000 {
		t.Errorf("after approve balance = %d, want 44000", got)
	}

	// Next day, a held withdrawal that is rejected returns the funds.
	s.Now = func() time.Time { return approved.CreatedAt.Add(25 * time.Hour) }
	held, err := s.RequestWithdrawal(ctx, a.ID, addr, 5_500, "held")
	if err != nil || held.Status != ledger.StatusPendingReview {
		t.Fatalf("held: %+v %v", held, err)
	}
	rejected, err := s.ReviewWithdrawal(ctx, held.ID, false, "ops@custody")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != ledger.StatusRejected {
		t.Errorf("status = %s", rejected.Status)
	}
	if got := balance(t, s, a.ID); got != 44_000 {
		t.Errorf("after reject balance = %d, want 44000", got)
	}
}

func TestVelocityLimitRejects(t *testing.T) {
	s, c, a := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		w, err := s.RequestWithdrawal(ctx, a.ID, addr, 100, fmt.Sprintf("v%d", i))
		if err != nil || w.Status != ledger.StatusApproved {
			t.Fatalf("withdrawal %d: %+v %v", i, w, err)
		}
		c.Add(time.Minute)
	}
	w, err := s.RequestWithdrawal(ctx, a.ID, addr, 100, "v3")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != ledger.StatusRejected {
		t.Fatalf("fourth in an hour: status %s", w.Status)
	}
	if got := balance(t, s, a.ID); got != 49_700 {
		t.Errorf("rejected withdrawal moved funds: balance %d", got)
	}

	c.Add(time.Hour)
	if w, _ := s.RequestWithdrawal(ctx, a.ID, addr, 100, "v4"); w.Status != ledger.StatusApproved {
		t.Errorf("after an hour: status %s %v", w.Status, w.Reasons)
	}
}

func TestWithdrawalIdempotency(t *testing.T) {
	s, _, a := setup(t)
	ctx := context.Background()
	first, err := s.RequestWithdrawal(ctx, a.ID, addr, 1_000, "same")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.RequestWithdrawal(ctx, a.ID, addr, 1_000, "same")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.ID != first.ID {
		t.Errorf("retry: %+v", again)
	}
	if got := balance(t, s, a.ID); got != 49_000 {
		t.Errorf("retry moved funds: %d", got)
	}
	if _, err := s.RequestWithdrawal(ctx, a.ID, addr, 2_000, "same"); !errors.Is(err, ledger.ErrIdempotencyKeyConflict) {
		t.Errorf("reused key: got %v", err)
	}
}

// Concurrent withdrawals must not jointly exceed the daily limit, which is
// why the account row is locked before history is read.
func TestConcurrentWithdrawalsRespectDailyLimit(t *testing.T) {
	s, _, a := setup(t)
	s.SetPolicy("BTC", risk.Policy{DailyLimit: 10_000})
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]ledger.Withdrawal, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := s.RequestWithdrawal(ctx, a.ID, addr, 1_000, fmt.Sprintf("c%d", i))
			if err != nil {
				t.Errorf("withdrawal %d: %v", i, err)
			}
			results[i] = w
		}(i)
	}
	wg.Wait()

	approved := 0
	for _, w := range results {
		if w.Status == ledger.StatusApproved {
			approved++
		}
	}
	if approved != 10 {
		t.Errorf("approved %d, want exactly 10 under a 10000 limit", approved)
	}
	if got := balance(t, s, a.ID); got != 40_000 {
		t.Errorf("balance = %d, want 40000", got)
	}
}

func TestWithdrawalValidation(t *testing.T) {
	s, _, a := setup(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		id      uuid.UUID
		address string
		amount  int64
		want    error
	}{
		{"overdraw", a.ID, addr, 60_000, ledger.ErrInsufficientFunds},
		{"bad address", a.ID, "not-an-address!", 10, ledger.ErrInvalidAddress},
		{"zero", a.ID, addr, 0, ledger.ErrInvalidAmount},
		{"unknown account", uuid.New(), addr, 10, ledger.ErrAccountNotFound},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.RequestWithdrawal(ctx, tc.id, tc.address, tc.amount, fmt.Sprintf("bad%d", i))
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func fourEyes(t *testing.T) (*ledger.Store, ledger.Account, ledger.Withdrawal) {
	t.Helper()
	s, _, a := setup(t)
	s.SetPolicy("BTC", risk.Policy{ReviewAbove: 1, ReviewApprovals: 2})
	w, err := s.RequestWithdrawal(context.Background(), a.ID, addr, 1_000, "held")
	if err != nil || w.Status != ledger.StatusPendingReview || w.ApprovalsRequired != 2 {
		t.Fatalf("held: %+v %v", w, err)
	}
	return s, a, w
}

func TestFourEyesNeedsTwoDistinctApprovers(t *testing.T) {
	s, a, w := fourEyes(t)
	ctx := context.Background()

	got, err := s.ReviewWithdrawal(ctx, w.ID, true, "alice-ops")
	if err != nil || got.Status != ledger.StatusPendingReview || len(got.Approvals) != 1 {
		t.Fatalf("first approval: %+v %v", got, err)
	}
	if _, err := s.ReviewWithdrawal(ctx, w.ID, true, "alice-ops"); !errors.Is(err, ledger.ErrAlreadyReviewed) {
		t.Errorf("same reviewer twice: %v", err)
	}
	if got := balance(t, s, a.ID); got != 49_000 {
		t.Errorf("funds should stay held: %d", got)
	}

	got, err = s.ReviewWithdrawal(ctx, w.ID, true, "bob-ops")
	if err != nil || got.Status != ledger.StatusApproved || got.ReviewedBy != "alice-ops,bob-ops" {
		t.Fatalf("second approval: %+v %v", got, err)
	}
	if _, err := s.ReviewWithdrawal(ctx, w.ID, true, "carol-ops"); !errors.Is(err, ledger.ErrNotPendingReview) {
		t.Errorf("approval after settlement: %v", err)
	}
}

func TestOneRejectionReleasesFunds(t *testing.T) {
	s, a, w := fourEyes(t)
	ctx := context.Background()
	if _, err := s.ReviewWithdrawal(ctx, w.ID, true, "alice-ops"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReviewWithdrawal(ctx, w.ID, false, "bob-ops")
	if err != nil || got.Status != ledger.StatusRejected {
		t.Fatalf("reject: %+v %v", got, err)
	}
	if got := balance(t, s, a.ID); got != 50_000 {
		t.Errorf("funds not released: %d", got)
	}
}

// Many reviewers approving at the same moment must settle exactly once, with
// exactly the required number of approvals recorded.
func TestConcurrentApprovalsSettleOnce(t *testing.T) {
	s, a, w := fourEyes(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	var settled, pending, closed atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := s.ReviewWithdrawal(ctx, w.ID, true, fmt.Sprintf("ops-%d", i))
			switch {
			case errors.Is(err, ledger.ErrNotPendingReview):
				closed.Add(1)
			case err != nil:
				t.Errorf("reviewer %d: %v", i, err)
			case got.Status == ledger.StatusApproved:
				settled.Add(1)
			default:
				pending.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if settled.Load() != 1 || pending.Load() != 1 || closed.Load() != 8 {
		t.Errorf("settled %d pending %d closed %d", settled.Load(), pending.Load(), closed.Load())
	}
	final, _ := s.GetWithdrawal(ctx, w.ID)
	if len(final.Approvals) != 2 {
		t.Errorf("approvals recorded: %v", final.Approvals)
	}
	if got := balance(t, s, a.ID); got != 49_000 {
		t.Errorf("balance %d", got)
	}
}

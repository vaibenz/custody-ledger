package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/vaibenz/custody-ledger/internal/audit"
	"github.com/vaibenz/custody-ledger/internal/outbox"
	"github.com/vaibenz/custody-ledger/internal/risk"
)

var (
	ErrInvalidAddress     = errors.New("address must be 20 to 100 letters and digits")
	ErrWithdrawalNotFound = errors.New("withdrawal not found")
	ErrNotPendingReview   = errors.New("withdrawal is not pending review")
	ErrReviewerRequired   = errors.New("reviewer is required")
	ErrAlreadyReviewed    = errors.New("this reviewer has already approved this withdrawal")
)

// Withdrawal statuses.
const (
	StatusApproved      = "approved"
	StatusPendingReview = "pending_review"
	StatusRejected      = "rejected"
)

// A loose format check. Real address validation is per chain (checksums,
// bech32 and so on) and belongs in a chain adapter.
var addressPattern = regexp.MustCompile(`^[A-Za-z0-9]{20,100}$`)

type Address struct {
	AccountID uuid.UUID `json:"account_id"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
}

type Withdrawal struct {
	ID         uuid.UUID `json:"id"`
	AccountID  uuid.UUID `json:"account_id"`
	Address    string    `json:"address"`
	Amount     int64     `json:"amount"`
	Status     string    `json:"status"`
	Reasons    []string  `json:"reasons"`
	ReviewedBy string    `json:"reviewed_by,omitempty"`
	// Approvals lists the operators who have approved so far, oldest first.
	Approvals         []string   `json:"approvals"`
	ApprovalsRequired int        `json:"approvals_required"`
	CreatedAt         time.Time  `json:"created_at"`
	DecidedAt         *time.Time `json:"decided_at,omitempty"`
	Replayed          bool       `json:"replayed"`
}

// SetPolicy sets the risk policy for one asset.
func (s *Store) SetPolicy(asset string, p risk.Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policies == nil {
		s.policies = map[string]risk.Policy{}
	}
	s.policies[asset] = p
}

func (s *Store) policy(asset string) risk.Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.policies[asset]; ok {
		return p
	}
	return risk.DefaultPolicy
}

// AddAddress saves a destination address for an account. Adding an address
// that is already saved keeps its original timestamp, so re-adding cannot
// be used to reset the new address hold.
func (s *Store) AddAddress(ctx context.Context, accountID uuid.UUID, address string) (Address, error) {
	if !addressPattern.MatchString(address) {
		return Address{}, ErrInvalidAddress
	}
	if _, err := s.GetAccount(ctx, accountID); err != nil {
		return Address{}, err
	}
	a := Address{AccountID: accountID, Address: address}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO addresses (account_id, address, created_at) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, accountID, address, s.Now())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if err := audit.Write(ctx, tx, audit.Actor(ctx), "address.added", accountID.String(),
				map[string]any{"address": address}); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx,
			`SELECT created_at FROM addresses WHERE account_id = $1 AND address = $2`,
			accountID, address).Scan(&a.CreatedAt)
	})
	return a, err
}

// RequestWithdrawal runs a withdrawal through the risk engine and moves funds
// to match the decision, all in one database transaction:
//
//	approve  customer -> external
//	review   customer -> hold, until a reviewer decides
//	reject   nothing moves; the attempt is recorded for velocity checks
//
// The customer account row is locked first, which serialises withdrawals
// per account so the 24h and per hour checks see every earlier request.
func (s *Store) RequestWithdrawal(ctx context.Context, accountID uuid.UUID, address string, amount int64, key string) (Withdrawal, error) {
	if amount <= 0 {
		return Withdrawal{}, ErrInvalidAmount
	}
	if key == "" {
		return Withdrawal{}, ErrMissingIdempotencyKey
	}
	if !addressPattern.MatchString(address) {
		return Withdrawal{}, ErrInvalidAddress
	}

	var w Withdrawal
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			asset   string
			balance int64
		)
		err := tx.QueryRowContext(ctx, `
			SELECT asset, balance FROM accounts
			 WHERE id = $1 AND kind = 'user' FOR UPDATE`, accountID,
		).Scan(&asset, &balance)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountNotFound
		}
		if err != nil {
			return err
		}

		// Checked after taking the lock, so two concurrent retries of the
		// same request cannot both miss each other.
		existing, err := withdrawalByKey(ctx, tx, key)
		if err == nil {
			if existing.AccountID != accountID || existing.Address != address || existing.Amount != amount {
				return ErrIdempotencyKeyConflict
			}
			existing.Replayed = true
			w = existing
			return nil
		}
		if !errors.Is(err, ErrWithdrawalNotFound) {
			return err
		}

		if balance < amount {
			return ErrInsufficientFunds
		}

		now := s.Now()
		in := risk.Input{Amount: amount, Now: now}
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(amount) FILTER (WHERE status <> 'rejected' AND created_at > $2), 0),
			       COUNT(*) FILTER (WHERE created_at > $3)
			  FROM withdrawals WHERE account_id = $1`,
			accountID, now.Add(-24*time.Hour), now.Add(-time.Hour),
		).Scan(&in.WithdrawnLast24h, &in.CountLastHour); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx,
			`SELECT created_at FROM addresses WHERE account_id = $1 AND address = $2`,
			accountID, address).Scan(&in.AddressAddedAt)
		switch {
		case err == nil:
			in.AddressKnown = true
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}

		policy := s.policy(asset)
		decision := policy.Evaluate(in)
		w = Withdrawal{
			ID: uuid.New(), AccountID: accountID, Address: address, Amount: amount,
			Reasons: decision.Reasons, CreatedAt: now,
			Approvals: []string{}, ApprovalsRequired: policy.Approvals(),
		}
		if w.Reasons == nil {
			w.Reasons = []string{}
		}

		var holdTxn, finalTxn *uuid.UUID
		switch decision.Outcome {
		case risk.Approve:
			external, err := s.systemAccount(ctx, tx, asset, KindExternal)
			if err != nil {
				return err
			}
			txn, err := s.post(ctx, tx, "withdrawal", "withdrawal:"+w.ID.String(), []leg{
				{account: accountID, amount: -amount},
				{account: external, amount: amount},
			})
			if err != nil {
				return err
			}
			w.Status, w.DecidedAt, finalTxn = StatusApproved, &now, &txn.ID
			if err := enqueueSend(ctx, tx, w, asset); err != nil {
				return err
			}
		case risk.Review:
			hold, err := s.systemAccount(ctx, tx, asset, KindHold)
			if err != nil {
				return err
			}
			txn, err := s.post(ctx, tx, "withdrawal_hold", "withdrawal:"+w.ID.String()+":hold", []leg{
				{account: accountID, amount: -amount},
				{account: hold, amount: amount},
			})
			if err != nil {
				return err
			}
			w.Status, holdTxn = StatusPendingReview, &txn.ID
		default:
			w.Status, w.DecidedAt = StatusRejected, &now
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO withdrawals
			  (id, account_id, address, amount, status, reasons, idempotency_key,
			   hold_txn, final_txn, created_at, decided_at, approvals_required)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			w.ID, accountID, address, amount, w.Status, pq.Array(w.Reasons), key,
			holdTxn, finalTxn, now, w.DecidedAt, w.ApprovalsRequired)
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Actor(ctx), "withdrawal.requested", w.ID.String(),
			map[string]any{"account_id": accountID, "address": address, "amount": amount,
				"status": w.Status, "reasons": w.Reasons})
	})
	return w, err
}

// ReviewWithdrawal records one operator's decision on a held withdrawal.
//
// A rejection by any reviewer releases the held funds back to the customer.
// An approval is recorded, and only when approvals from the required number
// of distinct operators are in does the withdrawal settle to the external
// account. The same operator approving twice is refused, so one person can
// never satisfy four-eyes alone. The withdrawal row is locked for the whole
// decision, so two reviewers acting at once are applied one after the other.
func (s *Store) ReviewWithdrawal(ctx context.Context, id uuid.UUID, approve bool, reviewer string) (Withdrawal, error) {
	if reviewer == "" {
		return Withdrawal{}, ErrReviewerRequired
	}
	var w Withdrawal
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Lock first, read second. Under READ COMMITTED, a statement that
		// waits for a row lock re-checks that row but still evaluates its
		// subqueries against the snapshot taken before the wait. Reading
		// the approvals in the same statement as FOR UPDATE would miss an
		// approval committed while we waited, letting a third approval in.
		// TestConcurrentApprovalsSettleOnce catches exactly that.
		var locked bool
		err := tx.QueryRowContext(ctx,
			`SELECT true FROM withdrawals WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWithdrawalNotFound
		}
		if err != nil {
			return err
		}
		w, err = scanWithdrawal(tx.QueryRowContext(ctx, withdrawalSelect+` WHERE id = $1`, id))
		if err != nil {
			return err
		}
		if w.Status != StatusPendingReview {
			return ErrNotPendingReview
		}
		if slices.Contains(w.Approvals, reviewer) {
			return ErrAlreadyReviewed
		}

		if approve {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO withdrawal_approvals (withdrawal_id, reviewer) VALUES ($1, $2)`,
				w.ID, reviewer); err != nil {
				return err
			}
			w.Approvals = append(w.Approvals, reviewer)
			if len(w.Approvals) < w.ApprovalsRequired {
				return audit.Write(ctx, tx, audit.Actor(ctx), "withdrawal.approval_recorded", w.ID.String(),
					map[string]any{"reviewer": reviewer, "approvals": len(w.Approvals),
						"required": w.ApprovalsRequired})
			}
		}

		var asset string
		if err := tx.QueryRowContext(ctx,
			`SELECT asset FROM accounts WHERE id = $1`, w.AccountID).Scan(&asset); err != nil {
			return err
		}
		hold, err := s.systemAccount(ctx, tx, asset, KindHold)
		if err != nil {
			return err
		}

		var (
			kind, suffix string
			to           uuid.UUID
		)
		if approve {
			if to, err = s.systemAccount(ctx, tx, asset, KindExternal); err != nil {
				return err
			}
			kind, suffix, w.Status = "withdrawal", ":settle", StatusApproved
			w.ReviewedBy = strings.Join(w.Approvals, ",")
		} else {
			to = w.AccountID
			kind, suffix, w.Status = "withdrawal_release", ":release", StatusRejected
			w.ReviewedBy = reviewer
			w.Reasons = append(w.Reasons, fmt.Sprintf("rejected on review by %s", reviewer))
		}
		txn, err := s.post(ctx, tx, kind, "withdrawal:"+w.ID.String()+suffix, []leg{
			{account: hold, amount: -w.Amount},
			{account: to, amount: w.Amount},
		})
		if err != nil {
			return err
		}

		if approve {
			if err := enqueueSend(ctx, tx, w, asset); err != nil {
				return err
			}
		}

		now := s.Now()
		w.DecidedAt = &now
		_, err = tx.ExecContext(ctx, `
			UPDATE withdrawals
			   SET status = $2, reasons = $3, final_txn = $4, reviewed_by = $5, decided_at = $6
			 WHERE id = $1`,
			w.ID, w.Status, pq.Array(w.Reasons), txn.ID, w.ReviewedBy, now)
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Actor(ctx), "withdrawal."+w.Status, w.ID.String(),
			map[string]any{"reviewer": reviewer, "approvals": w.Approvals})
	})
	return w, err
}

// TopicWithdrawalSend is the outbox topic a signer consumes to broadcast
// approved withdrawals on chain.
const TopicWithdrawalSend = "withdrawal.send"

// enqueueSend announces an approved withdrawal in the same transaction that
// approves it. The withdrawal id is the consumer's idempotency key.
func enqueueSend(ctx context.Context, tx *sql.Tx, w Withdrawal, asset string) error {
	return outbox.Enqueue(ctx, tx, TopicWithdrawalSend, w.ID.String(), map[string]any{
		"withdrawal_id": w.ID,
		"account_id":    w.AccountID,
		"asset":         asset,
		"address":       w.Address,
		"amount":        w.Amount,
	})
}

// PendingReviews lists held withdrawals, oldest first.
func (s *Store) PendingReviews(ctx context.Context, limit int) ([]Withdrawal, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, withdrawalSelect+`
		WHERE status = 'pending_review' ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Withdrawal{}
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWithdrawal returns one withdrawal by id.
func (s *Store) GetWithdrawal(ctx context.Context, id uuid.UUID) (Withdrawal, error) {
	return scanWithdrawal(s.db.QueryRowContext(ctx, withdrawalSelect+` WHERE id = $1`, id))
}

const withdrawalSelect = `
	SELECT id, account_id, address, amount, status, reasons,
	       COALESCE(reviewed_by, ''), approvals_required,
	       ARRAY(SELECT reviewer FROM withdrawal_approvals a
	              WHERE a.withdrawal_id = withdrawals.id ORDER BY a.created_at, a.reviewer),
	       created_at, decided_at
	  FROM withdrawals`

type scanner interface{ Scan(dest ...any) error }

func scanWithdrawal(row scanner) (Withdrawal, error) {
	var (
		w         Withdrawal
		reasons   pq.StringArray
		approvals pq.StringArray
		decided   sql.NullTime
	)
	err := row.Scan(&w.ID, &w.AccountID, &w.Address, &w.Amount, &w.Status, &reasons,
		&w.ReviewedBy, &w.ApprovalsRequired, &approvals, &w.CreatedAt, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return Withdrawal{}, ErrWithdrawalNotFound
	}
	if err != nil {
		return Withdrawal{}, err
	}
	w.Reasons = []string(reasons)
	if w.Reasons == nil {
		w.Reasons = []string{}
	}
	w.Approvals = []string(approvals)
	if w.Approvals == nil {
		w.Approvals = []string{}
	}
	if decided.Valid {
		t := decided.Time
		w.DecidedAt = &t
	}
	return w, nil
}

func withdrawalByKey(ctx context.Context, q querier, key string) (Withdrawal, error) {
	return scanWithdrawal(q.QueryRowContext(ctx, withdrawalSelect+` WHERE idempotency_key = $1`, key))
}

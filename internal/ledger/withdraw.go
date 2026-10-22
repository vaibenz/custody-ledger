package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/vaibenz/custody-ledger/internal/risk"
)

var (
	ErrInvalidAddress     = errors.New("address must be 20 to 100 letters and digits")
	ErrWithdrawalNotFound = errors.New("withdrawal not found")
	ErrNotPendingReview   = errors.New("withdrawal is not pending review")
	ErrReviewerRequired   = errors.New("reviewer is required")
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
	ID         uuid.UUID  `json:"id"`
	AccountID  uuid.UUID  `json:"account_id"`
	Address    string     `json:"address"`
	Amount     int64      `json:"amount"`
	Status     string     `json:"status"`
	Reasons    []string   `json:"reasons"`
	ReviewedBy string     `json:"reviewed_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	Replayed   bool       `json:"replayed"`
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
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO addresses (account_id, address, created_at) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, accountID, address, s.Now()); err != nil {
		return Address{}, err
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT created_at FROM addresses WHERE account_id = $1 AND address = $2`,
		accountID, address).Scan(&a.CreatedAt)
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

		decision := s.policy(asset).Evaluate(in)
		w = Withdrawal{
			ID: uuid.New(), AccountID: accountID, Address: address, Amount: amount,
			Reasons: decision.Reasons, CreatedAt: now,
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
			   hold_txn, final_txn, created_at, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			w.ID, accountID, address, amount, w.Status, pq.Array(w.Reasons), key,
			holdTxn, finalTxn, now, w.DecidedAt)
		return err
	})
	return w, err
}

// ReviewWithdrawal records a reviewer's decision on a held withdrawal and
// moves the held funds out to the external account or back to the customer.
func (s *Store) ReviewWithdrawal(ctx context.Context, id uuid.UUID, approve bool, reviewer string) (Withdrawal, error) {
	if reviewer == "" {
		return Withdrawal{}, ErrReviewerRequired
	}
	var w Withdrawal
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		w, err = scanWithdrawal(tx.QueryRowContext(ctx,
			withdrawalSelect+` WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if w.Status != StatusPendingReview {
			return ErrNotPendingReview
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
		} else {
			to = w.AccountID
			kind, suffix, w.Status = "withdrawal_release", ":release", StatusRejected
			w.Reasons = append(w.Reasons, fmt.Sprintf("rejected on review by %s", reviewer))
		}
		txn, err := s.post(ctx, tx, kind, "withdrawal:"+w.ID.String()+suffix, []leg{
			{account: hold, amount: -w.Amount},
			{account: to, amount: w.Amount},
		})
		if err != nil {
			return err
		}

		now := s.Now()
		w.DecidedAt, w.ReviewedBy = &now, reviewer
		_, err = tx.ExecContext(ctx, `
			UPDATE withdrawals
			   SET status = $2, reasons = $3, final_txn = $4, reviewed_by = $5, decided_at = $6
			 WHERE id = $1`,
			w.ID, w.Status, pq.Array(w.Reasons), txn.ID, reviewer, now)
		return err
	})
	return w, err
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
	       COALESCE(reviewed_by, ''), created_at, decided_at
	  FROM withdrawals`

type scanner interface{ Scan(dest ...any) error }

func scanWithdrawal(row scanner) (Withdrawal, error) {
	var (
		w       Withdrawal
		reasons pq.StringArray
		decided sql.NullTime
	)
	err := row.Scan(&w.ID, &w.AccountID, &w.Address, &w.Amount, &w.Status, &reasons,
		&w.ReviewedBy, &w.CreatedAt, &decided)
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
	if decided.Valid {
		t := decided.Time
		w.DecidedAt = &t
	}
	return w, nil
}

func withdrawalByKey(ctx context.Context, q querier, key string) (Withdrawal, error) {
	return scanWithdrawal(q.QueryRowContext(ctx, withdrawalSelect+` WHERE idempotency_key = $1`, key))
}

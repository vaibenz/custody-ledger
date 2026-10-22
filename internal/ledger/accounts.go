package ledger

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/audit"
)

var assetPattern = regexp.MustCompile(`^[A-Z]{2,10}$`)

// CreateAccount opens a customer account for one asset. The asset's system
// accounts are created on first use.
func (s *Store) CreateAccount(ctx context.Context, owner, asset string) (Account, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Account{}, ErrInvalidOwner
	}
	if !assetPattern.MatchString(asset) {
		return Account{}, ErrInvalidAsset
	}
	if err := s.ensureSystemAccounts(ctx, asset); err != nil {
		return Account{}, err
	}
	a := Account{ID: uuid.New(), Owner: owner, Asset: asset, Kind: KindUser}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO accounts (id, owner, asset, kind) VALUES ($1, $2, $3, 'user')
			RETURNING balance, created_at`, a.ID, owner, asset,
		).Scan(&a.Balance, &a.CreatedAt); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Actor(ctx), "account.created", a.ID.String(),
			map[string]any{"owner": owner, "asset": asset})
	})
	return a, err
}

// ListAccounts returns an owner's accounts, oldest first.
func (s *Store) ListAccounts(ctx context.Context, owner string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner, asset, kind, balance, created_at
		  FROM accounts WHERE owner = $1 AND kind = 'user' ORDER BY created_at, id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Owner, &a.Asset, &a.Kind, &a.Balance, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ensureSystemAccounts(ctx context.Context, asset string) error {
	for _, kind := range []string{KindExternal, KindHold} {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO accounts (id, owner, asset, kind) VALUES ($1, 'system', $2, $3)
			ON CONFLICT (asset, kind) WHERE kind <> 'user' DO NOTHING`,
			uuid.New(), asset, kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) systemAccount(ctx context.Context, q querier, asset, kind string) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE asset = $1 AND kind = $2`, asset, kind).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrAccountNotFound
	}
	return id, err
}

// GetAccount returns a customer account. System accounts are not exposed.
func (s *Store) GetAccount(ctx context.Context, id uuid.UUID) (Account, error) {
	return getAccount(ctx, s.db, id)
}

func getAccount(ctx context.Context, q querier, id uuid.UUID) (Account, error) {
	var a Account
	err := q.QueryRowContext(ctx, `
		SELECT id, owner, asset, kind, balance, created_at
		  FROM accounts WHERE id = $1 AND kind = 'user'`, id,
	).Scan(&a.ID, &a.Owner, &a.Asset, &a.Kind, &a.Balance, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	return a, err
}

// Deposit credits a customer account with value arriving from outside
// custody, for example a confirmed on chain deposit.
func (s *Store) Deposit(ctx context.Context, accountID uuid.UUID, amount int64, key string) (Transaction, error) {
	if amount <= 0 {
		return Transaction{}, ErrInvalidAmount
	}
	acct, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return Transaction{}, err
	}
	external, err := s.systemAccount(ctx, s.db, acct.Asset, KindExternal)
	if err != nil {
		return Transaction{}, err
	}
	var txn Transaction
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		txn, err = s.post(ctx, tx, "deposit", key, []leg{
			{account: external, amount: -amount},
			{account: accountID, amount: amount},
		})
		if err != nil || txn.Replayed {
			return err
		}
		return audit.Write(ctx, tx, audit.Actor(ctx), "deposit", accountID.String(),
			map[string]any{"amount": amount, "transaction_id": txn.ID})
	})
	return txn, err
}

// Entries lists an account's ledger entries, newest first.
func (s *Store) Entries(ctx context.Context, accountID uuid.UUID, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, transaction_id, account_id, amount, created_at
		  FROM entries WHERE account_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.TransactionID, &e.AccountID, &e.Amount, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

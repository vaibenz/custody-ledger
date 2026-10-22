// Package ledger implements a custodial double entry ledger on PostgreSQL.
//
// Amounts are int64 in the asset's smallest unit (satoshis for BTC, wei is
// out of range for int64 so ETH is tracked in gwei, and so on). Floating
// point never touches money.
package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/vaibenz/custody-ledger/internal/risk"
)

var (
	ErrAccountNotFound        = errors.New("account not found")
	ErrInsufficientFunds      = errors.New("insufficient funds")
	ErrAssetMismatch          = errors.New("accounts hold different assets")
	ErrInvalidAmount          = errors.New("amount must be positive")
	ErrSameAccount            = errors.New("cannot transfer to the same account")
	ErrMissingIdempotencyKey  = errors.New("idempotency key is required")
	ErrIdempotencyKeyConflict = errors.New("idempotency key was already used for a different request")
	ErrInvalidAsset           = errors.New("asset must be 2 to 10 uppercase letters")
	ErrInvalidOwner           = errors.New("owner is required")
)

// Account kinds. User accounts belong to customers. Each asset also has one
// external account, the counterparty for value entering or leaving custody,
// and one hold account where withdrawals under review are parked.
const (
	KindUser     = "user"
	KindExternal = "external"
	KindHold     = "hold"
)

type Account struct {
	ID        uuid.UUID `json:"id"`
	Owner     string    `json:"owner"`
	Asset     string    `json:"asset"`
	Kind      string    `json:"kind"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

type Transaction struct {
	ID             uuid.UUID `json:"id"`
	Kind           string    `json:"kind"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
	// Replayed is true when this call matched an earlier request with the
	// same idempotency key and nothing new was written.
	Replayed bool `json:"replayed"`
}

type Entry struct {
	ID            int64     `json:"id"`
	TransactionID uuid.UUID `json:"transaction_id"`
	AccountID     uuid.UUID `json:"account_id"`
	Amount        int64     `json:"amount"`
	CreatedAt     time.Time `json:"created_at"`
}

// leg is one side of a transaction: a signed change to an account.
type leg struct {
	account uuid.UUID
	amount  int64
}

// Store is safe for concurrent use.
type Store struct {
	db *sql.DB
	// Now is the clock used for time windows. Tests override it.
	Now func() time.Time
	// ExternalShards is how many external accounts each asset is spread
	// over. Set before any account is opened.
	ExternalShards int

	mu       sync.RWMutex
	policies map[string]risk.Policy
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, Now: time.Now, ExternalShards: 8}
}

// DB exposes the pool for read only helpers such as the audit log.
func (s *Store) DB() *sql.DB { return s.db }

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// withTx runs fn in a READ COMMITTED transaction. Correctness comes from
// explicit row locks rather than isolation level. A deadlock is retried,
// since Postgres has already rolled the victim back.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	const maxAttempts = 3
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = s.runTx(ctx, fn)
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "40P01" {
			continue
		}
		return err
	}
	return err
}

func (s *Store) runTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func requestHash(kind string, legs []leg) string {
	var b strings.Builder
	b.WriteString(kind)
	for _, l := range legs {
		fmt.Fprintf(&b, "|%s:%d", l.account, l.amount)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// post writes one balanced transaction inside tx.
//
// Idempotency: the transaction row is inserted first with ON CONFLICT DO
// NOTHING on the key. If a concurrent request holds the same key, this
// statement waits for it to commit, then sees the conflict and returns the
// existing transaction instead of applying the change twice. Reusing a key
// for a different request is an error rather than a silent replay.
//
// Concurrency: every touched account is locked with SELECT ... FOR UPDATE in
// one global order (user accounts before system accounts, then by id), so
// two transfers between the same accounts in opposite directions cannot
// deadlock, and a balance is never read and written by two writers at once.
func (s *Store) post(ctx context.Context, tx *sql.Tx, kind, key string, legs []leg) (Transaction, error) {
	if key == "" {
		return Transaction{}, ErrMissingIdempotencyKey
	}
	var total int64
	for _, l := range legs {
		if l.amount == 0 {
			return Transaction{}, ErrInvalidAmount
		}
		total += l.amount
	}
	if total != 0 {
		return Transaction{}, fmt.Errorf("ledger: legs of %s do not balance (%d)", kind, total)
	}

	hash := requestHash(kind, legs)
	txn := Transaction{ID: uuid.New(), Kind: kind, IdempotencyKey: key}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO transactions (id, kind, idempotency_key, request_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING created_at`,
		txn.ID, kind, key, hash,
	).Scan(&txn.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return replay(ctx, tx, key, hash)
	}
	if err != nil {
		return Transaction{}, err
	}

	// Net change per account, so an account appearing in two legs is
	// checked once.
	net := map[uuid.UUID]int64{}
	ids := make([]string, 0, len(legs))
	for _, l := range legs {
		if _, seen := net[l.account]; !seen {
			ids = append(ids, l.account.String())
		}
		net[l.account] += l.amount
	}
	sort.Strings(ids)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, asset, kind, balance
		  FROM accounts
		 WHERE id = ANY($1::uuid[])
		 ORDER BY (kind <> 'user'), id
		   FOR UPDATE`, pq.Array(ids))
	if err != nil {
		return Transaction{}, err
	}
	locked := 0
	asset := ""
	for rows.Next() {
		var (
			id                  uuid.UUID
			acctAsset, acctKind string
			balance             int64
		)
		if err := rows.Scan(&id, &acctAsset, &acctKind, &balance); err != nil {
			rows.Close()
			return Transaction{}, err
		}
		locked++
		if asset == "" {
			asset = acctAsset
		} else if asset != acctAsset {
			rows.Close()
			return Transaction{}, ErrAssetMismatch
		}
		if acctKind != KindExternal && balance+net[id] < 0 {
			rows.Close()
			return Transaction{}, ErrInsufficientFunds
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Transaction{}, err
	}
	if locked != len(ids) {
		return Transaction{}, ErrAccountNotFound
	}

	for _, l := range legs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entries (transaction_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4)`,
			txn.ID, l.account, l.amount, txn.CreatedAt); err != nil {
			return Transaction{}, err
		}
	}
	for id, delta := range net {
		if delta == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET balance = balance + $2 WHERE id = $1`, id, delta); err != nil {
			return Transaction{}, err
		}
	}
	return txn, nil
}

func replay(ctx context.Context, q querier, key, hash string) (Transaction, error) {
	var (
		txn          Transaction
		existingHash string
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, kind, idempotency_key, request_hash, created_at
		  FROM transactions WHERE idempotency_key = $1`, key,
	).Scan(&txn.ID, &txn.Kind, &txn.IdempotencyKey, &existingHash, &txn.CreatedAt)
	if err != nil {
		return Transaction{}, err
	}
	if existingHash != hash {
		return Transaction{}, ErrIdempotencyKeyConflict
	}
	txn.Replayed = true
	return txn, nil
}

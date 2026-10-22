package ledger

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
)

// Transfer moves amount between two customer accounts holding the same
// asset. It is safe to retry with the same key: a retry returns the
// original transaction and moves nothing.
func (s *Store) Transfer(ctx context.Context, from, to uuid.UUID, amount int64, key string) (Transaction, error) {
	if amount <= 0 {
		return Transaction{}, ErrInvalidAmount
	}
	if from == to {
		return Transaction{}, ErrSameAccount
	}
	var txn Transaction
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Both ends must be customer accounts. Kind never changes, so this
		// read does not need the row lock that post takes.
		if _, err := getAccount(ctx, tx, from); err != nil {
			return err
		}
		if _, err := getAccount(ctx, tx, to); err != nil {
			return err
		}
		var err error
		txn, err = s.post(ctx, tx, "transfer", key, []leg{
			{account: from, amount: -amount},
			{account: to, amount: amount},
		})
		return err
	})
	return txn, err
}

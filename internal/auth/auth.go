// Package auth issues and checks credentials for the three kinds of caller:
//
//	customer  an API key (ck_...) tied to one owner; sees only that owner's accounts
//	operator  a named staff token (op_...); credits deposits, reviews withdrawals
//	root      the bootstrap ADMIN_TOKEN; creates operators and customer keys only
//
// Root deliberately cannot move money or review withdrawals, so those
// actions are always attributable to a named person.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/audit"
)

var (
	ErrUnauthenticated = errors.New("missing or invalid credentials")
	ErrInvalidName     = errors.New("name must be 1 to 64 letters, digits, dot, dash or underscore")
	ErrNameTaken       = errors.New("an operator with that name already exists")
	ErrKeyNotFound     = errors.New("key not found")
)

const (
	KindCustomer = "customer"
	KindOperator = "operator"
	KindRoot     = "root"

	customerPrefix = "ck_"
	operatorPrefix = "op_"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type Principal struct {
	Kind string
	Name string
}

// Actor is the audit log identity, e.g. "operator:bob".
func (p Principal) Actor() string { return p.Kind + ":" + p.Name }

type Store struct {
	db        *sql.DB
	rootToken string
}

// NewStore takes the bootstrap root token; an empty one disables root.
func NewStore(db *sql.DB, rootToken string) *Store {
	return &Store{db: db, rootToken: rootToken}
}

func newSecret(prefix string) (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	secret := prefix + base64.RawURLEncoding.EncodeToString(b)
	return secret, hash(secret), nil
}

func hash(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// IssueKey creates an API key for owner. The key is returned once and never
// stored in plain text.
func (s *Store) IssueKey(ctx context.Context, owner string) (key string, id uuid.UUID, err error) {
	if !namePattern.MatchString(owner) {
		return "", uuid.Nil, ErrInvalidName
	}
	key, h, err := newSecret(customerPrefix)
	if err != nil {
		return "", uuid.Nil, err
	}
	id = uuid.New()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO api_keys (id, owner, key_hash, prefix) VALUES ($1, $2, $3, $4)`,
			id, owner, h, key[:10]); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Actor(ctx), "api_key.issued", "owner:"+owner,
			map[string]any{"key_id": id, "prefix": key[:10]})
	})
	return key, id, err
}

// RevokeKey disables a key immediately.
func (s *Store) RevokeKey(ctx context.Context, id uuid.UUID) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx,
			`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL RETURNING owner`,
			id).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrKeyNotFound
		}
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Actor(ctx), "api_key.revoked", "owner:"+owner,
			map[string]any{"key_id": id})
	})
}

// CreateOperator adds a named operator and returns their token once.
func (s *Store) CreateOperator(ctx context.Context, name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", ErrInvalidName
	}
	token, h, err := newSecret(operatorPrefix)
	if err != nil {
		return "", err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO operators (name, token_hash) VALUES ($1, $2) ON CONFLICT DO NOTHING`, name, h)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNameTaken
		}
		return audit.Write(ctx, tx, audit.Actor(ctx), "operator.created", "operator:"+name, nil)
	})
	return token, err
}

// Authenticate resolves a bearer token to a principal.
func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	switch {
	case strings.HasPrefix(token, customerPrefix):
		var owner string
		err := s.db.QueryRowContext(ctx,
			`SELECT owner FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
			hash(token)).Scan(&owner)
		if err != nil {
			return Principal{}, notFoundAs(err, ErrUnauthenticated)
		}
		return Principal{Kind: KindCustomer, Name: owner}, nil

	case strings.HasPrefix(token, operatorPrefix):
		var name string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM operators WHERE token_hash = $1 AND disabled_at IS NULL`,
			hash(token)).Scan(&name)
		if err != nil {
			return Principal{}, notFoundAs(err, ErrUnauthenticated)
		}
		return Principal{Kind: KindOperator, Name: name}, nil

	case s.rootToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.rootToken)) == 1:
		return Principal{Kind: KindRoot, Name: "root"}, nil
	}
	return Principal{}, ErrUnauthenticated
}

func notFoundAs(err, as error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return as
	}
	return err
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
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

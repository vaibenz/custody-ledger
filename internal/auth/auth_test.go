package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vaibenz/custody-ledger/internal/audit"
	"github.com/vaibenz/custody-ledger/internal/auth"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

func TestCredentialsResolveToPrincipals(t *testing.T) {
	pool := testdb.New(t)
	s := auth.NewStore(pool, "root-secret")
	ctx := audit.WithActor(context.Background(), "root:root")

	key, keyID, err := s.IssueKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "ck_") || len(key) < 40 {
		t.Fatalf("key looks weak: %q", key)
	}
	opToken, err := s.CreateOperator(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		token string
		want  auth.Principal
	}{
		{key, auth.Principal{Kind: auth.KindCustomer, Name: "alice"}},
		{opToken, auth.Principal{Kind: auth.KindOperator, Name: "bob"}},
		{"root-secret", auth.Principal{Kind: auth.KindRoot, Name: "root"}},
	}
	for _, tc := range cases {
		got, err := s.Authenticate(ctx, tc.token)
		if err != nil || got != tc.want {
			t.Errorf("Authenticate(%.8s...) = %+v, %v; want %+v", tc.token, got, err, tc.want)
		}
	}

	for _, bad := range []string{"", "ck_nope", "op_nope", "root-secre", key + "x"} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) = %v, want unauthenticated", bad, err)
		}
	}

	// Plain text keys are never stored.
	var stored int
	pool.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE key_hash = $1::bytea`, []byte(key)).Scan(&stored)
	if stored != 0 {
		t.Error("raw key found in database")
	}

	if err := s.RevokeKey(ctx, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, key); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("revoked key still works: %v", err)
	}
	if err := s.RevokeKey(ctx, keyID); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("second revoke: %v", err)
	}

	log, _ := audit.ForSubject(ctx, pool, "owner:alice")
	if len(log) != 2 || log[0].Action != "api_key.issued" || log[1].Actor != "root:root" {
		t.Errorf("audit = %+v", log)
	}
}

func TestOperatorNames(t *testing.T) {
	s := auth.NewStore(testdb.New(t), "")
	ctx := context.Background()
	if _, err := s.CreateOperator(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOperator(ctx, "bob"); !errors.Is(err, auth.ErrNameTaken) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := s.CreateOperator(ctx, "bob smith"); !errors.Is(err, auth.ErrInvalidName) {
		t.Errorf("invalid: %v", err)
	}
	if _, err := s.Authenticate(ctx, ""); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("empty root token must not authenticate: %v", err)
	}
}

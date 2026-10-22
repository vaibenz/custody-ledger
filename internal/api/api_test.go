package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vaibenz/custody-ledger/internal/api"
	"github.com/vaibenz/custody-ledger/internal/auth"
	"github.com/vaibenz/custody-ledger/internal/ledger"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

const rootToken = "root-test-token"

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *ledger.Store
}

type resp struct {
	status int
	body   map[string]any
}

func (e env) call(token, method, path string, body any, idemKey string) resp {
	e.t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	return resp{r.StatusCode, out}
}

func (e env) must(want int, r resp) map[string]any {
	e.t.Helper()
	if r.status != want {
		e.t.Fatalf("status %d, want %d: %v", r.status, want, r.body)
	}
	return r.body
}

func newEnv(t *testing.T) env {
	pool := testdb.New(t)
	store := ledger.NewStore(pool)
	h := api.New(store, auth.NewStore(pool, rootToken), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return env{t: t, srv: srv, store: store}
}

// credentials creates an operator and two customers through the root API.
func (e env) credentials() (op, alice, bob string) {
	op = e.must(201, e.call(rootToken, "POST", "/v1/admin/operators", map[string]any{"name": "ops-1"}, ""))["token"].(string)
	alice = e.must(201, e.call(rootToken, "POST", "/v1/admin/keys", map[string]any{"owner": "alice"}, ""))["key"].(string)
	bob = e.must(201, e.call(rootToken, "POST", "/v1/admin/keys", map[string]any{"owner": "bob"}, ""))["key"].(string)
	return
}

func TestEndToEndFlow(t *testing.T) {
	e := newEnv(t)
	op, alice, bob := e.credentials()

	aliceAcct := e.must(201, e.call(alice, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, ""))
	bobAcct := e.must(201, e.call(bob, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, ""))
	aID, bID := aliceAcct["id"].(string), bobAcct["id"].(string)
	if aliceAcct["owner"] != "alice" {
		t.Errorf("owner comes from the key, got %v", aliceAcct["owner"])
	}

	e.must(201, e.call(op, "POST", "/v1/deposits", map[string]any{"account_id": aID, "amount": 10_000}, "d1"))

	pay := map[string]any{"from": aID, "to": bID, "amount": 2_500}
	e.must(201, e.call(alice, "POST", "/v1/transfers", pay, "t1"))
	if body := e.must(200, e.call(alice, "POST", "/v1/transfers", pay, "t1")); body["replayed"] != true {
		t.Errorf("retry not replayed: %v", body)
	}
	if bal := e.must(200, e.call(alice, "GET", "/v1/accounts/"+aID, nil, ""))["balance"]; bal != 7_500.0 {
		t.Errorf("alice balance %v", bal)
	}
	list := e.must(200, e.call(bob, "GET", "/v1/accounts", nil, ""))["accounts"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["balance"] != 2_500.0 {
		t.Errorf("bob accounts %v", list)
	}

	// A new address is held for review.
	addr := "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"
	e.must(201, e.call(alice, "POST", "/v1/accounts/"+aID+"/addresses", map[string]any{"address": addr}, ""))
	wd := e.must(202, e.call(alice, "POST", "/v1/withdrawals",
		map[string]any{"account_id": aID, "address": addr, "amount": 1_000}, "w1"))
	wID := wd["id"].(string)

	reviews := e.must(200, e.call(op, "GET", "/v1/reviews", nil, ""))["withdrawals"].([]any)
	if len(reviews) != 1 {
		t.Fatalf("reviews %v", reviews)
	}
	// Four-eyes: one approval is not enough, and the same operator cannot
	// approve twice.
	first := e.must(200, e.call(op, "POST", "/v1/reviews/"+wID+"/approve", nil, ""))
	if first["status"] != "pending_review" {
		t.Errorf("after one approval %v", first)
	}
	e.must(409, e.call(op, "POST", "/v1/reviews/"+wID+"/approve", nil, ""))
	op2 := e.must(201, e.call(rootToken, "POST", "/v1/admin/operators", map[string]any{"name": "ops-2"}, ""))["token"].(string)
	decided := e.must(200, e.call(op2, "POST", "/v1/reviews/"+wID+"/approve", nil, ""))
	if decided["status"] != "approved" || decided["reviewed_by"] != "ops-1,ops-2" {
		t.Errorf("decided %v", decided)
	}
	if got := e.must(200, e.call(alice, "GET", "/v1/withdrawals/"+wID, nil, "")); got["status"] != "approved" {
		t.Errorf("customer view %v", got)
	}

	// The audit trail names every actor.
	trail := e.must(200, e.call(op, "GET", "/v1/audit/"+wID, nil, ""))["entries"].([]any)
	actors := []string{}
	for _, x := range trail {
		actors = append(actors, x.(map[string]any)["actor"].(string)+" "+x.(map[string]any)["action"].(string))
	}
	want := []string{
		"customer:alice withdrawal.requested",
		"operator:ops-1 withdrawal.approval_recorded",
		"operator:ops-2 withdrawal.approved",
	}
	if strings.Join(actors, "|") != strings.Join(want, "|") {
		t.Errorf("audit trail %v", actors)
	}
}

func TestCustomersCannotTouchOthersAccounts(t *testing.T) {
	e := newEnv(t)
	op, alice, bob := e.credentials()
	aID := e.must(201, e.call(alice, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, ""))["id"].(string)
	bID := e.must(201, e.call(bob, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, ""))["id"].(string)
	e.must(201, e.call(op, "POST", "/v1/deposits", map[string]any{"account_id": aID, "amount": 5_000}, "d1"))

	// Bob tries to spend, read and withdraw from Alice's account. Every
	// attempt looks exactly like a missing account.
	attempts := []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/accounts/" + aID, nil},
		{"GET", "/v1/accounts/" + aID + "/entries", nil},
		{"POST", "/v1/transfers", map[string]any{"from": aID, "to": bID, "amount": 100}},
		{"POST", "/v1/accounts/" + aID + "/addresses", map[string]any{"address": "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"}},
		{"POST", "/v1/withdrawals", map[string]any{"account_id": aID, "address": "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh", "amount": 100}},
	}
	for _, a := range attempts {
		if r := e.call(bob, a.method, a.path, a.body, "k-"+a.path); r.status != 404 {
			t.Errorf("bob %s %s: %d %v", a.method, a.path, r.status, r.body)
		}
	}
	if bal := e.must(200, e.call(alice, "GET", "/v1/accounts/"+aID, nil, ""))["balance"]; bal != 5_000.0 {
		t.Errorf("alice balance changed to %v", bal)
	}
}

func TestRolesAreEnforced(t *testing.T) {
	e := newEnv(t)
	op, alice, _ := e.credentials()
	aID := e.must(201, e.call(alice, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, ""))["id"].(string)

	cases := []struct {
		name, token, method, path string
		body                      any
		want                      int
	}{
		{"no token", "", "GET", "/v1/accounts", nil, 401},
		{"garbage token", "ck_garbage", "GET", "/v1/accounts", nil, 401},
		{"customer cannot mint money", alice, "POST", "/v1/deposits", map[string]any{"account_id": aID, "amount": 1}, 403},
		{"customer cannot review", alice, "GET", "/v1/reviews", nil, 403},
		{"customer cannot issue keys", alice, "POST", "/v1/admin/keys", map[string]any{"owner": "eve"}, 403},
		{"operator cannot open accounts", op, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, 403},
		{"operator cannot issue keys", op, "POST", "/v1/admin/keys", map[string]any{"owner": "eve"}, 403},
		{"root cannot move money", rootToken, "POST", "/v1/deposits", map[string]any{"account_id": aID, "amount": 1}, 403},
		{"root cannot review", rootToken, "GET", "/v1/reviews", nil, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r := e.call(tc.token, tc.method, tc.path, tc.body, "k"); r.status != tc.want {
				t.Errorf("status %d, want %d (%v)", r.status, tc.want, r.body)
			}
		})
	}
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	e := newEnv(t)
	issued := e.must(201, e.call(rootToken, "POST", "/v1/admin/keys", map[string]any{"owner": "carol"}, ""))
	key := issued["key"].(string)
	e.must(200, e.call(key, "GET", "/v1/accounts", nil, ""))
	if r := e.call(rootToken, "DELETE", "/v1/admin/keys/"+issued["id"].(string), nil, ""); r.status != 204 {
		t.Fatalf("revoke: %d", r.status)
	}
	if r := e.call(key, "GET", "/v1/accounts", nil, ""); r.status != 401 {
		t.Errorf("revoked key: %d", r.status)
	}
}

func TestErrorMapping(t *testing.T) {
	e := newEnv(t)
	_, alice, bob := e.credentials()
	aID := e.must(201, e.call(alice, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, ""))["id"].(string)
	bID := e.must(201, e.call(bob, "POST", "/v1/accounts", map[string]any{"asset": "BTC"}, ""))["id"].(string)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		key    string
		want   int
	}{
		{"bad asset", "POST", "/v1/accounts", map[string]any{"asset": "btc"}, "", 400},
		{"unknown field", "POST", "/v1/accounts", map[string]any{"asset": "BTC", "owner": "bob"}, "", 400},
		{"bad id", "GET", "/v1/accounts/nope", nil, "", 400},
		{"no idempotency key", "POST", "/v1/transfers", map[string]any{"from": aID, "to": bID, "amount": 1}, "", 400},
		{"insufficient", "POST", "/v1/transfers", map[string]any{"from": aID, "to": bID, "amount": 1}, "x", 409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r := e.call(alice, tc.method, tc.path, tc.body, tc.key); r.status != tc.want {
				t.Errorf("status = %d, want %d (%v)", r.status, tc.want, r.body)
			}
		})
	}
}

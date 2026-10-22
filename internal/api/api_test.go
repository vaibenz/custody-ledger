package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vaibenz/custody-ledger/internal/api"
	"github.com/vaibenz/custody-ledger/internal/ledger"
	"github.com/vaibenz/custody-ledger/internal/testdb"
)

const adminToken = "test-admin-token"

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func (c client) do(method, path string, body any, headers map[string]string) (int, map[string]any) {
	c.t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, buf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newClient(t *testing.T) client {
	pool := testdb.New(t)
	h := api.New(ledger.NewStore(pool), adminToken, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client{t: t, srv: srv}
}

func key(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }

func TestEndToEndFlow(t *testing.T) {
	c := newClient(t)

	status, alice := c.do("POST", "/v1/accounts", map[string]any{"owner": "alice", "asset": "BTC"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create alice: %d %v", status, alice)
	}
	_, bob := c.do("POST", "/v1/accounts", map[string]any{"owner": "bob", "asset": "BTC"}, nil)
	aliceID, bobID := alice["id"].(string), bob["id"].(string)

	if status, body := c.do("POST", "/v1/deposits",
		map[string]any{"account_id": aliceID, "amount": 10_000}, key("d1")); status != http.StatusCreated {
		t.Fatalf("deposit: %d %v", status, body)
	}

	transfer := map[string]any{"from": aliceID, "to": bobID, "amount": 2_500}
	if status, body := c.do("POST", "/v1/transfers", transfer, key("t1")); status != http.StatusCreated {
		t.Fatalf("transfer: %d %v", status, body)
	}
	// Retry: 200 and replayed, not a second transfer.
	status, body := c.do("POST", "/v1/transfers", transfer, key("t1"))
	if status != http.StatusOK || body["replayed"] != true {
		t.Fatalf("retry: %d %v", status, body)
	}

	_, got := c.do("GET", "/v1/accounts/"+aliceID, nil, nil)
	if got["balance"].(float64) != 7_500 {
		t.Errorf("alice balance = %v", got["balance"])
	}

	// An address saved just now is held for review.
	addr := "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"
	if status, body := c.do("POST", "/v1/accounts/"+aliceID+"/addresses",
		map[string]any{"address": addr}, nil); status != http.StatusCreated {
		t.Fatalf("add address: %d %v", status, body)
	}
	status, wd := c.do("POST", "/v1/withdrawals",
		map[string]any{"account_id": aliceID, "address": addr, "amount": 1_000}, key("w1"))
	if status != http.StatusAccepted || wd["status"] != "pending_review" {
		t.Fatalf("withdrawal: %d %v", status, wd)
	}

	// Review endpoints need the admin token.
	if status, _ := c.do("GET", "/v1/reviews", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("reviews without token: %d", status)
	}
	auth := map[string]string{"Authorization": "Bearer " + adminToken}
	status, list := c.do("GET", "/v1/reviews", nil, auth)
	if status != http.StatusOK || len(list["withdrawals"].([]any)) != 1 {
		t.Fatalf("reviews: %d %v", status, list)
	}
	status, decided := c.do("POST", "/v1/reviews/"+wd["id"].(string)+"/approve",
		map[string]any{"reviewer": "ops"}, auth)
	if status != http.StatusOK || decided["status"] != "approved" {
		t.Fatalf("approve: %d %v", status, decided)
	}

	_, got = c.do("GET", "/v1/accounts/"+aliceID, nil, nil)
	if got["balance"].(float64) != 6_500 {
		t.Errorf("alice balance after withdrawal = %v", got["balance"])
	}
}

func TestErrorMapping(t *testing.T) {
	c := newClient(t)
	_, a := c.do("POST", "/v1/accounts", map[string]any{"owner": "alice", "asset": "BTC"}, nil)
	_, b := c.do("POST", "/v1/accounts", map[string]any{"owner": "bob", "asset": "BTC"}, nil)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		hdr    map[string]string
		want   int
	}{
		{"bad asset", "POST", "/v1/accounts", map[string]any{"owner": "x", "asset": "btc"}, nil, 400},
		{"unknown field", "POST", "/v1/accounts", map[string]any{"owner": "x", "asset": "BTC", "admin": true}, nil, 400},
		{"bad id", "GET", "/v1/accounts/nope", nil, nil, 400},
		{"missing account", "GET", "/v1/accounts/00000000-0000-0000-0000-000000000000", nil, nil, 404},
		{"no key", "POST", "/v1/transfers", map[string]any{"from": a["id"], "to": b["id"], "amount": 1}, nil, 400},
		{"insufficient", "POST", "/v1/transfers", map[string]any{"from": a["id"], "to": b["id"], "amount": 1}, key("x"), 409},
		{"wrong token", "GET", "/v1/reviews", nil, map[string]string{"Authorization": "Bearer nope"}, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := c.do(tc.method, tc.path, tc.body, tc.hdr); status != tc.want {
				t.Errorf("status = %d, want %d (%v)", status, tc.want, body)
			}
		})
	}
}

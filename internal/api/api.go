// Package api exposes the ledger over JSON HTTP.
//
// Every /v1 route needs "Authorization: Bearer <token>". What a caller may do
// depends on the kind of token (see package auth):
//
//	customer  open and read their own accounts, save addresses, transfer, withdraw
//	operator  credit deposits, review held withdrawals, read the audit log
//	root      create operators, issue and revoke customer keys
//
// A customer asking for someone else's account gets 404, not 403, so account
// ids cannot be probed.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/vaibenz/custody-ledger/internal/audit"
	"github.com/vaibenz/custody-ledger/internal/auth"
	"github.com/vaibenz/custody-ledger/internal/ledger"
)

type Server struct {
	store *ledger.Store
	auth  *auth.Store
	log   *slog.Logger
}

func New(store *ledger.Store, authStore *auth.Store, log *slog.Logger) http.Handler {
	s := &Server{store: store, auth: authStore, log: log}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.logRequests)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(10 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/v1", func(r chi.Router) {
		r.Use(s.authenticate)

		r.Group(func(r chi.Router) {
			r.Use(only(auth.KindCustomer))
			r.Post("/accounts", s.createAccount)
			r.Get("/accounts", s.listAccounts)
			r.Get("/accounts/{id}", s.getAccount)
			r.Get("/accounts/{id}/entries", s.listEntries)
			r.Post("/accounts/{id}/addresses", s.addAddress)
			r.Post("/transfers", s.transfer)
			r.Post("/withdrawals", s.requestWithdrawal)
		})

		r.With(only(auth.KindCustomer, auth.KindOperator)).Get("/withdrawals/{id}", s.getWithdrawal)

		r.Group(func(r chi.Router) {
			r.Use(only(auth.KindOperator))
			r.Post("/deposits", s.deposit)
			r.Get("/reviews", s.listReviews)
			r.Post("/reviews/{id}/approve", s.review(true))
			r.Post("/reviews/{id}/reject", s.review(false))
			r.Get("/audit/{subject}", s.auditTrail)
		})

		r.Group(func(r chi.Router) {
			r.Use(only(auth.KindRoot))
			r.Post("/admin/operators", s.createOperator)
			r.Post("/admin/keys", s.issueKey)
			r.Delete("/admin/keys/{id}", s.revokeKey)
		})
	})
	return r
}

type principalKey struct{}

func principal(ctx context.Context) auth.Principal {
	p, _ := ctx.Value(principalKey{}).(auth.Principal)
	return p
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, auth.ErrUnauthenticated.Error())
			return
		}
		p, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthenticated) {
				s.log.Error("authenticate", "err", err)
			}
			writeError(w, http.StatusUnauthorized, auth.ErrUnauthenticated.Error())
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		ctx = audit.WithActor(ctx, p.Actor())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func only(kinds ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := principal(r.Context())
			for _, k := range kinds {
				if p.Kind == k {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, "not allowed for this credential")
		})
	}
}

// ownAccount loads an account the calling customer owns. Anything else,
// including another customer's account, is reported as not found.
func (s *Server) ownAccount(ctx context.Context, id uuid.UUID) (ledger.Account, error) {
	acct, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return ledger.Account{}, err
	}
	if acct.Owner != principal(ctx).Name {
		return ledger.Account{}, ledger.ErrAccountNotFound
	}
	return acct, nil
}

// Customer endpoints.

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Asset string `json:"asset"`
	}
	if !decode(w, r, &req) {
		return
	}
	acct, err := s.store.CreateAccount(r.Context(), principal(r.Context()).Name, req.Asset)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, acct)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAccounts(r.Context(), principal(r.Context()).Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": list})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	acct, err := s.ownAccount(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.ownAccount(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.store.Entries(r.Context(), id, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) addAddress(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.ownAccount(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	a, err := s.store.AddAddress(r.Context(), id, req.Address)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) transfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From   uuid.UUID `json:"from"`
		To     uuid.UUID `json:"to"`
		Amount int64     `json:"amount"`
	}
	if !decode(w, r, &req) {
		return
	}
	// Only the source must belong to the caller; paying another customer
	// is the point of a transfer.
	if _, err := s.ownAccount(r.Context(), req.From); err != nil {
		s.fail(w, r, err)
		return
	}
	txn, err := s.store.Transfer(r.Context(), req.From, req.To, req.Amount, idempotencyKey(r))
	s.writeTxn(w, r, txn, err)
}

func (s *Server) requestWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID uuid.UUID `json:"account_id"`
		Address   string    `json:"address"`
		Amount    int64     `json:"amount"`
	}
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.ownAccount(r.Context(), req.AccountID); err != nil {
		s.fail(w, r, err)
		return
	}
	wd, err := s.store.RequestWithdrawal(r.Context(), req.AccountID, req.Address, req.Amount, idempotencyKey(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	switch {
	case wd.Replayed:
		status = http.StatusOK
	case wd.Status == ledger.StatusPendingReview:
		status = http.StatusAccepted
	case wd.Status == ledger.StatusRejected:
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, wd)
}

func (s *Server) getWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	wd, err := s.store.GetWithdrawal(r.Context(), id)
	if err == nil && principal(r.Context()).Kind == auth.KindCustomer {
		_, err = s.ownAccount(r.Context(), wd.AccountID)
		if errors.Is(err, ledger.ErrAccountNotFound) {
			err = ledger.ErrWithdrawalNotFound
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, wd)
}

// Operator endpoints.

// deposit credits a customer when funds arrive on chain. In production this
// is called by the chain watcher service under its own operator identity.
func (s *Server) deposit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID uuid.UUID `json:"account_id"`
		Amount    int64     `json:"amount"`
	}
	if !decode(w, r, &req) {
		return
	}
	txn, err := s.store.Deposit(r.Context(), req.AccountID, req.Amount, idempotencyKey(r))
	s.writeTxn(w, r, txn, err)
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.store.PendingReviews(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"withdrawals": list})
}

// review records the calling operator's decision. The reviewer is taken
// from the credential, never from the request body.
func (s *Server) review(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		wd, err := s.store.ReviewWithdrawal(r.Context(), id, approve, principal(r.Context()).Name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, wd)
	}
}

func (s *Server) auditTrail(w http.ResponseWriter, r *http.Request) {
	entries, err := audit.ForSubject(r.Context(), s.store.DB(), chi.URLParam(r, "subject"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// Root endpoints.

func (s *Server) createOperator(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	token, err := s.auth.CreateOperator(r.Context(), req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "token": token})
}

func (s *Server) issueKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner string `json:"owner"`
	}
	if !decode(w, r, &req) {
		return
	}
	key, id, err := s.auth.IssueKey(r.Context(), req.Owner)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "owner": req.Owner, "key": key})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.auth.RevokeKey(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Helpers.

// writeTxn answers 201 for a new transaction and 200 for a replay, so a
// client can tell a retry from a first attempt.
func (s *Server) writeTxn(w http.ResponseWriter, r *http.Request, txn ledger.Transaction, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if txn.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, txn)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()))
	})
}

// fail maps domain errors to HTTP statuses. Anything unexpected is logged
// and returned as a generic 500 so internals never leak to clients.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound),
		errors.Is(err, ledger.ErrWithdrawalNotFound),
		errors.Is(err, auth.ErrKeyNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ledger.ErrInsufficientFunds),
		errors.Is(err, ledger.ErrIdempotencyKeyConflict),
		errors.Is(err, ledger.ErrNotPendingReview),
		errors.Is(err, auth.ErrNameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ledger.ErrInvalidAmount),
		errors.Is(err, ledger.ErrSameAccount),
		errors.Is(err, ledger.ErrAssetMismatch),
		errors.Is(err, ledger.ErrMissingIdempotencyKey),
		errors.Is(err, ledger.ErrInvalidAsset),
		errors.Is(err, ledger.ErrInvalidOwner),
		errors.Is(err, ledger.ErrInvalidAddress),
		errors.Is(err, ledger.ErrReviewerRequired),
		errors.Is(err, auth.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("internal error", "err", err, "request_id", middleware.GetReqID(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func idempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

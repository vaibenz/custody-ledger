// Package api exposes the ledger over JSON HTTP.
package api

import (
	"crypto/subtle"
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

	"github.com/vaibenz/custody-ledger/internal/ledger"
)

type Server struct {
	store      *ledger.Store
	adminToken string
	log        *slog.Logger
}

// New builds the router. Review endpoints require
// "Authorization: Bearer <adminToken>"; with an empty token they are off.
func New(store *ledger.Store, adminToken string, log *slog.Logger) http.Handler {
	s := &Server{store: store, adminToken: adminToken, log: log}

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
		r.Post("/accounts", s.createAccount)
		r.Get("/accounts/{id}", s.getAccount)
		r.Get("/accounts/{id}/entries", s.listEntries)
		r.Post("/accounts/{id}/addresses", s.addAddress)
		r.Post("/deposits", s.deposit)
		r.Post("/transfers", s.transfer)
		r.Post("/withdrawals", s.requestWithdrawal)
		r.Get("/withdrawals/{id}", s.getWithdrawal)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAdmin)
			r.Get("/reviews", s.listReviews)
			r.Post("/reviews/{id}/approve", s.review(true))
			r.Post("/reviews/{id}/reject", s.review(false))
		})
	})
	return r
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner string `json:"owner"`
		Asset string `json:"asset"`
	}
	if !decode(w, r, &req) {
		return
	}
	acct, err := s.store.CreateAccount(r.Context(), req.Owner, req.Asset)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, acct)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	acct, err := s.store.GetAccount(r.Context(), id)
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
	if _, err := s.store.GetAccount(r.Context(), id); err != nil {
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
	a, err := s.store.AddAddress(r.Context(), id, req.Address)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

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

func (s *Server) transfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From   uuid.UUID `json:"from"`
		To     uuid.UUID `json:"to"`
		Amount int64     `json:"amount"`
	}
	if !decode(w, r, &req) {
		return
	}
	txn, err := s.store.Transfer(r.Context(), req.From, req.To, req.Amount, idempotencyKey(r))
	s.writeTxn(w, r, txn, err)
}

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

func (s *Server) requestWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID uuid.UUID `json:"account_id"`
		Address   string    `json:"address"`
		Amount    int64     `json:"amount"`
	}
	if !decode(w, r, &req) {
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
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, wd)
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

func (s *Server) review(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var req struct {
			Reviewer string `json:"reviewer"`
		}
		if !decode(w, r, &req) {
			return
		}
		wd, err := s.store.ReviewWithdrawal(r.Context(), id, approve, req.Reviewer)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, wd)
	}
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.adminToken == "" || !found ||
			subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
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
	case errors.Is(err, ledger.ErrAccountNotFound), errors.Is(err, ledger.ErrWithdrawalNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ledger.ErrInsufficientFunds),
		errors.Is(err, ledger.ErrIdempotencyKeyConflict),
		errors.Is(err, ledger.ErrNotPendingReview):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ledger.ErrInvalidAmount),
		errors.Is(err, ledger.ErrSameAccount),
		errors.Is(err, ledger.ErrAssetMismatch),
		errors.Is(err, ledger.ErrMissingIdempotencyKey),
		errors.Is(err, ledger.ErrInvalidAsset),
		errors.Is(err, ledger.ErrInvalidOwner),
		errors.Is(err, ledger.ErrInvalidAddress),
		errors.Is(err, ledger.ErrReviewerRequired):
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

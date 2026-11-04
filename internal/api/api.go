// Package api is Ferry's public HTTP API (v1). Handlers are thin: they
// authenticate, decode and validate input, call the domain services, and
// map their errors to HTTP statuses.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/idempotency"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
)

// Config wires the API to its dependencies.
type Config struct {
	Pool    *pgxpool.Pool
	Payouts *payout.Service
	Idem    *idempotency.Store
	House   ledger.HouseAccounts
	// AdminKey authorises operator routes (creating customers, dev
	// deposits). Compared in constant time; must be long and random.
	AdminKey string
	// Dev enables /v1/dev routes that move money without a real deposit.
	Dev bool
	Log *slog.Logger
}

type server struct {
	Config
	adminHash [32]byte
}

// New returns the API's HTTP handler.
func New(cfg Config) http.Handler {
	s := &server{Config: cfg, adminHash: sha256.Sum256([]byte(cfg.AdminKey))}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.logRequests, middleware.Recoverer)

	r.Route("/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) { // operator
			r.Use(s.requireAdmin)
			r.Post("/customers", s.createCustomer)
			if cfg.Dev {
				r.Post("/dev/deposits", s.devDeposit)
			}
		})
		r.Group(func(r chi.Router) { // customer
			r.Use(s.requireCustomer)
			r.Get("/customers/{id}/balances", s.balances)
			r.Get("/payouts/{id}", s.getPayout)
			r.Group(func(r chi.Router) {
				r.Use(cfg.Idem.Middleware(customerOf, writeError))
				r.Post("/payouts", s.createPayout)
				r.Post("/payouts/{id}/confirm", s.confirmPayout)
			})
		})
	})
	return r
}

// ---- auth ----

type ctxKey struct{}

func customerOf(r *http.Request) uuid.UUID {
	id, _ := r.Context().Value(ctxKey{}).(uuid.UUID)
	return id
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return t
	}
	return ""
}

func (s *server) requireCustomer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := customer.Authenticate(r.Context(), s.Pool, bearer(r))
		if err != nil {
			if !errors.Is(err, customer.ErrUnauthorized) {
				s.Log.ErrorContext(r.Context(), "authenticate", "error", err)
			}
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

func (s *server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Compare hashes in constant time, so response timing reveals
		// nothing about the key.
		got := sha256.Sum256([]byte(bearer(r)))
		if s.AdminKey == "" || subtle.ConstantTimeCompare(got[:], s.adminHash[:]) != 1 {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- logging ----

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.Log.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
			"idempotency_key", r.Header.Get(idempotency.Header))
	})
}

// ---- JSON ----

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may have gone; nothing to do
}

func writeError(w http.ResponseWriter, _ *http.Request, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

// decode reads exactly one JSON object into v, rejecting unknown fields
// and trailing data.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, idempotency.MaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	if dec.More() {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "trailing data after the JSON object")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *server) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.Log.ErrorContext(r.Context(), what, "error", err, "request_id", middleware.GetReqID(r.Context()))
	writeError(w, r, http.StatusInternalServerError, "internal", fmt.Sprintf("internal error (request %s)", middleware.GetReqID(r.Context())))
}

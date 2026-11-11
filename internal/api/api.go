// Package api is Ferry's public HTTP API (v1). Handlers are thin: they
// authenticate, decode and validate input, call the domain services, and
// map their errors to HTTP statuses.
package api

import (
	"strconv"

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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/destination"
	"github.com/darkodi/ferry/internal/idempotency"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/obs"
	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/treasury"
)

// Config wires the API to its dependencies.
type Config struct {
	Pool    *pgxpool.Pool
	Payouts *payout.Service
	// Destinations stores customers' bank accounts (needs the data key).
	Destinations *destination.Store
	Idem         *idempotency.Store
	House        ledger.HouseAccounts
	// Addresses assigns deposit addresses to new customers. Nil assigns
	// none (no chain configured).
	Addresses *chain.Addresses
	// AdminKey authorises operator routes (creating customers, dev
	// deposits). Compared in constant time; must be long and random.
	AdminKey string
	// Dev enables /v1/dev routes that move money without a real deposit.
	Dev bool
	// RailWebhooks verifies the payout rail's webhooks; nil serves no
	// webhook route (the mock rail sends none).
	RailWebhooks RailWebhooks
	// Treasury books treasury top-ups; nil serves no top-up route (the
	// mock rail has no balance to top up).
	Treasury *treasury.Service
	Log      *slog.Logger
}

type server struct {
	Config
	adminHash [32]byte
}

// RailWebhooks verifies and parses one rail's webhooks
// (noah.Webhooks).
type RailWebhooks interface {
	Name() string
	Parse(body []byte, header http.Header) (rail.Event, error)
}

// New returns the API's HTTP handler.
func New(cfg Config) http.Handler {
	s := &server{Config: cfg, adminHash: sha256.Sum256([]byte(cfg.AdminKey))}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.logRequests, middleware.Recoverer)

	r.Route("/v1", func(r chi.Router) {
		// The rail's webhooks: no customer or admin key, the body's
		// signature is the authentication.
		if cfg.RailWebhooks != nil {
			r.Post("/webhooks/{rail}", s.railWebhook)
		}
		r.Group(func(r chi.Router) { // operator
			r.Use(s.requireAdmin)
			r.Post("/customers", s.createCustomer)
			r.Get("/admin/recon-breaks", s.listBreaks)
			r.Post("/admin/recon-breaks/{id}/resolve", s.resolveBreak)
			if cfg.Treasury != nil {
				r.Post("/admin/treasury-topups", s.recordTopup)
			}
			if cfg.Dev {
				r.Post("/dev/deposits", s.devDeposit)
			}
		})
		r.Group(func(r chi.Router) { // customer
			r.Use(s.requireCustomer)
			r.Get("/customers/{id}/balances", s.balances)
			r.Get("/customers/{id}/deposit-addresses", s.depositAddresses)
			r.Get("/customers/{id}/deposits", s.deposits)
			r.Get("/payouts/{id}", s.getPayout)
			r.Get("/destinations", s.listDestinations)
			r.Group(func(r chi.Router) {
				r.Use(cfg.Idem.Middleware(customerOf, writeError))
				r.Post("/destinations", s.createDestination)
				r.Post("/payouts", s.createPayout)
				r.Post("/payouts/{id}/confirm", s.confirmPayout)
			})
		})
	})
	return withHealth(r, cfg.Pool)
}

// withHealth answers GET /healthz for the load balancer ahead of the
// router, so health checks are not logged as requests: 200 when the
// database answers within two seconds, 503 otherwise. It reveals nothing
// else.
func withHealth(next http.Handler, pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if pool != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := pool.Ping(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
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

// logRequests traces, measures and logs every request. Each request starts
// a new trace: a client's traceparent is ignored, because with parent-based
// sampling a caller could otherwise mark its own requests "not sampled"
// and keep them out of traces. Metrics use the route pattern
// (/v1/payouts/{id}), never the raw path, so ids do not become labels.
func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := obs.Tracer.Start(r.Context(), "HTTP "+r.Method,
			trace.WithSpanKind(trace.SpanKindServer), trace.WithNewRoot())
		defer span.End()
		r = r.WithContext(ctx)
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		span.SetName("HTTP " + r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.status_code", ww.Status()))
		obs.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(ww.Status())).Inc()
		obs.HTTPDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
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

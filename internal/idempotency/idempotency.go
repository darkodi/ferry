// Package idempotency implements the Idempotency-Key protocol for mutating
// API endpoints:
//
//   - same key, same request: the stored response is returned;
//   - same key, different request: 422;
//   - same key while the first request still runs: 409.
//
// A key is claimed with a lease (locked_until). If the process dies before
// storing a response, the lease runs out and a retry runs the request
// again; the business tables have their own unique keys (for example
// payouts.idempotency_key), so a rerun finds the work already done instead
// of repeating it.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/idempotency/idempotencydb"
	"github.com/darkodi/ferry/internal/platform/db"
)

// Header is the request header carrying the key.
const Header = "Idempotency-Key"

// MaxBody is the largest request body the middleware will hash.
const MaxBody = 64 << 10

// Outcome is what Begin decided.
type Outcome int

// Begin outcomes.
const (
	// Proceed: the caller owns the key and must Complete or Unlock it.
	Proceed Outcome = iota
	// Replay: a response is stored; return it.
	Replay
	// Mismatch: the key was used for a different request.
	Mismatch
	// InFlight: another request with this key is still running.
	InFlight
)

// Result is Begin's answer. Status and Body are set for Replay.
type Result struct {
	Outcome Outcome
	Status  int
	Body    []byte
}

// Store keeps idempotency keys in Postgres.
type Store struct {
	pool  *pgxpool.Pool
	now   func() time.Time
	lease time.Duration
	log   *slog.Logger
}

// NewStore returns a Store. lease is how long a running request holds its
// key before a retry may take over.
func NewStore(pool *pgxpool.Pool, now func() time.Time, lease time.Duration, log *slog.Logger) *Store {
	return &Store{pool: pool, now: now, lease: lease, log: log}
}

// Begin claims a key for a request with the given hash, or reports why the
// caller must not run it.
func (s *Store) Begin(ctx context.Context, customerID uuid.UUID, key string, hash []byte) (Result, error) {
	var res Result
	until := s.now().Add(s.lease)
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := idempotencydb.New(tx)
		_, err := q.ClaimNew(ctx, idempotencydb.ClaimNewParams{
			CustomerID: customerID, Key: key, RequestHash: hash, LockedUntil: &until,
		})
		if err == nil {
			res = Result{Outcome: Proceed}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("idempotency: claim: %w", err)
		}
		// The key exists. Lock its row so two retries cannot both take over
		// an expired lease.
		row, err := q.GetForUpdate(ctx, idempotencydb.GetForUpdateParams{CustomerID: customerID, Key: key})
		if err != nil {
			return fmt.Errorf("idempotency: read: %w", err)
		}
		switch {
		case !bytes.Equal(row.RequestHash, hash):
			res = Result{Outcome: Mismatch}
		case row.ResponseStatus.Valid:
			res = Result{Outcome: Replay, Status: int(row.ResponseStatus.Int32), Body: row.ResponseBody}
		case row.LockedUntil != nil && row.LockedUntil.After(s.now()):
			res = Result{Outcome: InFlight}
		default: // the earlier request gave up or died: take over
			if err := q.Relock(ctx, idempotencydb.RelockParams{CustomerID: customerID, Key: key, LockedUntil: &until}); err != nil {
				return fmt.Errorf("idempotency: relock: %w", err)
			}
			res = Result{Outcome: Proceed}
		}
		return nil
	})
	return res, err
}

// Complete stores the response for a key claimed with Begin.
func (s *Store) Complete(ctx context.Context, customerID uuid.UUID, key string, status int, body []byte) error {
	err := idempotencydb.New(s.pool).Complete(ctx, idempotencydb.CompleteParams{
		CustomerID: customerID, Key: key, ResponseBody: body,
		ResponseStatus: pgtype.Int4{Int32: int32(status), Valid: true}, //nolint:gosec // HTTP status codes fit in int32
	})
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	return nil
}

// Unlock releases a key without storing a response, so the client may
// retry at once (used after a server error).
func (s *Store) Unlock(ctx context.Context, customerID uuid.UUID, key string) error {
	if err := idempotencydb.New(s.pool).Unlock(ctx, idempotencydb.UnlockParams{CustomerID: customerID, Key: key}); err != nil {
		return fmt.Errorf("idempotency: unlock: %w", err)
	}
	return nil
}

// Hash identifies a request: method, path and body. The same key reused on
// another endpoint therefore counts as a different request.
func Hash(method, path string, body []byte) []byte {
	h := sha256.New()
	_, _ = io.WriteString(h, method+"\n"+path+"\n") // hash.Hash writes never fail
	_, _ = h.Write(body)
	return h.Sum(nil)
}

// Middleware enforces the protocol on a route. customerOf returns the
// authenticated customer; keys are scoped to them. Responses below 500 are
// stored and replayed; after a 5xx the key is unlocked so the client can
// retry.
func (s *Store) Middleware(customerOf func(*http.Request) uuid.UUID, onError func(http.ResponseWriter, *http.Request, int, string, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(Header)
			if key == "" || len(key) > 200 {
				onError(w, r, http.StatusBadRequest, "idempotency_key_required", "send an Idempotency-Key header of 1-200 characters")
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
			if err != nil {
				onError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
				return
			}
			customerID := customerOf(r)
			res, err := s.Begin(r.Context(), customerID, key, Hash(r.Method, r.URL.Path, body))
			if err != nil {
				onError(w, r, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			switch res.Outcome {
			case Replay:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(res.Status)
				_, _ = w.Write(res.Body)
				return
			case Mismatch:
				onError(w, r, http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was used for a different request")
				return
			case InFlight:
				w.Header().Set("Retry-After", "1")
				onError(w, r, http.StatusConflict, "request_in_progress", "a request with this Idempotency-Key is still running")
				return
			case Proceed:
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			rec := &recorder{header: w.Header(), status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// Store the outcome even if the client has gone away: the work
			// is done, and its retry must see the result.
			ctx := context.WithoutCancel(r.Context())
			// A failure here is logged, not returned: the response is still
			// correct, and the lease expiry lets a retry run the handler
			// again, which finds the work already done.
			if rec.status >= 500 {
				err = s.Unlock(ctx, customerID, key)
			} else {
				err = s.Complete(ctx, customerID, key, rec.status, rec.body.Bytes())
			}
			if err != nil {
				s.log.ErrorContext(ctx, "store idempotent response", "idempotency_key", key, "error", err)
			}
			w.WriteHeader(rec.status)
			_, _ = w.Write(rec.body.Bytes())
		})
	}
}

// recorder buffers a handler's response so it can be stored before it is
// sent.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) } //nolint:wrapcheck // bytes.Buffer never fails

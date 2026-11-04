package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/db"
)

// Amounts go over the wire as decimal strings ("101.500000"), never as
// JSON numbers, so no client parses them into floats.

// ---- customers ----

type createCustomerReq struct {
	Name string `json:"name"`
}

type customerResp struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	APIKey    string    `json:"api_key,omitempty"` // only in the create response
	CreatedAt time.Time `json:"created_at"`
}

func (s *server) createCustomer(w http.ResponseWriter, r *http.Request) {
	var req createCustomerReq
	if !decode(w, r, &req) {
		return
	}
	c, key, err := customer.Create(r.Context(), s.Pool, req.Name)
	if errors.Is(err, customer.ErrInvalidName) {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request", err.Error())
		return
	}
	if err != nil {
		s.internal(w, r, "create customer", err)
		return
	}
	writeJSON(w, http.StatusCreated, customerResp{ID: c.ID, Name: c.Name, APIKey: key, CreatedAt: c.CreatedAt})
}

type balancesResp struct {
	Currency  ledger.Currency `json:"currency"`
	Available string          `json:"available"`
	Held      string          `json:"held"`
}

func (s *server) balances(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if id != customerOf(r) { // never reveal whether another customer exists
		writeError(w, r, http.StatusNotFound, "not_found", "not found")
		return
	}
	resp, err := s.loadBalances(r, id)
	if err != nil {
		s.internal(w, r, "balances", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) loadBalances(r *http.Request, id uuid.UUID) (balancesResp, error) {
	accts, err := ledger.FindCustomerAccounts(r.Context(), s.Pool, id)
	if err != nil {
		return balancesResp{}, err
	}
	avail, err := ledger.Balance(r.Context(), s.Pool, accts.Available)
	if err != nil {
		return balancesResp{}, err
	}
	held, err := ledger.Balance(r.Context(), s.Pool, accts.Held)
	if err != nil {
		return balancesResp{}, err
	}
	return balancesResp{Currency: ledger.USDC, Available: avail.String(), Held: held.String()}, nil
}

// ---- payouts ----

type createPayoutReq struct {
	FiatAmount   int64  `json:"fiat_amount"` // minor units, e.g. cents
	FiatCurrency string `json:"fiat_currency"`
	Destination  string `json:"destination"`
}

type quoteResp struct {
	CryptoAuthorized string    `json:"crypto_authorized"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type payoutResp struct {
	ID              uuid.UUID  `json:"id"`
	Status          string     `json:"status"`
	FiatAmount      int64      `json:"fiat_amount"`
	FiatCurrency    string     `json:"fiat_currency"`
	Destination     string     `json:"destination"`
	Quote           *quoteResp `json:"quote,omitempty"`
	NeedsResolution bool       `json:"needs_resolution"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func toResp(p payout.Payout) payoutResp {
	out := payoutResp{
		ID: p.ID, Status: string(p.Status),
		FiatAmount: p.FiatAmount, FiatCurrency: p.FiatCurrency, Destination: p.Destination,
		NeedsResolution: p.NeedsResolution, FailureReason: p.FailureReason,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if p.Quote != nil {
		out.Quote = &quoteResp{CryptoAuthorized: p.Quote.CryptoAuthorized.String(), ExpiresAt: p.Quote.ExpiresAt}
	}
	return out
}

func (s *server) createPayout(w http.ResponseWriter, r *http.Request) {
	var req createPayoutReq
	if !decode(w, r, &req) {
		return
	}
	p, err := s.Payouts.Create(r.Context(), payout.CreateRequest{
		CustomerID:     customerOf(r),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		FiatAmount:     req.FiatAmount,
		FiatCurrency:   req.FiatCurrency,
		Destination:    req.Destination,
	})
	switch {
	case errors.Is(err, payout.ErrInvalidRequest):
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request", err.Error())
	case errors.Is(err, payout.ErrKeyReused):
		writeError(w, r, http.StatusUnprocessableEntity, "idempotency_key_reused", err.Error())
	case err != nil:
		s.internal(w, r, "create payout", err)
	default:
		writeJSON(w, http.StatusCreated, toResp(p))
	}
}

func (s *server) getPayout(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := s.Payouts.Get(r.Context(), customerOf(r), id)
	switch {
	case errors.Is(err, payout.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "not found")
	case err != nil:
		s.internal(w, r, "get payout", err)
	default:
		writeJSON(w, http.StatusOK, toResp(p))
	}
}

// confirmPayout places the hold and, until M3 moves this to a worker,
// submits to the rail and applies its answer in the same request.
func (s *server) confirmPayout(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := s.Payouts.Confirm(r.Context(), customerOf(r), id)
	switch {
	case errors.Is(err, payout.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "not found")
		return
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(w, r, http.StatusUnprocessableEntity, "insufficient_funds", "available balance does not cover the quote")
		return
	case errors.Is(err, payout.ErrQuoteExpired):
		writeError(w, r, http.StatusUnprocessableEntity, "quote_expired", "the quote expired; create a new payout")
		return
	case errors.Is(err, payout.ErrNotConfirmable):
		writeError(w, r, http.StatusConflict, "not_confirmable", err.Error())
		return
	case err != nil:
		s.internal(w, r, "confirm payout", err)
		return
	}
	// The hold is committed. A submit problem does not undo it: the payout
	// stays held (with needs_resolution if the outcome is unknown) and the
	// response shows that state.
	if sp, err := s.Payouts.Submit(r.Context(), p.ID); err != nil {
		s.Log.ErrorContext(r.Context(), "submit payout", "payout_id", p.ID, "error", err)
	} else {
		p = sp
	}
	writeJSON(w, http.StatusOK, toResp(p))
}

// ---- dev ----

type devDepositReq struct {
	CustomerID uuid.UUID `json:"customer_id"`
	Amount     string    `json:"amount"` // USDC, e.g. "100.50"
}

// devDeposit credits a customer as if a chain deposit had confirmed. It
// exists only when Dev is set, until the chain watchers arrive in M4.
func (s *server) devDeposit(w http.ResponseWriter, r *http.Request) {
	var req devDepositReq
	if !decode(w, r, &req) {
		return
	}
	amount, err := ledger.ParseUSDC(req.Amount)
	if err != nil || amount <= 0 {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request", "amount must be a positive USDC decimal")
		return
	}
	accts, err := ledger.FindCustomerAccounts(r.Context(), s.Pool, req.CustomerID)
	if errors.Is(err, ledger.ErrUnknownAccount) {
		writeError(w, r, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	if err != nil {
		s.internal(w, r, "dev deposit", err)
		return
	}
	e := ledger.Deposit("dev", uuid.NewString(), 0, uuid.New(), s.House, accts, amount)
	err = db.WithTx(r.Context(), s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := ledger.PostEntry(r.Context(), tx, e)
		return err
	})
	if err != nil {
		s.internal(w, r, "dev deposit", err)
		return
	}
	resp, err := s.loadBalances(r, req.CustomerID)
	if err != nil {
		s.internal(w, r, "dev deposit", err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

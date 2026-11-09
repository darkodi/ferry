// Package noah is Ferry's rail adapter for the Noah Business API
// (https://docs.noah.com). Ferry is a client of Noah: it holds a
// pre-funded USDC balance there, and each payout is a sell transaction
// against that balance, paid out to a customer's bank account.
//
// A payout takes four calls:
//
//  1. GET  /channels/sell              pick the channel for the bank account
//  2. POST /transactions/sell/prepare  price it; Noah validates the bank form
//  3. POST /transactions/sell          execute it (Nonce makes retries safe)
//  4. GET  /transactions/{id}          follow it to Settled or Failed
//
// Everything Noah-specific (field names, statuses, error types, amount
// strings, request signing) stays in this package; the payout core sees
// only rail.Rail.
package noah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/rail"
)

// Config configures the adapter.
type Config struct {
	// CryptoCurrency is what Ferry sells: USDC_TEST in sandbox, USDC in
	// production.
	CryptoCurrency string
	// QuoteTTL is how long Ferry honours a prepared price. Noah's prepare
	// returns no expiry, and its authorized amount already carries a buffer
	// for price moves, so this is Ferry's own limit (default 2 minutes).
	QuoteTTL time.Duration
	// LookbackPages bounds how many pages of GET /transactions a lookup by
	// ExternalID reads (default 10, of 100 transactions each).
	LookbackPages int
}

// Rail implements rail.Rail against Noah.
type Rail struct {
	c   *Client
	cfg Config
	now func() time.Time
}

var _ rail.Rail = (*Rail)(nil)

// New returns the adapter.
func New(c *Client, cfg Config, now func() time.Time) (*Rail, error) {
	if cfg.CryptoCurrency == "" {
		return nil, errors.New("noah: crypto currency is required (USDC_TEST in sandbox)")
	}
	if cfg.QuoteTTL <= 0 {
		cfg.QuoteTTL = 2 * time.Minute
	}
	if cfg.LookbackPages <= 0 {
		cfg.LookbackPages = 10
	}
	return &Rail{c: c, cfg: cfg, now: now}, nil
}

// Name implements rail.Rail.
func (*Rail) Name() string { return "noah" }

// ---- wire types (only the fields Ferry uses) ----

type channel struct {
	ID                string `json:"ID"`
	PaymentMethodType string `json:"PaymentMethodType"`
	FiatCurrency      string `json:"FiatCurrency"`
	Country           string `json:"Country"`
	Limits            struct {
		MinLimit string `json:"MinLimit"`
		MaxLimit string `json:"MaxLimit"`
	} `json:"Limits"`
}

type channelsResp struct {
	Items []channel `json:"Items"`
}

type prepareReq struct {
	ChannelID       string          `json:"ChannelID"`
	CryptoCurrency  string          `json:"CryptoCurrency"`
	FiatAmount      string          `json:"FiatAmount"`
	Form            json.RawMessage `json:"Form,omitempty"`
	PaymentMethodID string          `json:"PaymentMethodID,omitempty"`
}

type prepareResp struct {
	CryptoAuthorizedAmount string          `json:"CryptoAuthorizedAmount"`
	CryptoAmountEstimate   string          `json:"CryptoAmountEstimate"`
	FormSessionID          string          `json:"FormSessionID"`
	TotalFee               string          `json:"TotalFee"`
	NextStep               json.RawMessage `json:"NextStep"`
}

type sellReq struct {
	CryptoCurrency         string `json:"CryptoCurrency"`
	FiatAmount             string `json:"FiatAmount"`
	CryptoAuthorizedAmount string `json:"CryptoAuthorizedAmount"`
	FormSessionID          string `json:"FormSessionID"`
	Nonce                  string `json:"Nonce"`
	ExternalID             string `json:"ExternalID"`
}

// Transaction is Noah's transaction object, as returned by the sell, get
// and list endpoints and carried in Transaction webhooks.
type Transaction struct {
	ID             string    `json:"ID"`
	Created        time.Time `json:"Created"`
	Status         string    `json:"Status"`    // Pending, Settled, Failed
	SubStatus      string    `json:"SubStatus"` // AmlScreening, UnderReview, Submitted, Confirming
	Direction      string    `json:"Direction"` // In, Out
	ExternalID     string    `json:"ExternalID"`
	Amount         string    `json:"Amount"`
	CryptoCurrency string    `json:"CryptoCurrency"`
	Reverses       string    `json:"Reverses"`
	RFI            *struct {
		Status string `json:"Status"`
		Type   string `json:"Type"`
	} `json:"RFI"`
	Breakdown []struct {
		Type   string `json:"Type"` // ChannelFee, BusinessFee, Remaining
		Amount string `json:"Amount"`
	} `json:"Breakdown"`
	FiatPaymentMethod *struct {
		ID string `json:"ID"`
	} `json:"FiatPaymentMethod"`
}

type txResp struct {
	Transaction Transaction `json:"Transaction"`
}

type txListResp struct {
	Items     []Transaction `json:"Items"`
	PageToken string        `json:"PageToken"`
}

// ---- Prepare ----

// Prepare implements rail.Rail: it picks the channel for the destination's
// payment method and asks Noah to price the payout. Noah validates the
// bank form here; a refusal (bad form, unsupported method, amount outside
// the channel's limits) is rail.ErrRejected, so the payout fails cleanly.
func (r *Rail) Prepare(ctx context.Context, req rail.QuoteRequest) (rail.Quote, error) {
	d := req.Destination
	fiat, err := formatFiat(req.FiatAmount, req.FiatCurrency)
	if err != nil {
		return rail.Quote{}, fmt.Errorf("%w: %w", rail.ErrRejected, err)
	}
	ch, err := r.channel(ctx, d, req.FiatCurrency, req.FiatAmount)
	if err != nil {
		return rail.Quote{}, err
	}
	in := prepareReq{
		ChannelID:      ch.ID,
		CryptoCurrency: r.cfg.CryptoCurrency,
		FiatAmount:     fiat,
	}
	if d.PaymentMethodID != "" {
		in.PaymentMethodID = d.PaymentMethodID // Noah asks only for what it lacks
	} else {
		in.Form = d.Form
	}
	var out prepareResp
	if err := r.c.do(ctx, http.MethodPost, "/transactions/sell/prepare", nil, in, &out); err != nil {
		return rail.Quote{}, classifyPrepare(err)
	}
	if len(out.NextStep) > 0 && string(out.NextStep) != "null" {
		// Noah needs another form step (e.g. an acknowledgement or more
		// fields); a saved destination cannot answer it.
		return rail.Quote{}, fmt.Errorf("%w: the bank details are incomplete for this channel (Noah asks for another form step)", rail.ErrRejected)
	}
	authorized, err := parseAmount(out.CryptoAuthorizedAmount)
	if err != nil {
		return rail.Quote{}, err
	}
	if authorized <= 0 || out.FormSessionID == "" {
		return rail.Quote{}, fmt.Errorf("noah: prepare returned authorized %q and form session %q", out.CryptoAuthorizedAmount, out.FormSessionID)
	}
	return rail.Quote{
		RailQuoteID:      out.FormSessionID,
		FiatAmount:       req.FiatAmount,
		FiatCurrency:     req.FiatCurrency,
		CryptoAuthorized: authorized,
		ExpiresAt:        r.now().Add(r.cfg.QuoteTTL),
	}, nil
}

// channel reads the sell channels for the destination's country and
// currency (never cached: Noah's channel ids and limits change) and
// returns the one for its payment method.
func (r *Rail) channel(ctx context.Context, d rail.Destination, currency string, minor int64) (channel, error) {
	q := url.Values{}
	q.Set("Country", d.Country)
	q.Set("CryptoCurrency", r.cfg.CryptoCurrency)
	q.Set("FiatCurrency", currency)
	var out channelsResp
	if err := r.c.do(ctx, http.MethodGet, "/channels/sell", q, nil, &out); err != nil {
		return channel{}, classifyPrepare(err)
	}
	for _, ch := range out.Items {
		if ch.PaymentMethodType != d.PaymentMethodType || ch.ID == "" {
			continue
		}
		if minLim, ok := parseFiat(ch.Limits.MinLimit, currency); ok && minor < minLim {
			return channel{}, fmt.Errorf("%w: amount below the %s channel minimum of %s %s", rail.ErrRejected, ch.PaymentMethodType, ch.Limits.MinLimit, currency)
		}
		if maxLim, ok := parseFiat(ch.Limits.MaxLimit, currency); ok && maxLim > 0 && minor > maxLim {
			return channel{}, fmt.Errorf("%w: amount above the %s channel maximum of %s %s", rail.ErrRejected, ch.PaymentMethodType, ch.Limits.MaxLimit, currency)
		}
		return ch, nil
	}
	return channel{}, fmt.Errorf("%w: no %s payout channel for %s in %s", rail.ErrRejected, d.PaymentMethodType, currency, d.Country)
}

// classifyPrepare: a 4xx means Noah refused this payout as asked (the
// payout fails); anything else may be temporary (the payout stays
// created, and retrying the create resumes the quote).
func classifyPrepare(err error) error {
	switch st := statusOf(err); {
	case st == http.StatusUnauthorized || st == http.StatusConflict || st == http.StatusTooManyRequests:
		return err // configuration or a retryable conflict: not the customer's fault
	case st >= 400 && st < 500:
		return fmt.Errorf("%w: %w", rail.ErrRejected, err)
	default:
		return err
	}
}

// ---- Submit ----

// Submit implements rail.Rail. The same nonce never creates a second
// Noah transaction, so the core may resend it after any unclear outcome.
//
// The contract with the core: rail.ErrRejected only when Noah definitely
// has no transaction for this payout, because the core then releases the
// hold. A 400/402/403 on its own is not proof: if an earlier attempt with
// this nonce went through and its answer was lost, a resend could be
// refused for another reason. So before reporting a rejection, Submit
// looks the payout up by ExternalID and returns the transaction if it
// exists. Every other failure (401, 409, 429, 5xx, timeout) is an unknown
// outcome: the core keeps the hold and the resolver resends.
func (r *Rail) Submit(ctx context.Context, q rail.Quote, nonce, externalID string) (rail.Tx, error) {
	fiat, err := formatFiat(q.FiatAmount, q.FiatCurrency)
	if err != nil {
		return rail.Tx{}, fmt.Errorf("%w: %w", rail.ErrRejected, err)
	}
	in := sellReq{
		CryptoCurrency:         r.cfg.CryptoCurrency,
		FiatAmount:             fiat,
		CryptoAuthorizedAmount: formatAmount(q.CryptoAuthorized),
		FormSessionID:          q.RailQuoteID,
		Nonce:                  nonce,
		ExternalID:             externalID,
	}
	var out txResp
	err = r.c.do(ctx, http.MethodPost, "/transactions/sell", nil, in, &out)
	if err == nil {
		return r.toTx(out.Transaction)
	}
	switch st := statusOf(err); st {
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusForbidden:
		t, found, lookErr := r.findByExternalID(ctx, externalID)
		switch {
		case lookErr != nil:
			return rail.Tx{}, fmt.Errorf("noah: submit refused (%w), and the lookup failed: %w", err, lookErr)
		case found:
			return r.toTx(t)
		default:
			return rail.Tx{}, fmt.Errorf("%w: %w", rail.ErrRejected, err)
		}
	default:
		return rail.Tx{}, err
	}
}

// findByExternalID looks for this payout's sell transaction among recent
// transactions. Noah cannot filter by ExternalID, so it pages back
// newest first over the last week, at most LookbackPages pages.
func (r *Rail) findByExternalID(ctx context.Context, externalID string) (Transaction, bool, error) {
	q := url.Values{}
	q.Set("CreatedFrom", r.now().Add(-7*24*time.Hour).UTC().Format(time.RFC3339))
	q.Set("PageSize", "100")
	for range r.cfg.LookbackPages {
		var out txListResp
		if err := r.c.do(ctx, http.MethodGet, "/transactions", q, nil, &out); err != nil {
			return Transaction{}, false, err
		}
		for _, t := range out.Items {
			if t.ExternalID == externalID && t.Direction == "Out" {
				return t, true, nil
			}
		}
		if out.PageToken == "" {
			return Transaction{}, false, nil
		}
		q.Set("PageToken", out.PageToken)
	}
	return Transaction{}, false, fmt.Errorf("noah: payout %s not found in the last %d pages; giving up without a verdict", externalID, r.cfg.LookbackPages)
}

// ---- Get ----

// Get implements rail.Rail.
func (r *Rail) Get(ctx context.Context, railTxID string) (rail.Tx, error) {
	var out txResp
	err := r.c.do(ctx, http.MethodGet, "/transactions/"+url.PathEscape(railTxID), nil, nil, &out)
	if st := statusOf(err); st == http.StatusNotFound {
		return rail.Tx{}, fmt.Errorf("%w: %w", rail.ErrNotFound, err)
	}
	if err != nil {
		return rail.Tx{}, err
	}
	if out.Transaction.ID != railTxID {
		return rail.Tx{}, fmt.Errorf("noah: asked for transaction %s, got %q", railTxID, out.Transaction.ID)
	}
	return r.toTx(out.Transaction)
}

// Version ranks a transaction's progress. GET /transactions/{id} carries
// no event version (only webhooks do), and it always returns the current
// state, so progress is ordered by how far the transaction has come: the
// core never moves a payout backwards on an older-looking report.
const (
	versionPending = 1
	versionFinal   = 2 // Settled or Failed
)

// toTx maps a Noah transaction to the rail's view, checking what Ferry
// relies on: it is an outgoing sale of the configured currency, and a
// settled one explains its amount (fail closed if not).
func (r *Rail) toTx(t Transaction) (rail.Tx, error) {
	if t.ID == "" {
		return rail.Tx{}, errors.New("noah: transaction without an ID")
	}
	if t.Direction != "Out" || t.CryptoCurrency != r.cfg.CryptoCurrency {
		return rail.Tx{}, fmt.Errorf("noah: transaction %s is %s %s, want Out %s", t.ID, t.Direction, t.CryptoCurrency, r.cfg.CryptoCurrency)
	}
	tx := rail.Tx{ID: t.ID, ExternalID: t.ExternalID, SubStatus: subStatus(t)}
	if t.FiatPaymentMethod != nil {
		tx.PaymentMethodID = t.FiatPaymentMethod.ID
	}
	switch t.Status {
	case "Pending":
		tx.Status, tx.Version = rail.StatusPending, versionPending
	case "Failed":
		tx.Status, tx.Version = rail.StatusFailed, versionFinal
	case "Settled":
		tx.Status, tx.Version = rail.StatusSettled, versionFinal
		spent, fee, err := settledAmounts(t)
		if err != nil {
			return rail.Tx{}, err
		}
		tx.Spent, tx.Fee = spent, fee
	default:
		return rail.Tx{}, fmt.Errorf("noah: transaction %s has unknown status %q", t.ID, t.Status)
	}
	return tx, nil
}

// subStatus combines Noah's SubStatus and RFI into one informational
// string, e.g. "UnderReview, RFI AwaitingCustomer".
func subStatus(t Transaction) string {
	parts := make([]string, 0, 2)
	if t.SubStatus != "" {
		parts = append(parts, t.SubStatus)
	}
	if t.RFI != nil && t.RFI.Status != "" {
		parts = append(parts, "RFI "+t.RFI.Status)
	}
	s := strings.Join(parts, ", ")
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

// settledAmounts splits a settled sale into what paid the bank (Spent) and
// the fees (Fee), from Noah's Breakdown, which is in crypto: Remaining is
// the net amount, ChannelFee and BusinessFee are fees. The parts must add
// up to Amount; if they do not, or the breakdown is missing, the payout
// is not settled (the error keeps it submitted for a human to look at).
func settledAmounts(t Transaction) (spent, fee ledger.Amount, err error) {
	total, err := parseAmount(t.Amount)
	if err != nil {
		return 0, 0, fmt.Errorf("noah: settled transaction %s amount: %w", t.ID, err)
	}
	if len(t.Breakdown) == 0 {
		return 0, 0, fmt.Errorf("noah: settled transaction %s has no breakdown", t.ID)
	}
	for _, b := range t.Breakdown {
		a, err := parseAmount(b.Amount)
		if err != nil {
			return 0, 0, fmt.Errorf("noah: settled transaction %s breakdown: %w", t.ID, err)
		}
		switch b.Type {
		case "Remaining":
			spent += a
		case "ChannelFee", "BusinessFee":
			fee += a
		default:
			return 0, 0, fmt.Errorf("noah: settled transaction %s: unknown breakdown type %q", t.ID, b.Type)
		}
	}
	if spent+fee != total {
		return 0, 0, fmt.Errorf("noah: settled transaction %s: breakdown %s + %s does not add up to %s",
			t.ID, spent, fee, total)
	}
	return spent, fee, nil
}

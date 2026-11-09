package noah_test

// Adapter tests against the fake Noah server (noahtest), which checks the
// API key and every request signature. No database.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/rail/noah"
	"github.com/darkodi/ferry/internal/rail/noah/noahtest"
)

const apiKey = "test-api-key"

var bankForm = json.RawMessage(`{"BankDetails":{"AccountNumber":"12345678","BankCode":"123456789"}}`)

// setup starts a fake that requires signatures and an adapter that signs.
func setup(t *testing.T) (*noah.Rail, *noahtest.Server) {
	t.Helper()
	key := p384(t)
	fake := noahtest.New(apiKey, &key.PublicKey)
	t.Cleanup(fake.Close)
	signer, err := noah.NewSigner(key, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := noah.NewClient(fake.BaseURL(), apiKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	r, err := noah.New(client, noah.Config{CryptoCurrency: noahtest.Crypto}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return r, fake
}

func usd(cents int64, method string) rail.QuoteRequest {
	return rail.QuoteRequest{
		PayoutID: uuid.New(), FiatAmount: cents, FiatCurrency: "USD",
		Destination: rail.Destination{Country: "US", FiatCurrency: "USD", PaymentMethodType: method, Form: bankForm},
	}
}

func usdc(s string) ledger.Amount {
	a, err := ledger.ParseUSDC(s)
	if err != nil {
		panic(err)
	}
	return a
}

// Prepare picks the channel for the payment method and returns Noah's
// authorized amount; the quote's expiry is Ferry's own TTL.
func TestPrepare(t *testing.T) {
	r, fake := setup(t)
	before := time.Now()
	q, err := r.Prepare(t.Context(), usd(100_00, "BankAch"))
	if err != nil {
		t.Fatal(err)
	}
	// 100 + 0.5% fee + 1% buffer at the fake's 1:1 rate.
	if q.CryptoAuthorized != usdc("101.5") || q.RailQuoteID == "" || q.FiatAmount != 100_00 {
		t.Fatalf("quote %+v", q)
	}
	if d := q.ExpiresAt.Sub(before); d < 2*time.Minute || d > 2*time.Minute+5*time.Second {
		t.Fatalf("quote expires in %s, want Ferry's 2 minute TTL", d)
	}
	if got := fake.Calls(); len(got) != 2 || got[0] != "GET /v1/channels/sell" || got[1] != "POST /v1/transactions/sell/prepare" {
		t.Fatalf("calls %v", got)
	}
	if fake.Signed() != 2 {
		t.Fatalf("%d signed requests, want 2", fake.Signed())
	}
}

// What Noah refuses for this payout fails it (ErrRejected); the core
// then marks the payout failed without holding anything.
func TestPrepareRejections(t *testing.T) {
	r, _ := setup(t)
	bad := usd(100_00, "BankAch")
	bad.Destination.Form = json.RawMessage(`{"BankDetails":{"AccountNumber":"1"}}`)
	tooBig := usd(20_000_00, "BankAch")
	tooSmall := usd(50, "BankAch") // below the channel's 1 USD minimum
	noChannel := usd(100_00, "BankSepa")
	yen := usd(100, "BankAch")
	yen.FiatCurrency = "JPY"
	for name, req := range map[string]rail.QuoteRequest{
		"form Noah rejects":     bad,
		"above channel maximum": tooBig,
		"below channel minimum": tooSmall,
		"no such channel":       noChannel,
		"unsupported currency":  yen,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Prepare(t.Context(), req); !errors.Is(err, rail.ErrRejected) {
				t.Fatalf("got %v, want ErrRejected", err)
			}
		})
	}
}

// A prepare answer that asks for another form step cannot be completed by
// a saved destination: the quote is refused, not guessed at.
func TestPrepareNextStepRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/channels/sell") {
			_, _ = w.Write([]byte(`{"Items":[{"ID":"214eab50-e22b-5e0f-b487-37dc1addee90","PaymentMethodType":"BankAch","Limits":{"MinLimit":"0","MaxLimit":"15000"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"CryptoAuthorizedAmount":"101.5","CryptoAmountEstimate":"100.5","FormSessionID":"s","TotalFee":"0.5",` +
			`"NextStep":{"StepID":"Vop","StepType":"Ack","Schema":{"$schema":"x","type":"object","properties":{}}}}`))
	}))
	defer srv.Close()
	c, err := noah.NewClient(srv.URL+"/v1", apiKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := noah.New(c, noah.Config{CryptoCurrency: noahtest.Crypto}, time.Now)
	if _, err := r.Prepare(t.Context(), usd(100_00, "BankAch")); !errors.Is(err, rail.ErrRejected) {
		t.Fatalf("got %v, want ErrRejected", err)
	}
}

func submit(t *testing.T, r *noah.Rail, nonce string) (rail.Quote, rail.Tx, error) {
	t.Helper()
	q, err := r.Prepare(t.Context(), usd(100_00, "BankAch"))
	if err != nil {
		t.Fatal(err)
	}
	ext := uuid.NewString()
	tx, err := r.Submit(t.Context(), q, nonce, ext)
	if err == nil && tx.ExternalID != ext {
		t.Fatalf("tx external id %q, want %q", tx.ExternalID, ext)
	}
	return q, tx, err
}

// The sell carries prepare's authorized amount byte for byte (the fake
// refuses anything else), and a resent nonce returns the same transaction.
func TestSubmitAndResend(t *testing.T) {
	r, fake := setup(t)
	q, tx, err := submit(t, r, "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != rail.StatusPending || tx.SubStatus != "AmlScreening" || tx.Version != 1 {
		t.Fatalf("tx %+v", tx)
	}
	again, err := r.Submit(t.Context(), q, "nonce-1", tx.ExternalID)
	if err != nil || again.ID != tx.ID {
		t.Fatalf("resend: %+v, %v", again, err)
	}
	if n := len(fake.Transactions()); n != 1 {
		t.Fatalf("%d transactions at Noah, want 1", n)
	}
}

// Submit's error contract with the core: ErrRejected only when Noah
// definitely has no transaction; everything unclear is a plain error.
func TestSubmitErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fault    noahtest.Fault
		rejected bool // want ErrRejected
		accepted bool // want a transaction despite the error answer
	}{
		{"insufficient balance", noahtest.Fault{Status: 402, Type: "InsufficientBalance"}, true, false},
		{"compliance refusal", noahtest.Fault{Status: 403, Type: "Forbidden"}, true, false},
		{"bad request", noahtest.Fault{Status: 400, Type: "InvalidMessage"}, true, false},
		{"server error", noahtest.Fault{Status: 500, Type: "Unexpected"}, false, false},
		{"conflict", noahtest.Fault{Status: 409, Type: "Conflict"}, false, false},
		{"unauthorized", noahtest.Fault{Status: 401, Type: "Unauthorized"}, false, false},
		{"created, then server error", noahtest.Fault{Status: 500, Type: "Unexpected", Create: true}, false, false},
		// The sale went through but the answer was a refusal (e.g. a lost
		// first answer, then a resend refused): found by ExternalID.
		{"created, then refused", noahtest.Fault{Status: 400, Type: "InvalidMessage", Create: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fake := setup(t)
			fake.FailNextSell(tc.fault)
			_, tx, err := submit(t, r, uuid.NewString()[:36])
			switch {
			case tc.accepted:
				if err != nil || tx.ID == "" {
					t.Fatalf("got %+v, %v; want the existing transaction", tx, err)
				}
			case tc.rejected:
				if !errors.Is(err, rail.ErrRejected) {
					t.Fatalf("got %v, want ErrRejected", err)
				}
			default:
				if err == nil || errors.Is(err, rail.ErrRejected) {
					t.Fatalf("got %v, want an unknown-outcome error (not ErrRejected)", err)
				}
			}
		})
	}
}

// A settled sale reports what went to the bank and the fee, from Noah's
// breakdown; status and version follow Noah's progress.
func TestGetSettledAndFailed(t *testing.T) {
	r, fake := setup(t)
	_, tx, err := submit(t, r, "n-settle")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetSubStatus(tx.ID, "UnderReview", "AwaitingCustomer"); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(t.Context(), tx.ID)
	if err != nil || got.SubStatus != "UnderReview, RFI AwaitingCustomer" || got.Version != 1 {
		t.Fatalf("under review: %+v, %v", got, err)
	}
	if err := fake.Settle(tx.ID); err != nil {
		t.Fatal(err)
	}
	got, err = r.Get(t.Context(), tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != rail.StatusSettled || got.Version != 2 || got.Spent != usdc("100") || got.Fee != usdc("0.5") || got.PaymentMethodID == "" {
		t.Fatalf("settled: %+v", got)
	}

	_, tx2, err := submit(t, r, "n-fail")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Fail(tx2.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Get(t.Context(), tx2.ID); err != nil || got.Status != rail.StatusFailed || got.Version != 2 {
		t.Fatalf("failed: %+v, %v", got, err)
	}

	if _, err := r.Get(t.Context(), uuid.NewString()); !errors.Is(err, rail.ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
}

// A settlement whose breakdown is missing or does not add up is not
// applied: Get fails, so the payout stays submitted for a human.
func TestSettledBreakdownMustAddUp(t *testing.T) {
	for name, parts := range map[string]map[string]string{
		"missing":       nil,
		"does not sum":  {"Remaining": "100", "ChannelFee": "0.4"},
		"unknown type":  {"Remaining": "100", "Mystery": "0.5"},
		"bad amount":    {"Remaining": "100.0000001", "ChannelFee": "0.5"},
		"business fees": {"Remaining": "99", "ChannelFee": "0.5", "BusinessFee": "1"}, // adds up: accepted
	} {
		t.Run(name, func(t *testing.T) {
			r, fake := setup(t)
			_, tx, err := submit(t, r, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			if err := fake.SettleWithBreakdown(tx.ID, "100.5", parts); err != nil {
				t.Fatal(err)
			}
			got, err := r.Get(t.Context(), tx.ID)
			if name == "business fees" {
				if err != nil || got.Spent != usdc("99") || got.Fee != usdc("1.5") {
					t.Fatalf("got %+v, %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted an unexplained settlement: %+v", got)
			}
		})
	}
}

// Requests without a valid signature fail when Noah requires one.
func TestUnsignedRequestsRefused(t *testing.T) {
	key := p384(t)
	fake := noahtest.New(apiKey, &key.PublicKey)
	defer fake.Close()
	unsigned, err := noah.NewClient(fake.BaseURL(), apiKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := noah.New(unsigned, noah.Config{CryptoCurrency: noahtest.Crypto}, time.Now)
	_, err = r.Prepare(t.Context(), usd(100_00, "BankAch"))
	var apiErr *noah.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("got %v, want 401 from Noah", err)
	}

	wrongKey, _ := noah.NewSigner(p384(t), time.Now)
	c, _ := noah.NewClient(fake.BaseURL(), apiKey, wrongKey)
	r, _ = noah.New(c, noah.Config{CryptoCurrency: noahtest.Crypto}, time.Now)
	if _, err := r.Prepare(t.Context(), usd(100_00, "BankAch")); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("wrong signing key: got %v, want 401", err)
	}
}

func TestClientConfig(t *testing.T) {
	if _, err := noah.NewClient("https://api.noah.com/v1", apiKey, nil); err == nil {
		t.Error("production base URL without a signing key accepted")
	}
	if _, err := noah.NewClient("http://api.sandbox.noah.com/v1", apiKey, nil); err == nil {
		t.Error("plain http to a remote host accepted")
	}
	if _, err := noah.NewClient("https://api.sandbox.noah.com/v1", "", nil); err == nil {
		t.Error("empty API key accepted")
	}
	if _, err := noah.NewClient("https://api.sandbox.noah.com/v1", apiKey, nil); err != nil {
		t.Errorf("sandbox without signing: %v", err)
	}
}

// The client never follows a redirect, which would resend the API key and
// a signed body to wherever it points.
func TestNoRedirects(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := noah.NewClient(srv.URL+"/v1", apiKey, nil)
	r, _ := noah.New(c, noah.Config{CryptoCurrency: noahtest.Crypto}, time.Now)
	if _, err := r.Get(t.Context(), "x"); err == nil {
		t.Fatal("a redirect answer was treated as success")
	}
	if hit {
		t.Fatal("followed the redirect")
	}
}

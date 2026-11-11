package noah_test

// Contract test against the real Noah sandbox. It runs only when
// NOAH_API_KEY is set (plus NOAH_SIGNING_KEY_PATH if the key requires
// signing), and checks what the fake assumes:
//
//   - channel listing and prepare work with Ferry's signed requests,
//     including the queryParams claim on a GET with a query;
//   - prepare's CryptoAuthorizedAmount parses exactly and formats back
//     to the same string (what Submit sends);
//   - with NOAH_SANDBOX_SELL=1, a 1 USD sale succeeds, a resend with the
//     same nonce returns the same transaction, and GET returns it.
//
//	NOAH_API_KEY=... go test ./internal/rail/noah -run Sandbox -v
//
// The sell moves sandbox money only (USDC_TEST).

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/rail/noah"
)

func sandbox(t *testing.T) *noah.Rail {
	t.Helper()
	if os.Getenv("NOAH_API_KEY") == "" {
		t.Skip("set NOAH_API_KEY (and NOAH_SIGNING_KEY_PATH if required) to run against the Noah sandbox")
	}
	if b := os.Getenv("NOAH_BASE_URL"); b != "" && b != "https://api.sandbox.noah.com/v1" {
		t.Skipf("NOAH_BASE_URL is %s; the contract test runs only against the sandbox", b)
	}
	r, err := noah.FromEnv(t.Context(), os.Getenv, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSandboxContract(t *testing.T) {
	r := sandbox(t)
	req := rail.QuoteRequest{
		PayoutID: uuid.New(), FiatAmount: 1_00, FiatCurrency: "USD",
		Destination: rail.Destination{
			Country: "US", FiatCurrency: "USD", PaymentMethodType: "BankAch",
			Form: []byte(`{"AccountHolderAddress":{"Address":"123 Main Street","City":"New York","State":"NY","PostalCode":"10001"},` +
				`"BankDetails":{"AccountNumber":"12345678","BankCode":"021000021","AccountType":"Checking"},"PaymentPurpose":"Invoice"}`),
		},
	}
	q, err := r.Prepare(t.Context(), req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Logf("quote: authorized %s USDC_TEST, form session %s, signed=%v", q.CryptoAuthorized, q.RailQuoteID, r.Signed())

	if os.Getenv("NOAH_SANDBOX_SELL") != "1" {
		t.Log("set NOAH_SANDBOX_SELL=1 to also execute a 1 USD sale")
		return
	}
	nonce, ext := uuid.NewString(), req.PayoutID.String()
	tx, err := r.Submit(t.Context(), q, nonce, ext)
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	again, err := r.Submit(t.Context(), q, nonce, ext)
	if err != nil || again.ID != tx.ID {
		t.Fatalf("resend with the same nonce: %+v, %v (first %s)", again, err, tx.ID)
	}
	got, err := r.Get(t.Context(), tx.ID)
	if err != nil || got.ExternalID != ext {
		t.Fatalf("get: %+v, %v", got, err)
	}
	t.Logf("transaction %s: %s %s", got.ID, got.Status, got.SubStatus)
}

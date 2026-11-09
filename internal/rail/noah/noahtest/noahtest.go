// Package noahtest is a fake Noah Business API for tests and local runs.
//
// It implements the endpoints Ferry calls, with the request and response
// shapes from docs.noah.com: GET /channels/sell, POST /transactions/sell/
// prepare, POST /transactions/sell, GET /transactions/{id} and GET
// /transactions. It checks X-Api-Key and, when given a public key, every
// Api-Signature JWT (method, path, query and body hash), so the real
// adapter's signing is exercised end to end.
//
// Behaviour Noah's docs do not pin down is a stated assumption here:
//   - a sell resent with a used Nonce returns the original transaction
//     (202), which is what "idempotently retried" suggests;
//   - a settled sale's Breakdown is in crypto and adds up to Amount.
//
// The real-sandbox contract test (internal/rail/noah/sandbox_test.go)
// checks these once a sandbox key exists.
package noahtest

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/rail/noah"
)

// Crypto is the only currency the fake sells.
const Crypto = "USDC_TEST"

// Pricing: 1 USDC_TEST = 1 USD/EUR/GBP. The channel fee is 0.5% of the
// fiat amount; the authorized amount adds a 1% buffer on top.
const (
	feeDivisor    = 200
	bufferDivisor = 100
	microPerCent  = 10_000
)

// Channels are the fake's payout routes.
var Channels = []Channel{
	{ID: "214eab50-e22b-5e0f-b487-37dc1addee90", Country: "US", FiatCurrency: "USD", PaymentMethodType: "BankAch", MinLimit: "1", MaxLimit: "15000"},
	{ID: "a4c3f754-094b-5f73-9da9-e0f1cb367f31", Country: "US", FiatCurrency: "USD", PaymentMethodType: "BankFedwire", MinLimit: "0", MaxLimit: "15000"},
	{ID: "6b1e7c2a-1d3f-5e8a-9c4b-2f7d8e9a0b1c", Country: "DE", FiatCurrency: "EUR", PaymentMethodType: "BankSepa", MinLimit: "1", MaxLimit: "50000"},
}

// Channel is one payout route.
type Channel struct {
	ID, Country, FiatCurrency, PaymentMethodType, MinLimit, MaxLimit string
}

// Fault makes the next sell answer with an error.
type Fault struct {
	Status int    // HTTP status, e.g. 402 or 500
	Type   string // Noah error Type, e.g. "InsufficientBalance"
	// Create makes the transaction anyway before answering with the
	// error: a sale that went through but whose answer was lost.
	Create bool
}

// Server is the fake. Use New for a test server, or Handler to serve it
// yourself.
type Server struct {
	*httptest.Server // nil when served through Handler

	APIKey string
	// SigningKey, if set, makes every request require a valid
	// Api-Signature made with the matching private key.
	SigningKey *ecdsa.PublicKey

	mu       sync.Mutex
	now      func() time.Time
	seq      int
	sessions map[string]session
	txs      map[string]*noah.Transaction
	order    []string          // tx ids, oldest first
	byNonce  map[string]string // nonce -> tx id
	pms      map[string]string // payment method id -> account summary
	faults   []Fault
	calls    []string
	signed   int
}

type session struct {
	channel    Channel
	fiat       string
	fiatMinor  int64
	needed     ledger.Amount
	fee        ledger.Amount
	authorized string
	pmID       string
}

// New starts a fake on a local port. Close it when done.
func New(apiKey string, signingKey *ecdsa.PublicKey) *Server {
	s := NewUnstarted(apiKey, signingKey, time.Now)
	s.Server = httptest.NewServer(s.Handler())
	return s
}

// NewUnstarted returns a fake that is not listening; serve Handler().
func NewUnstarted(apiKey string, signingKey *ecdsa.PublicKey, now func() time.Time) *Server {
	return &Server{
		APIKey: apiKey, SigningKey: signingKey, now: now,
		sessions: map[string]session{}, txs: map[string]*noah.Transaction{},
		byNonce: map[string]string{}, pms: map[string]string{},
	}
}

// BaseURL is the API base, e.g. http://127.0.0.1:1234/v1.
func (s *Server) BaseURL() string { return s.URL + "/v1" }

// Handler serves the fake API under /v1 and test controls under
// /_control.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/channels/sell", s.auth(s.channels))
	mux.HandleFunc("POST /v1/transactions/sell/prepare", s.auth(s.prepare))
	mux.HandleFunc("POST /v1/transactions/sell", s.auth(s.sell))
	mux.HandleFunc("GET /v1/transactions/{id}", s.auth(s.get))
	mux.HandleFunc("GET /v1/transactions", s.auth(s.list))
	mux.HandleFunc("POST /_control/transactions/{id}/{action}", s.control)
	mux.HandleFunc("GET /_control/transactions", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := make([]noah.Transaction, 0, len(s.order))
		for _, id := range s.order {
			out = append(out, *s.txs[id])
		}
		writeJSON(w, http.StatusOK, map[string]any{"Items": out})
	})
	return mux
}

// ---- test controls ----

// FailNextSell queues faults for the next sell requests, in order.
func (s *Server) FailNextSell(f ...Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, f...)
}

// Settle settles a pending transaction at the quoted price: the fiat
// amount in crypto as Remaining, the channel fee as ChannelFee.
func (s *Server) Settle(id string) error {
	return s.update(id, func(t *noah.Transaction, ss session) {
		t.Status, t.SubStatus, t.RFI = "Settled", "", nil
		t.Amount = amount(ss.needed + ss.fee)
		t.Breakdown = breakdown(ss.needed, ss.fee)
	})
}

// SettleWithBreakdown settles with an arbitrary Breakdown (nil for none),
// to test how the adapter handles an inconsistent report.
func (s *Server) SettleWithBreakdown(id, total string, parts map[string]string) error {
	return s.update(id, func(t *noah.Transaction, _ session) {
		t.Status, t.SubStatus, t.Amount = "Settled", "", total
		t.Breakdown = nil
		for _, typ := range slices.Sorted(maps.Keys(parts)) {
			t.Breakdown = append(t.Breakdown, struct {
				Type   string `json:"Type"`
				Amount string `json:"Amount"`
			}{typ, parts[typ]})
		}
	})
}

// Fail fails a pending transaction, as a compliance rejection would (the
// authorized amount is refunded to the business balance).
func (s *Server) Fail(id string) error {
	return s.update(id, func(t *noah.Transaction, _ session) { t.Status, t.SubStatus, t.RFI = "Failed", "", nil })
}

// SetSubStatus moves a pending transaction to another sub-status and RFI
// status (empty for none).
func (s *Server) SetSubStatus(id, sub, rfi string) error {
	return s.update(id, func(t *noah.Transaction, _ session) {
		t.SubStatus, t.RFI = sub, nil
		if rfi != "" {
			t.RFI = &struct {
				Status string `json:"Status"`
				Type   string `json:"Type"`
			}{rfi, "Manual"}
		}
	})
}

// Transactions returns every transaction, oldest first.
func (s *Server) Transactions() []noah.Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]noah.Transaction, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.txs[id])
	}
	return out
}

// Calls returns "METHOD /path" for every authenticated API request.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Signed is how many requests carried a valid signature.
func (s *Server) Signed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signed
}

func (s *Server) update(id string, f func(*noah.Transaction, session)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txs[id]
	if !ok {
		return fmt.Errorf("noahtest: no transaction %s", id)
	}
	if t.Status != "Pending" {
		return fmt.Errorf("noahtest: transaction %s is %s, not Pending", id, t.Status)
	}
	f(t, s.sessionOf(t))
	return nil
}

// sessionOf returns the price a transaction was sold at.
func (s *Server) sessionOf(t *noah.Transaction) session { return s.sessions["used:"+t.ID] }

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "settle":
		err = s.Settle(id)
	case "fail":
		err = s.Fail(id)
	case "review":
		err = s.SetSubStatus(id, "UnderReview", "AwaitingCustomer")
	default:
		err = fmt.Errorf("unknown action %q (settle, fail, review)", r.PathValue("action"))
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidMessage", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- auth ----

func (s *Server) auth(next func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidMessage", "read body")
			return
		}
		if r.Header.Get("X-Api-Key") != s.APIKey {
			writeErr(w, http.StatusUnauthorized, "Unauthorized", "invalid API key")
			return
		}
		if s.SigningKey != nil {
			if err := s.checkSignature(r, body); err != nil {
				writeErr(w, http.StatusUnauthorized, "Unauthorized", "request signing failed: "+err.Error())
				return
			}
		}
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		if s.SigningKey != nil {
			s.signed++
		}
		s.mu.Unlock()
		next(w, r, body)
	}
}

// checkSignature verifies Api-Signature the way the docs describe:
// ES384, aud, a lifetime of at most 15 minutes, and claims that match the
// request's method, path, query and exact body bytes.
func (s *Server) checkSignature(r *http.Request, body []byte) error {
	tok := r.Header.Get("Api-Signature")
	if tok == "" {
		return fmt.Errorf("missing Api-Signature")
	}
	c, err := noah.VerifyJWT(tok, s.SigningKey)
	if err != nil {
		return err
	}
	now := s.now().Unix()
	switch {
	case c.Audience != noah.Audience:
		return fmt.Errorf("aud %q", c.Audience)
	case c.Expires < now || c.IssuedAt > now+60:
		return fmt.Errorf("token not valid now")
	case c.Expires-c.IssuedAt > 15*60:
		return fmt.Errorf("token lives longer than 15 minutes")
	case c.Method != r.Method:
		return fmt.Errorf("method %q, request is %s", c.Method, r.Method)
	case c.Path != r.URL.Path:
		return fmt.Errorf("path %q, request is %s", c.Path, r.URL.Path)
	}
	q := r.URL.Query()
	if len(q) != len(c.QueryParams) {
		return fmt.Errorf("query params do not match")
	}
	for k, v := range c.QueryParams {
		if got := q[k]; len(got) != 1 || got[0] != v {
			return fmt.Errorf("query param %s does not match", k)
		}
	}
	if len(body) == 0 {
		if c.BodyHash != "" {
			return fmt.Errorf("bodyHash set for an empty body")
		}
		return nil
	}
	sum := sha256.Sum256(body)
	if c.BodyHash != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("bodyHash does not match the body")
	}
	return nil
}

// ---- API ----

func (s *Server) channels(w http.ResponseWriter, r *http.Request, _ []byte) {
	q := r.URL.Query()
	if q.Get("CryptoCurrency") != Crypto {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}})
		return
	}
	items := []map[string]any{}
	for _, c := range Channels {
		if c.Country != q.Get("Country") || c.FiatCurrency != q.Get("FiatCurrency") {
			continue
		}
		items = append(items, map[string]any{
			"ID": c.ID, "Country": c.Country, "FiatCurrency": c.FiatCurrency,
			"PaymentMethodType": c.PaymentMethodType, "PaymentMethodCategory": "Bank",
			"Limits":     map[string]string{"MinLimit": c.MinLimit, "MaxLimit": c.MaxLimit},
			"Calculated": map[string]string{"TotalFee": "0.03"}, "ProcessingSeconds": 86400, "Rate": "1",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": items})
}

var decimalRE = regexp.MustCompile(`^[+]?([.]\d+|\d+[.]?\d*)$`) // Noah's amount pattern

func (s *Server) prepare(w http.ResponseWriter, _ *http.Request, body []byte) {
	var in struct {
		ChannelID       string          `json:"ChannelID"`
		CryptoCurrency  string          `json:"CryptoCurrency"`
		FiatAmount      string          `json:"FiatAmount"`
		Form            json.RawMessage `json:"Form"`
		PaymentMethodID string          `json:"PaymentMethodID"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "body is not JSON")
		return
	}
	var ch *Channel
	for i := range Channels {
		if Channels[i].ID == in.ChannelID {
			ch = &Channels[i]
		}
	}
	switch {
	case ch == nil:
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "unknown ChannelID")
		return
	case in.CryptoCurrency != Crypto:
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "unsupported CryptoCurrency")
		return
	case !decimalRE.MatchString(in.FiatAmount):
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "FiatAmount fails the pattern")
		return
	}
	cents, ok := centsOf(in.FiatAmount)
	if !ok || cents <= 0 {
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "FiatAmount must be positive with at most 2 decimals")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pmID := in.PaymentMethodID
	switch {
	case pmID != "":
		if _, ok := s.pms[pmID]; !ok {
			writeErr(w, http.StatusBadRequest, "InvalidMessage", "unknown PaymentMethodID")
			return
		}
	default:
		acct, err := accountOf(in.Form)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidMessage", "Form: "+err.Error())
			return
		}
		s.seq++
		pmID = fmt.Sprintf("Bank/%s/%s/%s/%d", ch.PaymentMethodType, ch.FiatCurrency, acct, s.seq)
		s.pms[pmID] = acct
	}
	needed := ledger.Amount(cents * microPerCent)
	fee := needed / feeDivisor
	s.seq++
	sid := fmt.Sprintf("00000000-0000-4000-8000-%012d", s.seq)
	ss := session{
		channel: *ch, fiat: in.FiatAmount, fiatMinor: cents, needed: needed, fee: fee,
		authorized: amount(needed + fee + needed/bufferDivisor), pmID: pmID,
	}
	s.sessions[sid] = ss
	writeJSON(w, http.StatusOK, map[string]any{
		"CryptoAmountEstimate":   amount(needed + fee),
		"CryptoAuthorizedAmount": ss.authorized,
		"FormSessionID":          sid,
		"TotalFee":               amount(fee),
	})
}

func (s *Server) sell(w http.ResponseWriter, _ *http.Request, body []byte) {
	var in struct {
		CryptoCurrency         string `json:"CryptoCurrency"`
		FiatAmount             string `json:"FiatAmount"`
		CryptoAuthorizedAmount string `json:"CryptoAuthorizedAmount"`
		FormSessionID          string `json:"FormSessionID"`
		Nonce                  string `json:"Nonce"`
		ExternalID             string `json:"ExternalID"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "body is not JSON")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byNonce[in.Nonce]; ok { // an idempotent retry
		writeJSON(w, http.StatusAccepted, map[string]any{"Transaction": s.txs[id]})
		return
	}
	var fault *Fault
	if len(s.faults) > 0 {
		f := s.faults[0]
		s.faults = s.faults[1:]
		fault = &f
	}
	ss, ok := s.sessions[in.FormSessionID]
	switch {
	case fault != nil && !fault.Create:
		writeErr(w, fault.Status, fault.Type, "injected fault")
		return
	case len(in.Nonce) < 1 || len(in.Nonce) > 36:
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "Nonce must be 1-36 characters")
		return
	case len(in.ExternalID) > 36:
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "ExternalID longer than 36")
		return
	case !ok:
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "unknown or expired FormSessionID")
		return
	case in.CryptoCurrency != Crypto || in.FiatAmount != ss.fiat:
		writeErr(w, http.StatusBadRequest, "InvalidMessage", "CryptoCurrency or FiatAmount differs from prepare")
		return
	case in.CryptoAuthorizedAmount != ss.authorized:
		// The docs say to send prepare's value as-is; the fake holds
		// Ferry to it byte for byte.
		writeErr(w, http.StatusBadRequest, "InvalidMessage",
			fmt.Sprintf("CryptoAuthorizedAmount %q differs from prepare's %q", in.CryptoAuthorizedAmount, ss.authorized))
		return
	}
	s.seq++
	t := &noah.Transaction{
		ID:             fmt.Sprintf("10000000-0000-4000-8000-%012d", s.seq),
		Created:        s.now().UTC().Truncate(time.Second),
		Status:         "Pending",
		SubStatus:      "AmlScreening",
		Direction:      "Out",
		ExternalID:     in.ExternalID,
		CryptoCurrency: Crypto,
	}
	t.FiatPaymentMethod = &struct {
		ID string `json:"ID"`
	}{ss.pmID}
	s.txs[t.ID] = t
	s.order = append(s.order, t.ID)
	s.byNonce[in.Nonce] = t.ID
	delete(s.sessions, in.FormSessionID)
	s.sessions["used:"+t.ID] = ss // keep the price for Settle
	if fault != nil {
		writeErr(w, fault.Status, fault.Type, "injected fault after creating the transaction")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"Transaction": t})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, _ []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txs[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, "ResourceNotFound", "no such transaction")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Transaction": t})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, _ []byte) {
	q := r.URL.Query()
	size := 20
	if n, err := strconv.Atoi(q.Get("PageSize")); err == nil && n >= 1 && n <= 100 {
		size = n
	}
	from := time.Time{}
	if v := q.Get("CreatedFrom"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidMessage", "CreatedFrom")
			return
		}
		from = t
	}
	start, _ := strconv.Atoi(q.Get("PageToken"))
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []*noah.Transaction
	for i := len(s.order) - 1; i >= 0; i-- { // DESC by default
		if t := s.txs[s.order[i]]; !t.Created.Before(from) {
			all = append(all, t)
		}
	}
	if start > len(all) {
		start = len(all)
	}
	end := min(start+size, len(all))
	resp := map[string]any{"Items": all[start:end]}
	if end < len(all) {
		resp["PageToken"] = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, typ, detail string) {
	writeJSON(w, status, map[string]string{"Type": typ, "Detail": detail, "Instance": "fake-" + strconv.Itoa(status)})
}

// amount writes micro-USDC the way Noah does: "1010.03", no trailing zeros.
func amount(a ledger.Amount) string {
	s := strings.TrimRight(a.String(), "0")
	return strings.TrimSuffix(s, ".")
}

func breakdown(needed, fee ledger.Amount) []struct {
	Type   string `json:"Type"`
	Amount string `json:"Amount"`
} {
	return []struct {
		Type   string `json:"Type"`
		Amount string `json:"Amount"`
	}{{"ChannelFee", amount(fee)}, {"Remaining", amount(needed)}}
}

func centsOf(s string) (int64, bool) {
	whole, frac, _ := strings.Cut(strings.TrimPrefix(s, "+"), ".")
	if len(frac) > 2 {
		return 0, false
	}
	if whole == "" {
		whole = "0"
	}
	v, err := strconv.ParseInt(whole+frac+strings.Repeat("0", 2-len(frac)), 10, 64)
	return v, err == nil
}

// accountOf checks a bank form the way the BankAch schema in the docs
// does: BankDetails.AccountNumber (4-17) and BankDetails.BankCode (9).
func accountOf(form json.RawMessage) (string, error) {
	var f struct {
		BankDetails struct {
			AccountNumber string `json:"AccountNumber"`
			BankCode      string `json:"BankCode"`
		} `json:"BankDetails"`
	}
	if len(form) == 0 || json.Unmarshal(form, &f) != nil {
		return "", fmt.Errorf("required")
	}
	n, code := f.BankDetails.AccountNumber, f.BankDetails.BankCode
	if len(n) < 4 || len(n) > 17 {
		return "", fmt.Errorf("BankDetails.AccountNumber must be 4-17 characters")
	}
	if len(code) != 9 {
		return "", fmt.Errorf("BankDetails.BankCode must be 9 characters")
	}
	return n, nil
}

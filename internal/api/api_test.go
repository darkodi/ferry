package api_test

// HTTP tests against a real server, a throwaway Postgres and the mock rail
// (auto-settling, as in dev). Skipped with -short.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/api"
	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/idempotency"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/rail/mock"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

const adminKey = "test-admin-key-0123456789abcdef0123456789"

var (
	pool   *pgxpool.Pool
	idem   *idempotency.Store
	srv    *httptest.Server
	relay  *outbox.Relay
	worker *payout.Worker
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	tdb, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test database:", err)
		return 1
	}
	defer tdb.Close()
	cfg, err := pgxpool.ParseConfig(tdb.DSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg.MaxConns = 20
	if pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	house, err := ledger.OpenHouseAccounts(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rl := mock.New(time.Now)
	rl.AutoSettle = true
	log := slog.New(slog.DiscardHandler)
	idem = idempotency.NewStore(pool, time.Now, 30*time.Second, log)
	payouts := payout.NewService(pool, rl, house, time.Now)
	// Account m/44'/60'/1' of the public test mnemonic, as in .env.example.
	deriver, err := evm.NewDeriver("evm-local", "xpub6Ce9NcJvTk372KjsGfWqbcex5DumjpNquQLApoeQUavSCjEc823BV1tb4rXUuPuht8h2hSxkg2EXUaKUJmniJvRZAELxypsCzBFdtosmV76")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	q := queue.NewMemory()
	relay = outbox.NewRelay(pool, q, time.Now, log)
	worker = payout.NewWorker(payouts, q, log)
	srv = httptest.NewServer(api.New(api.Config{
		Pool: pool, Payouts: payouts, Idem: idem,
		House: house, Addresses: chain.NewAddresses(deriver), AdminKey: adminKey, Dev: true, Log: log,
	}))
	defer srv.Close()
	return m.Run()
}

// ---- helpers ----

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return m
}

func call(t *testing.T, method, path, token, idemKey string, body any) resp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewBufferString(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set(idempotency.Header, idemKey)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func wantStatus(t *testing.T, r resp, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
}

// newCustomer creates a customer through the admin API and funds it.
func newCustomer(t *testing.T, funds string) (id, key string) {
	t.Helper()
	if srv == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	r := call(t, "POST", "/v1/customers", adminKey, "", map[string]string{"name": "Acme"})
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	id, key = m["id"].(string), m["api_key"].(string)
	if funds != "" {
		r = call(t, "POST", "/v1/dev/deposits", adminKey, "", map[string]string{"customer_id": id, "amount": funds})
		wantStatus(t, r, http.StatusCreated)
	}
	return id, key
}

var payoutBody = map[string]any{"fiat_amount": 100_00, "fiat_currency": "USD", "destination": "bank_acct_1"}

// ---- tests ----

func TestPayoutEndToEnd(t *testing.T) {
	id, key := newCustomer(t, "200")

	r := call(t, "POST", "/v1/payouts", key, "order-1", payoutBody)
	wantStatus(t, r, http.StatusCreated)
	p := r.json(t)
	if p["status"] != "quoted" {
		t.Fatalf("status %v, want quoted", p["status"])
	}
	if got := p["quote"].(map[string]any)["crypto_authorized"]; got != "101.500000" {
		t.Fatalf("crypto_authorized %v, want 101.500000", got)
	}

	pid := p["id"].(string)
	r = call(t, "POST", "/v1/payouts/"+pid+"/confirm", key, "confirm-1", nil)
	wantStatus(t, r, http.StatusOK)
	if s := r.json(t)["status"]; s != "held" {
		t.Fatalf("after confirm: status %v, want held", s)
	}

	// What runs in the background: the relay publishes payout.held and the
	// worker submits it; the mock rail settles at once.
	for {
		published, err := relay.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		received, err := worker.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if published == 0 && received == 0 {
			break
		}
	}

	r = call(t, "GET", "/v1/payouts/"+pid, key, "", nil)
	wantStatus(t, r, http.StatusOK)
	if s := r.json(t)["status"]; s != "settled" {
		t.Fatalf("after the worker: status %v, want settled", s)
	}

	r = call(t, "GET", "/v1/customers/"+id+"/balances", key, "", nil)
	wantStatus(t, r, http.StatusOK)
	// 200 - 100 spent - 0.50 rail fee; the 1.00 buffer came back.
	if b := r.json(t); b["available"] != "99.500000" || b["held"] != "0.000000" {
		t.Fatalf("balances %v, want available 99.500000, held 0.000000", b)
	}
}

func TestAuth(t *testing.T) {
	aID, aKey := newCustomer(t, "10")
	_, bKey := newCustomer(t, "10")
	r := call(t, "POST", "/v1/payouts", aKey, uuid.NewString(), map[string]any{
		"fiat_amount": 1_00, "fiat_currency": "USD", "destination": "bank_acct_1",
	})
	wantStatus(t, r, http.StatusCreated)
	pid := r.json(t)["id"].(string)

	for _, tc := range []struct {
		name, method, path, token string
		want                      int
	}{
		{"no key", "GET", "/v1/payouts/" + pid, "", http.StatusUnauthorized},
		{"wrong key", "GET", "/v1/payouts/" + pid, "ferry_sk_nope", http.StatusUnauthorized},
		{"other customer's payout", "GET", "/v1/payouts/" + pid, bKey, http.StatusNotFound},
		{"other customer's balances", "GET", "/v1/customers/" + aID + "/balances", bKey, http.StatusNotFound},
		{"other customer confirms", "POST", "/v1/payouts/" + pid + "/confirm", bKey, http.StatusNotFound},
		{"customer key on admin route", "POST", "/v1/customers", aKey, http.StatusUnauthorized},
		{"admin key on customer route", "GET", "/v1/payouts/" + pid, adminKey, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body any
			if tc.method == "POST" {
				body = map[string]string{"name": "x"}
			}
			wantStatus(t, call(t, tc.method, tc.path, tc.token, uuid.NewString(), body), tc.want)
		})
	}
}

func TestInputValidation(t *testing.T) {
	_, key := newCustomer(t, "")
	for _, tc := range []struct {
		name string
		body any
		want int
	}{
		{"unknown field", `{"fiat_amount":100,"fiat_currency":"USD","destination":"b","memo":"x"}`, http.StatusBadRequest},
		{"fractional amount", `{"fiat_amount":1.5,"fiat_currency":"USD","destination":"b"}`, http.StatusBadRequest},
		{"amount as string", `{"fiat_amount":"100","fiat_currency":"USD","destination":"b"}`, http.StatusBadRequest},
		{"trailing data", `{"fiat_amount":100,"fiat_currency":"USD","destination":"b"} {}`, http.StatusBadRequest},
		{"negative amount", `{"fiat_amount":-100,"fiat_currency":"USD","destination":"b"}`, http.StatusUnprocessableEntity},
		{"bad currency", `{"fiat_amount":100,"fiat_currency":"usd","destination":"b"}`, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantStatus(t, call(t, "POST", "/v1/payouts", key, uuid.NewString(), tc.body), tc.want)
		})
	}
}

func TestIdempotencyKey(t *testing.T) {
	_, key := newCustomer(t, "1000")

	wantStatus(t, call(t, "POST", "/v1/payouts", key, "", payoutBody), http.StatusBadRequest)

	first := call(t, "POST", "/v1/payouts", key, "k1", payoutBody)
	wantStatus(t, first, http.StatusCreated)
	again := call(t, "POST", "/v1/payouts", key, "k1", payoutBody)
	wantStatus(t, again, http.StatusCreated)
	if !bytes.Equal(first.body, again.body) || again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay differs or is not marked:\n%s\n%s", first.body, again.body)
	}

	other := map[string]any{"fiat_amount": 5_00, "fiat_currency": "USD", "destination": "bank_acct_1"}
	wantStatus(t, call(t, "POST", "/v1/payouts", key, "k1", other), http.StatusUnprocessableEntity)

	// A request with this key is still running elsewhere.
	cid, err := customerIDFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idem.Begin(context.Background(), cid, "k2", idempotency.Hash("POST", "/v1/payouts", []byte("x"))); err != nil {
		t.Fatal(err)
	}
	r := call(t, "POST", "/v1/payouts", key, "k2", "x")
	wantStatus(t, r, http.StatusConflict)
	if r.header.Get("Retry-After") == "" {
		t.Fatal("409 without Retry-After")
	}
}

// 100 clients send the same create at once: exactly one payout exists and
// every success returns it.
func TestConcurrentIdenticalCreates(t *testing.T) {
	_, key := newCustomer(t, "")
	const n = 100
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		ids   = map[string]int{}
		codes = map[int]int{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // closing the channel releases every goroutine at once
			r := call(t, "POST", "/v1/payouts", key, "same-key", payoutBody)
			mu.Lock()
			defer mu.Unlock()
			codes[r.status]++
			if r.status == http.StatusCreated {
				var m map[string]any
				_ = json.Unmarshal(r.body, &m)
				ids[fmt.Sprint(m["id"])]++
			}
		}()
	}
	close(start)
	wg.Wait()

	t.Logf("statuses: %v", codes) // expect a mix: 201 for the winner and replays, 409 while it ran
	if codes[http.StatusCreated]+codes[http.StatusConflict] != n {
		t.Fatalf("unexpected statuses: %v", codes)
	}
	if len(ids) != 1 {
		t.Fatalf("successful responses name %d payouts, want 1: %v", len(ids), ids)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payouts WHERE idempotency_key = 'same-key'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d payouts in the database, want 1", count)
	}
}

func customerIDFromKey(key string) (uuid.UUID, error) {
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`SELECT customer_id FROM api_keys WHERE key_hash = sha256($1::bytea)`, []byte(key)).Scan(&id)
	return id, err
}

// A new customer gets an EVM deposit address; deposits start empty; another
// customer's id answers 404 like an unknown one.
func TestDepositAddresses(t *testing.T) {
	id, key := newCustomer(t, "")
	_, other := newCustomer(t, "")

	r := call(t, "GET", "/v1/customers/"+id+"/deposit-addresses", key, "", nil)
	wantStatus(t, r, http.StatusOK)
	addrs := r.json(t)["deposit_addresses"].([]any)
	if len(addrs) != 1 {
		t.Fatalf("deposit addresses %v, want one", addrs)
	}
	a := addrs[0].(map[string]any)
	if a["chain"] != "evm-local" || a["status"] != "active" || len(a["address"].(string)) != 42 {
		t.Fatalf("address %v", a)
	}

	r = call(t, "GET", "/v1/customers/"+id+"/deposits", key, "", nil)
	wantStatus(t, r, http.StatusOK)
	if ds := r.json(t)["deposits"].([]any); len(ds) != 0 {
		t.Fatalf("deposits %v, want none", ds)
	}

	for _, path := range []string{"/deposit-addresses", "/deposits"} {
		wantStatus(t, call(t, "GET", "/v1/customers/"+id+path, other, "", nil), http.StatusNotFound)
	}
}

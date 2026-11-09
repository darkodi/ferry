package api_test

// Noah webhooks end to end: the fake Noah server (signed requests, signed
// webhooks) talks to an API server wired with the Noah rail. A payout is
// moved by webhooks alone, with no resolver. Skipped with -short.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/api"
	"github.com/darkodi/ferry/internal/destination"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/rail/noah"
	"github.com/darkodi/ferry/internal/rail/noah/noahtest"
)

type noahAPI struct {
	t      *testing.T
	srv    *httptest.Server
	fake   *noahtest.Server
	svc    *payout.Service
	worker *payout.Worker
	hook   *ecdsa.PrivateKey // the fake's webhook signing key
	key    string            // the customer's API key
	cust   string
}

func p384(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newNoahAPI starts the fake and an API server using it, and a funded
// customer with a saved bank account.
func newNoahAPI(t *testing.T, funds string) (*noahAPI, string) {
	t.Helper()
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	reqKey, hookKey := p384(t), p384(t)
	fake := noahtest.New("k", &reqKey.PublicKey)
	t.Cleanup(fake.Close)
	signer, err := noah.NewSigner(reqKey, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := noah.NewClient(fake.BaseURL(), "k", signer)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := noah.New(client, noah.Config{CryptoCurrency: noahtest.Crypto}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	dests, err := destination.NewStore(pool, "noah", make([]byte, destination.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	house, err := ledger.OpenHouseAccounts(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&hookKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := noah.NewWebhooks(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	svc := payout.NewService(pool, rl, dests, house, time.Now)
	n := &noahAPI{
		t: t, fake: fake, svc: svc, hook: hookKey,
		worker: payout.NewWorker(svc, queue.NewMemory(), log),
		srv: httptest.NewServer(api.New(api.Config{
			Pool: pool, Payouts: svc, Destinations: dests, Idem: idem, House: house,
			AdminKey: adminKey, Dev: true, RailWebhooks: hooks, Log: log,
		})),
	}
	t.Cleanup(n.srv.Close)
	fake.EnableWebhooks(n.srv.URL+"/v1/webhooks/noah", hookKey, false)

	n.cust, n.key = newCustomer(t, funds) // through the main test server: same database
	r := n.call("POST", "/v1/destinations", n.key, map[string]any{
		"country": "US", "fiat_currency": "USD", "payment_method_type": "BankAch", "form": json.RawMessage(bankForm),
	})
	wantStatus(t, r, http.StatusCreated)
	return n, r.json(t)["id"].(string)
}

func (n *noahAPI) call(method, path, token string, body any) resp {
	n.t.Helper()
	var buf []byte
	if body != nil {
		var err error
		if buf, err = json.Marshal(body); err != nil {
			n.t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(n.t.Context(), method, n.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	res, err := n.srv.Client().Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: out.Bytes()}
}

// submitted creates, confirms and submits a 100 USD payout.
func (n *noahAPI) submitted(dest string) string {
	n.t.Helper()
	r := n.call("POST", "/v1/payouts", n.key, payoutBody(dest, 100_00))
	wantStatus(n.t, r, http.StatusCreated)
	pid := r.json(n.t)["id"].(string)
	wantStatus(n.t, n.call("POST", "/v1/payouts/"+pid+"/confirm", n.key, nil), http.StatusOK)
	p, err := n.svc.Submit(n.t.Context(), uuid.MustParse(pid))
	if err != nil || p.Status != payout.Submitted {
		n.t.Fatalf("submit: %v %v", p.Status, err)
	}
	n.drain(pid) // the creation webhook
	return pid
}

// deliver sends the fake's pending webhooks to the API.
func (n *noahAPI) deliver() {
	n.t.Helper()
	if _, err := n.fake.DeliverWebhooks(); err != nil {
		n.t.Fatal(err)
	}
}

// drain hands the worker every rail.sync event written for the payout,
// as the relay would.
func (n *noahAPI) drain(pid string) {
	n.t.Helper()
	n.deliver()
	rows, err := pool.Query(n.t.Context(),
		`SELECT id, payload FROM outbox WHERE topic = $1 AND aggregate_id = $2 AND published_at IS NULL ORDER BY id`,
		payout.SyncTopic, pid)
	if err != nil {
		n.t.Fatal(err)
	}
	type row struct {
		id   int64
		body []byte
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.body); err != nil {
			n.t.Fatal(err)
		}
		rs = append(rs, r)
	}
	rows.Close()
	for _, r := range rs {
		d := queue.Delivery{Message: queue.Message{ID: strconv.FormatInt(r.id, 10), Topic: payout.SyncTopic, Body: r.body}}
		if err := n.worker.Handle(n.t.Context(), d); err != nil {
			n.t.Fatal(err)
		}
		if _, err := pool.Exec(n.t.Context(), `UPDATE outbox SET published_at = now() WHERE id = $1`, r.id); err != nil {
			n.t.Fatal(err)
		}
	}
}

func (n *noahAPI) status(pid string) string {
	n.t.Helper()
	r := n.call("GET", "/v1/payouts/"+pid, n.key, nil)
	wantStatus(n.t, r, http.StatusOK)
	return r.json(n.t)["status"].(string)
}

func (n *noahAPI) balances() (string, string) {
	n.t.Helper()
	r := n.call("GET", "/v1/customers/"+n.cust+"/balances", n.key, nil)
	wantStatus(n.t, r, http.StatusOK)
	b := r.json(n.t)
	return b["available"].(string), b["held"].(string)
}

func (n *noahAPI) noahTx(pid string) string {
	n.t.Helper()
	for _, tx := range n.fake.Transactions() {
		if tx.ExternalID == pid {
			return tx.ID
		}
	}
	n.t.Fatalf("no Noah transaction for %s", pid)
	return ""
}

func countEvents(t *testing.T, railTxID string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rail_events WHERE rail_tx_id = $1`, railTxID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// postWebhook signs body with key and POSTs it to the API.
func (n *noahAPI) postWebhook(key *ecdsa.PrivateKey, body []byte) int {
	n.t.Helper()
	sig, err := noahtest.SignWebhook(key, body)
	if err != nil {
		n.t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(n.t.Context(), "POST", n.srv.URL+"/v1/webhooks/noah", bytes.NewReader(body))
	req.Header.Set(noah.SignatureHeader, sig)
	res, err := n.srv.Client().Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// ---- tests ----

// Settle by webhook: Noah's Transaction event is stored, a rail.sync is
// queued, and the worker's re-fetch settles the payout. No polling.
func TestNoahWebhookSettlesPayout(t *testing.T) {
	n, dest := newNoahAPI(t, "300")
	pid := n.submitted(dest)
	if err := n.fake.Settle(n.noahTx(pid)); err != nil {
		t.Fatal(err)
	}
	n.drain(pid)
	if s := n.status(pid); s != "settled" {
		t.Fatalf("status %s, want settled", s)
	}
	if a, h := n.balances(); a != "199.500000" || h != "0.000000" {
		t.Fatalf("balances %s / %s, want 199.500000 / 0.000000", a, h)
	}
	if rows := countEvents(t, n.noahTx(pid)); rows != 2 { // created, settled
		t.Fatalf("%d stored events, want 2", rows)
	}
}

// Failure table: "Duplicate webhook" and "Out-of-order webhook". A
// redelivery is stored once and queues nothing; an older version is
// stored but queues nothing; a newer one queues a re-fetch.
func TestNoahWebhookReplayAndOrder(t *testing.T) {
	n, dest := newNoahAPI(t, "300")
	pid := n.submitted(dest)
	tx := n.noahTx(pid)
	ctx := t.Context()
	syncs := func() int {
		var c int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic = $1 AND aggregate_id = $2`, payout.SyncTopic, pid).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := syncs()
	event := func(version int64, status string) []byte {
		return []byte(fmt.Sprintf(`{"Data":{"ID":%q,"Status":%q,"Direction":"Out","CryptoCurrency":"USDC_TEST","ExternalID":%q},`+
			`"EventType":"Transaction","EventVersion":%d,"Occurred":"2026-11-09T12:00:00Z"}`, tx, status, pid, version))
	}
	newer := event(time.Now().Add(time.Hour).UnixMilli(), "Pending")
	for range 3 { // Noah retries the same event
		if code := n.postWebhook(n.hook, newer); code != http.StatusOK {
			t.Fatalf("webhook answered %d", code)
		}
	}
	if got := syncs(); got != before+1 {
		t.Fatalf("%d re-fetches queued for one event delivered 3 times, want 1", got-before)
	}
	// An older event (a stale "Pending" after the newer one): stored, ignored.
	if code := n.postWebhook(n.hook, event(1, "Settled")); code != http.StatusOK {
		t.Fatalf("old webhook answered %d", code)
	}
	if got := syncs(); got != before+1 {
		t.Fatal("an out-of-date event queued a re-fetch")
	}
	var stored int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM rail_events WHERE rail_tx_id = $1 AND event_version = 1`, tx).Scan(&stored)
	if stored != 1 {
		t.Fatal("the out-of-date event was not stored for audit")
	}
}

// Failure table: "Forged webhook". A webhook not signed by Noah's key is
// refused with 401 and leaves no trace; even a correctly signed event
// that claims Settled moves no money until Noah itself says so.
func TestNoahForgedWebhookRefused(t *testing.T) {
	n, dest := newNoahAPI(t, "300")
	pid := n.submitted(dest)
	tx := n.noahTx(pid)
	forged := []byte(fmt.Sprintf(`{"Data":{"ID":%q,"Status":"Settled","Direction":"Out","CryptoCurrency":"USDC_TEST"},`+
		`"EventType":"Transaction","EventVersion":%d}`, tx, time.Now().Add(time.Hour).UnixMilli()))
	if code := n.postWebhook(p384(t), forged); code != http.StatusUnauthorized {
		t.Fatalf("forged webhook answered %d, want 401", code)
	}
	var c int
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM rail_events WHERE payload = $1::jsonb`, string(forged)).Scan(&c)
	if c != 0 {
		t.Fatal("a forged webhook was stored")
	}

	// Signed with the right key but lying about the status: the worker
	// re-fetches from Noah, which still says Pending.
	if code := n.postWebhook(n.hook, forged); code != http.StatusOK {
		t.Fatalf("signed webhook answered %d", code)
	}
	n.drain(pid)
	if s := n.status(pid); s != "submitted" {
		t.Fatalf("status %s: a webhook's claim moved money", s)
	}
	if a, h := n.balances(); a != "198.500000" || h != "101.500000" {
		t.Fatalf("balances changed: %s / %s", a, h)
	}
}

// Failure table: "Webhook for an unknown transaction": stored for audit,
// matched to no payout, nothing queued.
func TestNoahWebhookUnknownTransaction(t *testing.T) {
	n, _ := newNoahAPI(t, "")
	id := uuid.NewString()
	body := []byte(fmt.Sprintf(`{"Data":{"ID":%q,"Status":"Settled"},"EventType":"Transaction","EventVersion":5}`, id))
	if code := n.postWebhook(n.hook, body); code != http.StatusOK {
		t.Fatalf("answered %d", code)
	}
	var payoutID *uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT payout_id FROM rail_events WHERE rail_tx_id = $1`, id).Scan(&payoutID); err != nil {
		t.Fatal(err)
	}
	if payoutID != nil {
		t.Fatalf("matched to payout %s", payoutID)
	}
}

// Failure table: "Bank returns a settled payout" with the real adapter:
// Noah's reversal is a new transaction; its webhook wakes the original
// payout, which is reversed for the returned amount, once.
func TestNoahWebhookReturn(t *testing.T) {
	n, dest := newNoahAPI(t, "300")
	pid := n.submitted(dest)
	tx := n.noahTx(pid)
	if err := n.fake.Settle(tx); err != nil {
		t.Fatal(err)
	}
	n.drain(pid)
	if _, err := n.fake.Reverse(tx, "97"); err != nil {
		t.Fatal(err)
	}
	n.drain(pid)
	if s := n.status(pid); s != "reversed" {
		t.Fatalf("status %s, want reversed", s)
	}
	// 199.50 after the settle, plus the 97 the bank sent back.
	if a, h := n.balances(); a != "296.500000" || h != "0.000000" {
		t.Fatalf("balances %s / %s, want 296.500000 / 0.000000", a, h)
	}
	n.drain(pid) // a repeat does nothing
	if a, _ := n.balances(); a != "296.500000" {
		t.Fatalf("a repeated return credited again: %s", a)
	}
}

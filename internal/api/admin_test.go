package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Reconciliation breaks are operator-only; resolving one needs a real
// reason and works once.
func TestAdminReconBreaks(t *testing.T) {
	_, custKey := newCustomer(t, "")
	ctx := t.Context()
	var runID int64
	if err := pool.QueryRow(ctx, `INSERT INTO recon_runs (window_from, window_to, checks) VALUES (now() - interval '1 day', now(), '{}') RETURNING id`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO recon_breaks (id, check_name, kind, subject, detail, last_run_id)
		VALUES ($1, 'rail', 'balance', $2, '{"rail":"1.000000","ledger":"0.000000"}', $3)`, id, "noah:api-test-"+id.String(), runID); err != nil {
		t.Fatal(err)
	}

	wantStatus(t, call(t, "GET", "/v1/admin/recon-breaks", custKey, "", nil), http.StatusUnauthorized)
	r := call(t, "GET", "/v1/admin/recon-breaks", adminKey, "", nil)
	wantStatus(t, r, http.StatusOK)
	found := false
	for _, b := range r.json(t)["breaks"].([]any) {
		if m := b.(map[string]any); m["id"] == id.String() && m["detail"].(map[string]any)["rail"] == "1.000000" {
			found = true
		}
	}
	if !found {
		t.Fatalf("break %s not listed: %s", id, r.body)
	}

	path := "/v1/admin/recon-breaks/" + id.String() + "/resolve"
	wantStatus(t, call(t, "POST", path, custKey, "", map[string]string{"reason": "looks fine"}), http.StatusUnauthorized)
	wantStatus(t, call(t, "POST", path, adminKey, "", map[string]string{"reason": "ok"}), http.StatusUnprocessableEntity)
	wantStatus(t, call(t, "POST", path, adminKey, "", map[string]string{"reason": "Top-up booked late; see ticket 12"}), http.StatusOK)
	wantStatus(t, call(t, "POST", path, adminKey, "", map[string]string{"reason": "again"}), http.StatusNotFound)
}

// A treasury top-up is booked from Noah's record, once; the operator
// names the transaction, never the amount.
func TestAdminTreasuryTopup(t *testing.T) {
	n, dest := newNoahAPI(t, "300")
	credit := n.fake.Credit("250.5")
	body := map[string]string{"rail_tx_id": credit}

	wantStatus(t, n.call("POST", "/v1/admin/treasury-topups", n.key, body), http.StatusUnauthorized)
	r := n.call("POST", "/v1/admin/treasury-topups", adminKey, body)
	wantStatus(t, r, http.StatusCreated)
	if m := r.json(t); m["amount"] != "250.500000" || m["new"] != true {
		t.Fatalf("top-up %v", m)
	}
	r = n.call("POST", "/v1/admin/treasury-topups", adminKey, body)
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["new"] != false {
		t.Fatal("a repeated top-up was booked again")
	}

	pid := n.submitted(dest)
	wantStatus(t, n.call("POST", "/v1/admin/treasury-topups", adminKey, map[string]string{"rail_tx_id": n.noahTx(pid)}), http.StatusUnprocessableEntity)
	wantStatus(t, n.call("POST", "/v1/admin/treasury-topups", adminKey, map[string]string{"rail_tx_id": uuid.NewString()}), http.StatusNotFound)
	wantStatus(t, n.call("POST", "/v1/admin/treasury-topups", adminKey, map[string]any{"rail_tx_id": credit, "amount": "1000000"}), http.StatusBadRequest)
}

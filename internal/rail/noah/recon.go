package noah

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/rail"
)

var _ rail.Reconcilable = (*Rail)(nil)

// maxRecordPages bounds one listing (100 transactions per page).
const maxRecordPages = 200

// Records implements rail.Reconcilable: every transaction Noah created in
// [from, to), oldest first. It fails rather than return a partial list.
func (r *Rail) Records(ctx context.Context, from, to time.Time) ([]rail.Record, error) {
	q := url.Values{}
	q.Set("CreatedFrom", from.UTC().Format(time.RFC3339))
	q.Set("CreatedTo", to.UTC().Format(time.RFC3339))
	q.Set("SortDirection", "ASC")
	q.Set("PageSize", "100")
	var out []rail.Record
	for range maxRecordPages {
		var page txListResp
		if err := r.c.do(ctx, http.MethodGet, "/transactions", q, nil, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Items {
			// CreatedTo is inclusive at Noah; the window is half-open.
			if !t.Created.Before(to) {
				continue
			}
			rec, err := r.toRecord(t)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
		if page.PageToken == "" {
			return out, nil
		}
		q.Set("PageToken", page.PageToken)
	}
	return nil, fmt.Errorf("noah: more than %d pages of transactions between %s and %s; use a shorter window", maxRecordPages, from, to)
}

// Record implements rail.Reconcilable.
func (r *Rail) Record(ctx context.Context, id string) (rail.Record, bool, error) {
	var out txResp
	err := r.c.do(ctx, http.MethodGet, "/transactions/"+url.PathEscape(id), nil, nil, &out)
	if statusOf(err) == http.StatusNotFound {
		return rail.Record{}, false, nil
	}
	if err != nil {
		return rail.Record{}, false, err
	}
	if out.Transaction.ID != id {
		return rail.Record{}, false, fmt.Errorf("noah: asked for transaction %s, got %q", id, out.Transaction.ID)
	}
	rec, err := r.toRecord(out.Transaction)
	return rec, err == nil, err
}

// toRecord maps any Noah transaction. A pending one may carry no amount
// yet; that is 0, not an error.
func (r *Rail) toRecord(t Transaction) (rail.Record, error) {
	rec := rail.Record{
		ID: t.ID, ExternalID: t.ExternalID, Direction: t.Direction, Reverses: t.Reverses,
		Currency: t.CryptoCurrency, Created: t.Created,
	}
	switch t.Status {
	case "Pending":
		rec.Status = rail.StatusPending
	case "Settled":
		rec.Status = rail.StatusSettled
	case "Failed":
		rec.Status = rail.StatusFailed
	default:
		return rail.Record{}, fmt.Errorf("noah: transaction %s has unknown status %q", t.ID, t.Status)
	}
	if t.Amount != "" {
		a, err := parseAmount(t.Amount)
		if err != nil {
			return rail.Record{}, fmt.Errorf("noah: transaction %s: %w", t.ID, err)
		}
		rec.Amount = a
	}
	return rec, nil
}

type balancesResp struct {
	Items []struct {
		AccountType    string `json:"AccountType"`
		CryptoCurrency string `json:"CryptoCurrency"`
		Available      string `json:"Available"`
		Total          string `json:"Total"`
	} `json:"Items"`
}

// Balance implements rail.Reconcilable: the Total of Ferry's current
// account in the configured currency. Total includes outflows that have
// not settled, which matches the ledger: noah_prefunded only moves when a
// sale settles.
func (r *Rail) Balance(ctx context.Context) (ledger.Amount, error) {
	var out balancesResp
	if err := r.c.do(ctx, http.MethodGet, "/balances", nil, nil, &out); err != nil {
		return 0, err
	}
	for _, b := range out.Items {
		if b.CryptoCurrency == r.cfg.CryptoCurrency && b.AccountType == "Current" {
			return parseAmount(b.Total)
		}
	}
	return 0, errors.New("noah: no Current balance for " + r.cfg.CryptoCurrency)
}

// Currency is the rail's code for what Ferry holds (USDC_TEST in sandbox).
func (r *Rail) Currency() string { return r.cfg.CryptoCurrency }

package ledger

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := func() Entry {
		return Entry{
			IdempotencyKey: "k",
			Type:           TypeDeposit,
			Postings: []Posting{
				{Account: 1, Currency: USDC, Direction: Debit, Amount: 100},
				{Account: 2, Currency: USDC, Direction: Credit, Amount: 100},
			},
		}
	}
	var one EntryID = 1

	tests := []struct {
		name   string
		mutate func(e *Entry)
		valid  bool
	}{
		{"balanced", func(*Entry) {}, true},
		{"one debit two credits", func(e *Entry) {
			e.Postings[1].Amount = 60
			e.Postings = append(e.Postings, Posting{Account: 3, Currency: USDC, Direction: Credit, Amount: 40})
		}, true},
		{"correction may reverse", func(e *Entry) { e.Type = TypeCorrection; e.Reverses = &one }, true},

		{"unbalanced", func(e *Entry) { e.Postings[1].Amount = 99 }, false},
		{"single posting", func(e *Entry) { e.Postings = e.Postings[:1] }, false},
		{"no postings", func(e *Entry) { e.Postings = nil }, false},
		{"zero amount", func(e *Entry) { e.Postings[0].Amount = 0; e.Postings[1].Amount = 0 }, false},
		{"negative amounts that balance", func(e *Entry) { e.Postings[0].Amount = -5; e.Postings[1].Amount = -5 }, false},
		{"unknown direction", func(e *Entry) { e.Postings[0].Direction = "sideways" }, false},
		{"unsupported currency", func(e *Entry) { e.Postings[0].Currency = "USD"; e.Postings[1].Currency = "USD" }, false},
		{"missing account", func(e *Entry) { e.Postings[0].Account = 0 }, false},
		{"empty key", func(e *Entry) { e.IdempotencyKey = "" }, false},
		{"key too long", func(e *Entry) { e.IdempotencyKey = strings.Repeat("k", 201) }, false},
		{"unknown type", func(e *Entry) { e.Type = "submit" }, false},
		{"non-correction reverses", func(e *Entry) { e.Reverses = &one }, false},
		{"sum overflows", func(e *Entry) {
			e.Postings = []Posting{
				{Account: 1, Currency: USDC, Direction: Debit, Amount: math.MaxInt64},
				{Account: 2, Currency: USDC, Direction: Debit, Amount: math.MaxInt64},
				{Account: 3, Currency: USDC, Direction: Credit, Amount: math.MaxInt64},
				{Account: 4, Currency: USDC, Direction: Credit, Amount: math.MaxInt64},
			}
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := ok()
			tc.mutate(&e)
			err := e.Validate()
			if tc.valid && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !tc.valid && !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("want ErrInvalidEntry, got %v", err)
			}
		})
	}
}

func TestKinds(t *testing.T) {
	tests := []struct {
		kind     Kind
		customer bool
		normal   Direction
	}{
		{KindAvailable, true, Credit},
		{KindHeld, true, Credit},
		{KindChainOmnibus, false, Debit},
		{KindNoahPrefunded, false, Debit},
		{KindFees, false, Credit},
	}
	for _, tc := range tests {
		if !tc.kind.Valid() || tc.kind.IsCustomer() != tc.customer || tc.kind.NormalSide() != tc.normal {
			t.Errorf("%s: valid=%v customer=%v normal=%s", tc.kind, tc.kind.Valid(), tc.kind.IsCustomer(), tc.kind.NormalSide())
		}
	}
	for _, k := range []Kind{"in_transit", "return_fees", ""} {
		if k.Valid() {
			t.Errorf("%q should not be a valid kind", k)
		}
	}
}

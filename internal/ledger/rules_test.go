package ledger

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

var (
	testHouse    = HouseAccounts{ChainOmnibus: 1, NoahPrefunded: 2, Fees: 3}
	testCustomer = CustomerAccounts{Available: 4, Held: 5}
	testPayout   = uuid.MustParse("9f1c0000-0000-4000-8000-000000000001")
)

func TestRules(t *testing.T) {
	settle, err := Settle(testPayout, testHouse, testCustomer, 50_200_000, 49_500_000, 300_000)
	if err != nil {
		t.Fatal(err)
	}
	settleExact, err := Settle(testPayout, testHouse, testCustomer, 49_800_000, 49_500_000, 300_000)
	if err != nil {
		t.Fatal(err)
	}
	deposit := uuid.MustParse("de000000-0000-4000-8000-000000000001")

	tests := []struct {
		name     string
		entry    Entry
		key      string
		typ      EntryType
		postings []Posting
	}{
		{
			"deposit",
			Deposit("evm-sepolia", "0xab12", 3, deposit, testHouse, testCustomer, 100),
			"deposit:evm-sepolia:0xab12:3", TypeDeposit,
			[]Posting{line(1, Debit, 100), line(4, Credit, 100)},
		},
		{
			"treasury top-up",
			TreasuryTopup("0xcd34", testHouse, 60),
			"topup:0xcd34", TypeTreasuryTopup,
			[]Posting{line(2, Debit, 60), line(1, Credit, 60)},
		},
		{
			"hold",
			Hold(testPayout, testCustomer, 50),
			"payout:" + testPayout.String() + ":hold", TypeHold,
			[]Posting{line(4, Debit, 50), line(5, Credit, 50)},
		},
		{
			"settle with remainder",
			settle,
			"payout:" + testPayout.String() + ":settle", TypeSettle,
			[]Posting{line(5, Debit, 50_200_000), line(2, Credit, 49_800_000), line(4, Credit, 400_000)},
		},
		{
			"settle without remainder omits the zero line",
			settleExact,
			"payout:" + testPayout.String() + ":settle", TypeSettle,
			[]Posting{line(5, Debit, 49_800_000), line(2, Credit, 49_800_000)},
		},
		{
			"release",
			Release(testPayout, testCustomer, 50),
			"payout:" + testPayout.String() + ":release", TypeRelease,
			[]Posting{line(5, Debit, 50), line(4, Credit, 50)},
		},
		{
			"bank return",
			BankReturn(testPayout, "rtx-9", testHouse, testCustomer, 45),
			"payout:" + testPayout.String() + ":return:rtx-9", TypeBankReturn,
			[]Posting{line(2, Debit, 45), line(4, Credit, 45)},
		},
		{
			"mirror swaps directions",
			Mirror("break-7", 42, []Posting{line(1, Debit, 100), line(4, Credit, 100)}),
			"correction:break-7", TypeCorrection,
			[]Posting{line(1, Credit, 100), line(4, Debit, 100)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.entry.IdempotencyKey != tc.key {
				t.Errorf("key = %q, want %q", tc.entry.IdempotencyKey, tc.key)
			}
			if tc.entry.Type != tc.typ {
				t.Errorf("type = %q, want %q", tc.entry.Type, tc.typ)
			}
			if !reflect.DeepEqual(tc.entry.Postings, tc.postings) {
				t.Errorf("postings = %+v, want %+v", tc.entry.Postings, tc.postings)
			}
			if err := tc.entry.Validate(); err != nil {
				t.Errorf("built entry is invalid: %v", err)
			}
		})
	}
}

func TestMirrorLinksOriginal(t *testing.T) {
	e := Mirror("break-7", 42, []Posting{line(1, Debit, 100), line(4, Credit, 100)})
	if e.Reverses == nil || *e.Reverses != 42 {
		t.Fatalf("Reverses = %v, want 42", e.Reverses)
	}
}

func TestSettleRejectsSpendAboveAuthorized(t *testing.T) {
	_, err := Settle(testPayout, testHouse, testCustomer, 50, 49, 2)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("want ErrInvalidEntry, got %v", err)
	}
}

package evm_test

import (
	"testing"

	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/ledger"
)

// Reconciliation reads balanceOf at a confirmed point no later than the
// watcher's cursor, so a newer transfer is not counted there yet.
func TestBalancesReadAtPoint(t *testing.T) {
	c := newChain(t)
	_, addr := c.newCustomer()
	c.call(c.usdc, "transfer", addr, usdc(7))
	c.mine(1)
	first := c.head()
	c.call(c.usdc, "transfer", addr, usdc(3))
	c.mine(1)

	b := evm.NewBalances(c.name, c.client, c.usdc, 1)
	point, err := b.Point(t.Context(), first+10) // cursor ahead: capped by confirmations
	if err != nil || point != c.head()-1 {
		t.Fatalf("point %d, %v; want head-1 = %d", point, err, c.head()-1)
	}
	if point, _ = b.Point(t.Context(), first-1); point != first-1 {
		t.Fatalf("point %d, want the cursor %d", point, first-1)
	}
	for _, tc := range []struct {
		at   uint64
		want ledger.Amount
	}{{first - 1, 0}, {first, 7_000000}, {c.head(), 10_000000}} {
		got, err := b.Balance(t.Context(), addr.Hex(), tc.at)
		if err != nil || got != tc.want {
			t.Errorf("balance at %d = %s, %v; want %s", tc.at, got, err, tc.want)
		}
	}
	if _, err := b.Balance(t.Context(), "not-an-address", c.head()); err == nil {
		t.Error("accepted a malformed address")
	}
}

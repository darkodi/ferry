package evm_test

import (
	"math/big"
	"testing"

	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/ledger"
)

// Account xpub m/44'/60'/0' of the public test mnemonic
// "test test test test test test test test test test test junk". Its
// addresses are Anvil's and Hardhat's well-known default accounts, so the
// vector can be checked against any wallet.
const anvilXpub = "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP"

func TestDeriveKnownAddresses(t *testing.T) {
	d, err := evm.NewDeriver("evm-local", anvilXpub)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{
		"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		"0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
	} {
		got, status, err := d.Derive(uint64(i))
		if err != nil || got != want || status != "active" {
			t.Errorf("index %d: %s %s %v, want %s active", i, got, status, err, want)
		}
	}
}

func TestDeriverRejectsBadKeys(t *testing.T) {
	// A private extended key must be refused: the deriver may hold public
	// keys only. This is the master key of BIP-32 test vector 1, published
	// in the BIP itself; it guards nothing.
	const xprv = "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi" // gitleaks:allow
	for _, k := range []string{"", "not a key", xprv} {
		if _, err := evm.NewDeriver("evm-local", k); err == nil {
			t.Errorf("NewDeriver(%q) accepted it", k)
		}
	}
}

func TestParseAmount(t *testing.T) {
	word := func(v *big.Int) []byte { return v.FillBytes(make([]byte, 32)) }
	maxInt64 := new(big.Int).SetInt64(1<<63 - 1)
	for _, tc := range []struct {
		name string
		data []byte
		want ledger.Amount
		ok   bool
	}{
		{"one cent", word(big.NewInt(10_000)), 10_000, true},
		{"largest int64", word(maxInt64), 1<<63 - 1, true},
		{"zero", word(big.NewInt(0)), 0, false},
		{"above int64", word(new(big.Int).Add(maxInt64, big.NewInt(1))), 0, false},
		{"2^256-1", word(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))), 0, false},
		{"short", []byte{1, 2, 3}, 0, false},
		{"long", make([]byte, 64), 0, false},
	} {
		got, ok := evm.ParseAmount(tc.data)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %d %v, want %d %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// FuzzParseAmount: any 32 bytes either decode to a positive int64 or are
// refused; nothing panics or wraps around.
func FuzzParseAmount(f *testing.F) {
	f.Add(make([]byte, 32))
	f.Add(big.NewInt(1).FillBytes(make([]byte, 32)))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, ok := evm.ParseAmount(data)
		if ok && got <= 0 {
			t.Fatalf("accepted non-positive %d", got)
		}
		if ok && new(big.Int).SetBytes(data).Int64() != int64(got) {
			t.Fatalf("value changed: %x -> %d", data, got)
		}
	})
}

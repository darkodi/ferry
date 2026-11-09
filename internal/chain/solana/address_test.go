package solana_test

import (
	"crypto/sha256"
	"testing"

	sol "github.com/gagliardetto/solana-go"

	"github.com/darkodi/ferry/internal/chain/solana"
)

// The address is sha256(authority || seed || token program), the rule the
// system program applies in createAccountWithSeed. Recomputing it by hand
// pins the derivation: a change here would strand every customer's funds
// at addresses Ferry no longer watches.
func TestDepositAddressDerivation(t *testing.T) {
	authority := testKey(9)
	d, err := solana.NewDeriver("solana-test", authority)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, idx := range []uint64{0, 1, 2, 41, 1 << 40} {
		addr, status, err := d.Derive(idx)
		if err != nil {
			t.Fatal(err)
		}
		if status != "provisioning" {
			t.Errorf("status = %q, want provisioning", status)
		}
		h := sha256.Sum256(append(append(authority.Bytes(), solana.Seed(idx)...), sol.TokenProgramID.Bytes()...))
		if want := sol.PublicKeyFromBytes(h[:]).String(); addr != want {
			t.Errorf("index %d: address %s, want %s", idx, addr, want)
		}
		if seen[addr] {
			t.Errorf("index %d: address %s repeats", idx, addr)
		}
		seen[addr] = true
	}
	if got := solana.Seed(18446744073709551615); len(got) > sol.MaxSeedLength {
		t.Errorf("largest seed %q is longer than %d bytes", got, sol.MaxSeedLength)
	}
}

func TestDeriverRejectsEmptyAuthority(t *testing.T) {
	if _, err := solana.NewDeriver("solana-test", sol.PublicKey{}); err == nil {
		t.Fatal("NewDeriver accepted an all-zero authority")
	}
}

// The provisioning key must never be the deposit owner, or it could move
// customers' USDC.
func TestProvisionerRefusesAuthorityAsOwner(t *testing.T) {
	key, err := sol.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := solana.NewProvisioner(nil, key, usdcMint, key.PublicKey()); err == nil {
		t.Fatal("NewProvisioner accepted the authority as deposit owner")
	}
	if _, err := solana.NewProvisioner(nil, key, usdcMint, sol.PublicKey{}); err == nil {
		t.Fatal("NewProvisioner accepted an empty deposit owner")
	}
	if _, err := solana.NewProvisioner(nil, key, usdcMint, testKey(8)); err != nil {
		t.Fatalf("NewProvisioner with a distinct owner: %v", err)
	}
}

package evm

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/ethereum/go-ethereum/crypto"
)

// Deriver derives one deposit address per customer index from an extended
// public key (xpub), BIP-32 non-hardened derivation. Holding only the xpub,
// Ferry can compute every address but cannot spend from any of them: the
// matching private key stays with whoever sweeps the funds (KMS in M7).
//
// The xpub should be the account-level key m/44'/60'/0', so customer i
// gets m/44'/60'/0'/0/i — the path wallets such as MetaMask use, which
// makes addresses easy to check by hand.
type Deriver struct {
	name     string
	external *hdkeychain.ExtendedKey // xpub/0
}

// NewDeriver parses xpub for the chain named name.
func NewDeriver(name, xpub string) (*Deriver, error) {
	k, err := hdkeychain.NewKeyFromString(xpub)
	if err != nil {
		return nil, fmt.Errorf("evm: parse xpub: %w", err)
	}
	if k.IsPrivate() {
		// The API and watcher must never hold a private key.
		return nil, errors.New("evm: expected an extended public key (xpub), got a private one")
	}
	ext, err := k.Derive(0)
	if err != nil {
		return nil, fmt.Errorf("evm: derive external chain: %w", err)
	}
	return &Deriver{name: name, external: ext}, nil
}

// Chain returns the chain name.
func (d *Deriver) Chain() string { return d.name }

// Derive returns the checksummed address for index. EVM addresses are
// usable at once, so the status is "active".
func (d *Deriver) Derive(index uint64) (string, string, error) {
	if index >= hdkeychain.HardenedKeyStart {
		return "", "", fmt.Errorf("evm: index %d out of range", index)
	}
	child, err := d.external.Derive(uint32(index))
	if err != nil {
		return "", "", fmt.Errorf("evm: derive index %d: %w", index, err)
	}
	pub, err := child.ECPubKey()
	if err != nil {
		return "", "", fmt.Errorf("evm: public key %d: %w", index, err)
	}
	ecdsa := pub.ToECDSA()
	return crypto.PubkeyToAddress(*ecdsa).Hex(), "active", nil
}

package solana_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"

	"github.com/darkodi/ferry/internal/chain/solana"
)

// rawEd25519 stands in for a KMS Ed25519 key: it signs raw messages.
type rawEd25519 struct{ k ed25519.PrivateKey }

func (r rawEd25519) Public() ed25519.PublicKey { return r.k.Public().(ed25519.PublicKey) }
func (r rawEd25519) Sign(_ context.Context, msg []byte) ([]byte, error) {
	return ed25519.Sign(r.k, msg), nil
}

// A transaction signed through the KMS adapter (a raw Ed25519 signature
// over the serialized message) is a valid Solana transaction, the same as
// one signed with the keypair itself.
func TestKMSSignerSignsTransactions(t *testing.T) {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, signer := range map[string]solana.TxSigner{
		"kms":     solana.KMSSigner(rawEd25519{k}),
		"keypair": solana.KeySigner(sol.PrivateKey(k)),
	} {
		t.Run(name, func(t *testing.T) {
			payer := signer.PublicKey()
			if payer != sol.PublicKeyFromBytes(k.Public().(ed25519.PublicKey)) {
				t.Fatal("public key differs from the key's")
			}
			tx, err := sol.NewTransaction([]sol.Instruction{system.NewTransferInstruction(1, payer, testKey(3)).Build()},
				sol.Hash{1}, sol.TransactionPayer(payer))
			if err != nil {
				t.Fatal(err)
			}
			msg, err := tx.Message.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			sig, err := signer.SignMessage(t.Context(), msg)
			if err != nil {
				t.Fatal(err)
			}
			tx.Signatures = []sol.Signature{sig}
			if err := tx.VerifySignatures(); err != nil {
				t.Fatalf("signature does not verify: %v", err)
			}
		})
	}
}

// The provisioner refuses an authority that is also the deposit owner,
// whatever kind of signer it is.
func TestProvisionerSignerOwnerCheck(t *testing.T) {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	s := solana.KMSSigner(rawEd25519{k})
	if _, err := solana.NewProvisionerWithSigner(nil, s, usdcMint, s.PublicKey()); err == nil {
		t.Fatal("authority accepted as deposit owner")
	}
	if _, err := solana.NewProvisionerWithSigner(nil, nil, usdcMint, testKey(8)); err == nil {
		t.Fatal("nil signer accepted")
	}
}

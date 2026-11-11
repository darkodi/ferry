package solana

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	bin "github.com/gagliardetto/binary"
	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
)

// TokenAccountSize is the size of an SPL Token account.
const TokenAccountSize = 165

// Provisioner creates customer deposit token accounts on-chain. It is the
// only part of Ferry that holds a Solana private key, the authority's, and
// that key only creates accounts and pays their rent: each account is
// allocated with Ferry's seed, owned by the Token program, and initialised
// for the USDC mint with a separate deposit owner as token owner. Only the
// deposit owner can move tokens out, and Ferry holds just its public key
// (its private key belongs to the sweep, KMS in M7). Leaking the authority
// key therefore exposes the SOL it holds for rent, not customer USDC. It
// implements chain.Provisioner.
type Provisioner struct {
	rpc       *rpc.Client
	signer    TxSigner
	authority sol.PublicKey
	mint      sol.PublicKey
	owner     sol.PublicKey

	// Timeout bounds how long Provision waits for its transaction to be
	// confirmed. Past it, the next round checks the account again.
	Timeout time.Duration
}

// NewProvisioner returns a provisioner signing with key, the authority
// whose public key the API derives addresses from, creating USDC (mint)
// accounts whose token owner is owner. The owner must not be the
// authority: that would let the provisioning key spend deposits.
func NewProvisioner(client *rpc.Client, key sol.PrivateKey, mint, owner sol.PublicKey) (*Provisioner, error) {
	if !key.IsValid() {
		return nil, errors.New("solana: invalid authority private key")
	}
	return NewProvisionerWithSigner(client, KeySigner(key), mint, owner)
}

// NewProvisionerWithSigner is NewProvisioner for an authority that signs
// elsewhere, such as a KMS key (KMSSigner).
func NewProvisionerWithSigner(client *rpc.Client, signer TxSigner, mint, owner sol.PublicKey) (*Provisioner, error) {
	if signer == nil || signer.PublicKey().IsZero() {
		return nil, errors.New("solana: no authority signer")
	}
	if owner.IsZero() || owner == signer.PublicKey() {
		return nil, errors.New("solana: the deposit owner must be set and differ from the provisioning authority")
	}
	return &Provisioner{rpc: client, signer: signer, authority: signer.PublicKey(), mint: mint, owner: owner, Timeout: 60 * time.Second}, nil
}

// TxSigner signs transaction messages for the authority: a keypair in
// memory (KeySigner, local development) or a key held in AWS KMS
// (KMSSigner), which Ferry never sees.
type TxSigner interface {
	PublicKey() sol.PublicKey
	SignMessage(ctx context.Context, msg []byte) (sol.Signature, error)
}

// KeySigner signs with a private key in memory.
func KeySigner(k sol.PrivateKey) TxSigner { return keySigner{k} }

type keySigner struct{ k sol.PrivateKey }

func (s keySigner) PublicKey() sol.PublicKey { return s.k.PublicKey() }
func (s keySigner) SignMessage(_ context.Context, msg []byte) (sol.Signature, error) {
	sig, err := s.k.Sign(msg)
	if err != nil {
		return sol.Signature{}, fmt.Errorf("solana: sign: %w", err)
	}
	return sig, nil
}

// Ed25519Signer is a raw Ed25519 signer, such as awskms.Ed25519Signer.
type Ed25519Signer interface {
	Public() ed25519.PublicKey
	Sign(ctx context.Context, msg []byte) ([]byte, error)
}

// KMSSigner adapts an Ed25519 signer (a KMS ECC_NIST_EDWARDS25519 key) to
// sign Solana transactions: a Solana signature is a plain Ed25519
// signature over the serialized message.
func KMSSigner(s Ed25519Signer) TxSigner { return kmsSigner{s} }

type kmsSigner struct{ s Ed25519Signer }

func (k kmsSigner) PublicKey() sol.PublicKey { return sol.PublicKeyFromBytes(k.s.Public()) }
func (k kmsSigner) SignMessage(ctx context.Context, msg []byte) (sol.Signature, error) {
	sig, err := k.s.Sign(ctx, msg)
	if err != nil {
		return sol.Signature{}, fmt.Errorf("solana: sign: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return sol.Signature{}, fmt.Errorf("solana: signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	return sol.SignatureFromBytes(sig), nil
}

// Authority returns the public key the provisioner signs with.
func (p *Provisioner) Authority() sol.PublicKey { return p.authority }

// Provision makes sure the deposit account for index exists at address and
// is Ferry's USDC token account. It is idempotent: an account that already
// exists is only checked. It returns nil once the account is ready.
func (p *Provisioner) Provision(ctx context.Context, index uint64, address string) error {
	addr, err := DepositAddress(p.authority, index)
	if err != nil {
		return err
	}
	if addr.String() != address {
		// The API and the watcher use different authorities: refuse rather
		// than create an account nobody will watch.
		return fmt.Errorf("solana: address %s for index %d does not match authority %s (got %s)", address, index, p.authority, addr)
	}
	st, lamports, err := p.state(ctx, addr)
	if err != nil || st == ready {
		return err
	}
	ixs, err := p.instructions(ctx, addr, index, st, lamports)
	if err != nil {
		return err
	}
	if err := p.send(ctx, ixs); err != nil {
		return err
	}
	if st, _, err = p.state(ctx, addr); err != nil {
		return err
	}
	if st != ready {
		return fmt.Errorf("solana: account %s not visible after provisioning", addr)
	}
	return nil
}

type accountState int

const (
	missing   accountState = iota // no account at the address
	prefunded                     // someone sent SOL there: a system account with no data
	ready                         // Ferry's USDC token account
)

// state reads the account at addr. Any account that is neither missing,
// a bare pre-funded system account, nor exactly Ferry's USDC token account
// is an error: the address is then not safe to use.
func (p *Provisioner) state(ctx context.Context, addr sol.PublicKey) (accountState, uint64, error) {
	res, err := p.rpc.GetAccountInfoWithOpts(ctx, addr, &rpc.GetAccountInfoOpts{
		Encoding: sol.EncodingBase64, Commitment: rpc.CommitmentConfirmed,
	})
	if errors.Is(err, rpc.ErrNotFound) {
		return missing, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("solana: account %s: %w", addr, err)
	}
	acct := res.Value
	data := acct.Data.GetBinary()
	switch {
	case acct.Owner == sol.SystemProgramID && len(data) == 0:
		return prefunded, acct.Lamports, nil
	case acct.Owner != sol.TokenProgramID || len(data) != TokenAccountSize:
		return 0, 0, fmt.Errorf("solana: account %s exists but is not a token account (owner %s)", addr, acct.Owner)
	}
	var ta token.Account
	if err := bin.NewBinDecoder(data).Decode(&ta); err != nil {
		return 0, 0, fmt.Errorf("solana: decode token account %s: %w", addr, err)
	}
	if ta.Mint != p.mint || ta.Owner != p.owner || ta.State != token.Initialized ||
		ta.Delegate != nil || ta.CloseAuthority != nil || ta.IsNative != nil {
		return 0, 0, fmt.Errorf("solana: token account %s is not Ferry's USDC account (mint %s, owner %s, state %d)",
			addr, ta.Mint, ta.Owner, ta.State)
	}
	return ready, acct.Lamports, nil
}

// instructions builds the account creation. A missing account is created
// with createAccountWithSeed. A pre-funded one cannot be (the system
// program refuses an address that holds lamports), so it is topped up to
// rent exemption and allocated with allocateWithSeed instead; this way a
// stranger's dust cannot block a customer's address.
func (p *Provisioner) instructions(ctx context.Context, addr sol.PublicKey, index uint64, st accountState, have uint64) ([]sol.Instruction, error) {
	rent, err := p.rpc.GetMinimumBalanceForRentExemption(ctx, TokenAccountSize, rpc.CommitmentConfirmed)
	if err != nil {
		return nil, fmt.Errorf("solana: rent exemption: %w", err)
	}
	seed := Seed(index)
	var ixs []sol.Instruction
	if st == missing {
		ixs = append(ixs, system.NewCreateAccountWithSeedInstruction(
			p.authority, seed, rent, TokenAccountSize, sol.TokenProgramID, p.authority, addr, p.authority).Build())
	} else {
		if have < rent {
			ixs = append(ixs, system.NewTransferInstruction(rent-have, p.authority, addr).Build())
		}
		ixs = append(ixs, system.NewAllocateWithSeedInstruction(
			p.authority, seed, TokenAccountSize, sol.TokenProgramID, addr, p.authority).Build())
	}
	ixs = append(ixs, token.NewInitializeAccount3Instruction(p.owner, addr, p.mint).Build())
	return ixs, nil
}

// send signs ixs with the authority (fee payer and seed base), submits
// them and waits until the transaction is confirmed or its blockhash
// expires.
func (p *Provisioner) send(ctx context.Context, ixs []sol.Instruction) error {
	bh, err := p.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("solana: latest blockhash: %w", err)
	}
	tx, err := sol.NewTransaction(ixs, bh.Value.Blockhash, sol.TransactionPayer(p.authority))
	if err != nil {
		return fmt.Errorf("solana: build transaction: %w", err)
	}
	// The authority is the only signer (fee payer and seed base). Sign
	// the serialized message, then check the result before sending.
	if tx.Message.Header.NumRequiredSignatures != 1 || len(tx.Message.AccountKeys) == 0 || tx.Message.AccountKeys[0] != p.authority {
		return errors.New("solana: provisioning transaction must have the authority as its only signer")
	}
	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		return fmt.Errorf("solana: serialize transaction: %w", err)
	}
	signature, err := p.signer.SignMessage(ctx, msg)
	if err != nil {
		return err
	}
	tx.Signatures = []sol.Signature{signature}
	if err := tx.VerifySignatures(); err != nil {
		return fmt.Errorf("solana: provisioning transaction signature: %w", err)
	}
	sig, err := p.rpc.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{PreflightCommitment: rpc.CommitmentConfirmed})
	if err != nil {
		return fmt.Errorf("solana: send provisioning transaction: %w", err)
	}
	return p.wait(ctx, sig, bh.Value.LastValidBlockHeight)
}

func (p *Provisioner) wait(ctx context.Context, sig sol.Signature, lastValid uint64) error {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		res, err := p.rpc.GetSignatureStatuses(ctx, false, sig)
		if err == nil && len(res.Value) == 1 && res.Value[0] != nil {
			s := res.Value[0]
			if s.Err != nil {
				return fmt.Errorf("solana: provisioning transaction %s failed: %v", sig, s.Err)
			}
			if s.ConfirmationStatus == rpc.ConfirmationStatusConfirmed || s.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
				return nil
			}
		}
		if h, err := p.rpc.GetBlockHeight(ctx, rpc.CommitmentConfirmed); err == nil && h > lastValid {
			return fmt.Errorf("solana: provisioning transaction %s expired", sig)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("solana: waiting for %s: %w", sig, ctx.Err())
		case <-tick.C:
		}
	}
}

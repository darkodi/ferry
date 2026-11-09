package solana_test

// Watcher tests on a real solana-test-validator and a throwaway Postgres,
// both in Docker. Each test gets its own mint, Ferry authority and chain
// name, so cursors, addresses and deposits do not mix. Skipped with -short.
//
// The watcher reads only finalized slots, which trail the tip by about 30
// slots (~12 s), so each test waits for its transactions to finalize.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/chain/solana"
	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
	"github.com/darkodi/ferry/internal/testutil/soltest"
)

var (
	pool   *pgxpool.Pool
	house  ledger.HouseAccounts
	rpcURL string
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	val, err := soltest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test validator:", err)
		return 1
	}
	defer val.Close()
	rpcURL = val.URL
	tdb, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test database:", err)
		return 1
	}
	defer tdb.Close()
	if pool, err = pgxpool.New(ctx, tdb.DSN); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	if house, err = ledger.OpenHouseAccounts(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// ---- a test chain ----

const decimals = 6

type testChain struct {
	t        *testing.T
	client   *rpc.Client
	payer    sol.PrivateKey // funds tests, mint authority, owns source
	auth     sol.PrivateKey // Ferry's authority: seed base, pays rent
	owner    sol.PublicKey  // deposit owner: token owner of deposit accounts
	mint     sol.PublicKey  // this test's USDC
	source   sol.PublicKey  // payer's USDC token account, 1,000 USDC
	name     string
	deriver  *solana.Deriver
	prov     *solana.Provisioner
	watcher  *chain.Watcher
	startPos int64 // the cursor's first slot, for rescans
}

func newChain(t *testing.T) *testChain {
	t.Helper()
	if pool == nil {
		t.Skip("validator and database tests need Docker; skipped with -short")
	}
	c := &testChain{t: t, client: rpc.New(rpcURL), payer: newKey(t), auth: newKey(t), owner: newKey(t).PublicKey()}
	c.airdrop(c.payer.PublicKey())
	c.airdrop(c.auth.PublicKey())

	mint := newKey(t)
	c.mint = mint.PublicKey()
	c.send([]sol.PrivateKey{mint}, append(c.createAccount(c.mint, 82),
		token.NewInitializeMint2Instruction(decimals, c.payer.PublicKey(), sol.PublicKey{}, c.mint).Build())...)
	src := newKey(t)
	c.source = src.PublicKey()
	c.send([]sol.PrivateKey{src}, append(c.createAccount(c.source, solana.TokenAccountSize),
		token.NewInitializeAccount3Instruction(c.payer.PublicKey(), c.source, c.mint).Build(),
		token.NewMintToInstruction(usdc(1000), c.mint, c.source, c.payer.PublicKey(), nil).Build())...)

	// The name must fit the deposit_addresses chain check: [a-z0-9-].
	c.name = "sol-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	if len(c.name) > 32 {
		c.name = c.name[:32]
	}
	var err error
	if c.deriver, err = solana.NewDeriver(c.name, c.auth.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if c.prov, err = solana.NewProvisioner(c.client, c.auth, c.mint, c.owner); err != nil {
		t.Fatal(err)
	}
	c.watcher = chain.NewWatcher(pool, solana.New(c.name, c.client, c.mint, c.owner), house, slog.New(slog.DiscardHandler))
	c.watcher.Confirmations = 0
	c.watcher.Provisioner = c.prov
	head, err := c.client.GetSlot(t.Context(), rpc.CommitmentFinalized)
	if err != nil {
		t.Fatal(err)
	}
	c.watcher.StartBlock = head + 1
	c.startPos = int64(head)
	t.Cleanup(func() { checkInvariants(t) })
	return c
}

func newKey(t *testing.T) sol.PrivateKey {
	t.Helper()
	k, err := sol.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func usdc(units uint64) uint64 { return units * 1_000_000 }

func (c *testChain) airdrop(to sol.PublicKey) {
	c.t.Helper()
	sig, err := c.client.RequestAirdrop(c.t.Context(), to, 10*sol.LAMPORTS_PER_SOL, rpc.CommitmentConfirmed)
	if err != nil {
		c.t.Fatal(err)
	}
	c.wait(sig, rpc.ConfirmationStatusConfirmed)
}

// createAccount returns instructions creating a Token-program account of
// size bytes at addr, paid by payer.
func (c *testChain) createAccount(addr sol.PublicKey, size uint64) []sol.Instruction {
	c.t.Helper()
	rent, err := c.client.GetMinimumBalanceForRentExemption(c.t.Context(), size, rpc.CommitmentConfirmed)
	if err != nil {
		c.t.Fatal(err)
	}
	return []sol.Instruction{system.NewCreateAccountInstruction(rent, size, sol.TokenProgramID, c.payer.PublicKey(), addr).Build()}
}

// send submits ixs paid and signed by payer (plus extra signers) and waits
// until the transaction is confirmed. It returns the signature.
func (c *testChain) send(extra []sol.PrivateKey, ixs ...sol.Instruction) sol.Signature {
	c.t.Helper()
	bh, err := c.client.GetLatestBlockhash(c.t.Context(), rpc.CommitmentConfirmed)
	if err != nil {
		c.t.Fatal(err)
	}
	tx, err := sol.NewTransaction(ixs, bh.Value.Blockhash, sol.TransactionPayer(c.payer.PublicKey()))
	if err != nil {
		c.t.Fatal(err)
	}
	signers := append([]sol.PrivateKey{c.payer}, extra...)
	if _, err := tx.Sign(func(k sol.PublicKey) *sol.PrivateKey {
		for i := range signers {
			if signers[i].PublicKey() == k {
				return &signers[i]
			}
		}
		return nil
	}); err != nil {
		c.t.Fatal(err)
	}
	sig, err := c.client.SendTransactionWithOpts(c.t.Context(), tx, rpc.TransactionOpts{PreflightCommitment: rpc.CommitmentConfirmed})
	if err != nil {
		c.t.Fatal(err)
	}
	c.wait(sig, rpc.ConfirmationStatusConfirmed)
	return sig
}

// wait polls until sig reaches status (or a later one).
func (c *testChain) wait(sig sol.Signature, status rpc.ConfirmationStatusType) {
	c.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		res, err := c.client.GetSignatureStatuses(c.t.Context(), false, sig)
		if err == nil && len(res.Value) == 1 && res.Value[0] != nil {
			s := res.Value[0]
			if s.Err != nil {
				c.t.Fatalf("transaction %s failed: %v", sig, s.Err)
			}
			if s.ConfirmationStatus == status || s.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.t.Fatalf("transaction %s not %s in time", sig, status)
}

// transfer sends amount USDC from the payer's source account to dest.
func (c *testChain) transfer(dest sol.PublicKey, amount uint64) sol.Signature {
	c.t.Helper()
	return c.send(nil, token.NewTransferCheckedInstruction(amount, decimals, c.source, c.mint, dest, c.payer.PublicKey(), nil).Build())
}

func (c *testChain) round() chain.Result {
	c.t.Helper()
	res, err := c.watcher.RunOnce(c.t.Context())
	if err != nil {
		c.t.Fatal(err)
	}
	return res
}

// newCustomer creates a customer with an address on this chain.
func (c *testChain) newCustomer() (customer.Customer, chain.Address) {
	c.t.Helper()
	cust, _, err := customer.Create(c.t.Context(), pool, "Acme "+c.t.Name(), chain.NewAddresses(c.deriver).Assign)
	if err != nil {
		c.t.Fatal(err)
	}
	return cust, c.address(cust)
}

func (c *testChain) address(cust customer.Customer) chain.Address {
	c.t.Helper()
	addrs, err := chain.CustomerAddresses(c.t.Context(), pool, cust.ID)
	if err != nil || len(addrs) != 1 || addrs[0].Chain != c.name {
		c.t.Fatalf("addresses: %v %+v", err, addrs)
	}
	return addrs[0]
}

func (c *testChain) signatureCount(addr sol.PublicKey) int {
	c.t.Helper()
	sigs, err := c.client.GetSignaturesForAddressWithOpts(c.t.Context(), addr, &rpc.GetSignaturesForAddressOpts{Commitment: rpc.CommitmentConfirmed})
	if err != nil {
		c.t.Fatal(err)
	}
	return len(sigs)
}

// ---- assertions ----

func deposits(t *testing.T, cust customer.Customer) []chain.Deposit {
	t.Helper()
	ds, err := chain.CustomerDeposits(t.Context(), pool, cust.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func wantAvailable(t *testing.T, cust customer.Customer, want string) {
	t.Helper()
	got, err := ledger.Balance(t.Context(), pool, cust.Accounts.Available)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ledger.ParseUSDC(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != w {
		t.Fatalf("available = %s, want %s", got, w)
	}
}

func depositEntries(t *testing.T, d chain.Deposit) int {
	t.Helper()
	var n int
	key := ledger.DepositKey(d.Chain, d.TxHash, uint64(d.LogIndex))
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM journal_entries WHERE idempotency_key = $1`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func checkInvariants(t *testing.T) {
	t.Helper()
	vs, err := ledger.CheckInvariants(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Check != ledger.CheckHouseNegative {
			t.Errorf("ledger invariant: %+v", v)
		}
	}
}

// ---- tests ----

// A new customer's address starts as provisioning. The first round creates
// its token account and marks it active; later rounds, and calling
// Provision again directly, send nothing.
func TestProvisionOnce(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	if addr.Status != "provisioning" {
		t.Fatalf("new address status %q, want provisioning", addr.Status)
	}
	if res := c.round(); res.Provisioned != 1 {
		t.Fatalf("first round: %+v, want 1 provisioned", res)
	}
	if got := c.address(cust).Status; got != "active" {
		t.Fatalf("status after provisioning %q, want active", got)
	}
	pk := sol.MustPublicKeyFromBase58(addr.Address)
	before := c.signatureCount(pk)
	if before != 1 {
		t.Fatalf("%d transactions touch the new account, want 1 (its creation)", before)
	}

	if res := c.round(); res.Provisioned != 0 {
		t.Fatalf("second round provisioned %d, want 0", res.Provisioned)
	}
	var idx int64
	if err := pool.QueryRow(t.Context(), `SELECT derivation_index FROM deposit_addresses WHERE address = $1`, addr.Address).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if err := c.prov.Provision(t.Context(), uint64(idx), addr.Address); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if after := c.signatureCount(pk); after != before {
		t.Fatalf("second Provision sent a transaction (%d -> %d)", before, after)
	}
}

// Anyone can send SOL to an address before Ferry creates its account,
// which makes createAccountWithSeed fail ("already in use"). The cheapest
// such grief is the rent minimum of an empty account, less than a token
// account needs. Provisioning must still succeed (topping up the rent),
// and the SOL transfer must not be mistaken for a deposit.
func TestProvisionPrefundedAddress(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	pk := sol.MustPublicKeyFromBase58(addr.Address)
	dust, err := c.client.GetMinimumBalanceForRentExemption(t.Context(), 0, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	c.send(nil, system.NewTransferInstruction(dust, c.payer.PublicKey(), pk).Build())

	if res := c.round(); res.Provisioned != 1 {
		t.Fatalf("round: %+v, want the pre-funded address provisioned", res)
	}
	if got := c.address(cust).Status; got != "active" {
		t.Fatalf("status %q, want active", got)
	}
	if ds := deposits(t, cust); len(ds) != 0 {
		t.Fatalf("SOL transfer recorded as deposits: %+v", ds)
	}
}

// A USDC transfer to the customer's account is credited once it is
// finalized, at once (no extra confirmations). USDC sent elsewhere is
// ignored. Scanning the same slots again (the cursor moved back, as after
// a crash) finds the transfer again but credits nothing new.
func TestDepositCreditedOnce(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	c.round() // provisions
	pk := sol.MustPublicKeyFromBase58(addr.Address)

	elsewhere := newKey(t)
	c.send([]sol.PrivateKey{elsewhere}, append(c.createAccount(elsewhere.PublicKey(), solana.TokenAccountSize),
		token.NewInitializeAccount3Instruction(c.payer.PublicKey(), elsewhere.PublicKey(), c.mint).Build())...)
	c.transfer(elsewhere.PublicKey(), usdc(5))
	sig := c.transfer(pk, usdc(200))
	c.wait(sig, rpc.ConfirmationStatusFinalized)

	var found, confirmed int
	for range 5 { // the range may take more than one batch
		res := c.round()
		found += res.Found
		confirmed += res.Confirmed
	}
	if found != 1 || confirmed != 1 {
		t.Fatalf("found %d, confirmed %d; want 1 and 1", found, confirmed)
	}
	ds := deposits(t, cust)
	if len(ds) != 1 || ds[0].Status != "confirmed" || ds[0].TxHash != sig.String() || ds[0].Amount != int64(usdc(200)) {
		t.Fatalf("deposits = %+v, want one confirmed 200 USDC from %s", ds, sig)
	}
	wantAvailable(t, cust, "200")

	// Rescan from the first slot.
	if _, err := pool.Exec(t.Context(),
		`UPDATE chain_cursors SET last_block = $2, last_hash = (SELECT hash FROM chain_blocks WHERE chain = $1 AND number = $2) WHERE chain = $1`,
		c.name, c.startPos); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if res := c.round(); res.Found+res.Confirmed != 0 {
			t.Fatalf("rescan round: %+v, want nothing new", res)
		}
	}
	if n := depositEntries(t, ds[0]); n != 1 {
		t.Fatalf("%d deposit entries, want 1", n)
	}
	wantAvailable(t, cust, "200")
}

package evm_test

// Watcher tests on go-ethereum's in-process simulated chain and a
// throwaway Postgres. Each test gets its own chain (and chain name), so
// cursors and deposits do not mix. Skipped with -short.

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/contracts"
	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

var (
	pool  *pgxpool.Pool
	house ledger.HouseAccounts
	token abi.ABI
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
	var err error
	if token, err = abi.JSON(strings.NewReader(contracts.TestUSDCABI)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
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

const confirmations = 3

type testChain struct {
	t       *testing.T
	backend *simulated.Backend
	client  simulated.Client
	key     *ecdsa.PrivateKey
	from    common.Address
	chainID *big.Int
	usdc    common.Address
	name    string
	watcher *chain.Watcher
	deriver *evm.Deriver
}

// newChain starts a simulated chain with a funded sender, deploys TestUSDC
// and mints 1,000 USDC to the sender, and sets up a watcher that starts
// scanning at the next block.
func newChain(t *testing.T) *testChain {
	t.Helper()
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	c := &testChain{t: t, key: key, from: crypto.PubkeyToAddress(key.PublicKey)}
	c.backend = simulated.NewBackend(types.GenesisAlloc{
		c.from: {Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
	})
	t.Cleanup(func() { _ = c.backend.Close() })
	c.client = c.backend.Client()
	if c.chainID, err = c.client.ChainID(t.Context()); err != nil {
		t.Fatal(err)
	}

	c.usdc = c.deploy()
	c.call(c.usdc, "mint", c.from, usdc(1000))
	c.backend.Commit()

	// The name must fit the deposit_addresses chain check: [a-z0-9-].
	c.name = "evm-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	if len(c.name) > 32 {
		c.name = c.name[:32]
	}
	if c.deriver, err = evm.NewDeriver(c.name, devXpub); err != nil {
		t.Fatal(err)
	}
	c.watcher = chain.NewWatcher(pool, evm.New(c.name, c.client, c.usdc), house, slog.New(slog.DiscardHandler))
	c.watcher.Confirmations = confirmations
	c.watcher.StartBlock = c.head() + 1
	t.Cleanup(func() { checkInvariants(t) })
	return c
}

// devXpub is account m/44'/60'/1' of the public test mnemonic (the one in
// .env.example), so deposit addresses never collide with funded accounts.
const devXpub = "xpub6Ce9NcJvTk372KjsGfWqbcex5DumjpNquQLApoeQUavSCjEc823BV1tb4rXUuPuht8h2hSxkg2EXUaKUJmniJvRZAELxypsCzBFdtosmV76"

func usdc(units int64) *big.Int { return big.NewInt(units * 1_000_000) }

func (c *testChain) head() uint64 {
	n, err := c.client.BlockNumber(c.t.Context())
	if err != nil {
		c.t.Fatal(err)
	}
	return n
}

// signed builds and signs a transaction from the sender; to == nil deploys.
func (c *testChain) signed(nonce uint64, to *common.Address, data []byte, tip int64) *types.Transaction {
	c.t.Helper()
	tx, err := types.SignNewTx(c.key, types.LatestSignerForChainID(c.chainID), &types.DynamicFeeTx{
		ChainID: c.chainID, Nonce: nonce, To: to, Data: data, Gas: 2_000_000,
		GasTipCap: big.NewInt(tip), GasFeeCap: big.NewInt(50 * tip), // a replacement must raise both by 10%
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return tx
}

func (c *testChain) nonce() uint64 {
	n, err := c.client.PendingNonceAt(c.t.Context(), c.from)
	if err != nil {
		c.t.Fatal(err)
	}
	return n
}

func (c *testChain) send(tx *types.Transaction) {
	c.t.Helper()
	if err := c.client.SendTransaction(c.t.Context(), tx); err != nil && !strings.Contains(err.Error(), "already known") {
		c.t.Fatal(err)
	}
}

func (c *testChain) deploy() common.Address {
	c.t.Helper()
	tx := c.signed(c.nonce(), nil, common.FromHex(contracts.TestUSDCBin), 1e9)
	c.send(tx)
	c.backend.Commit()
	r, err := c.client.TransactionReceipt(c.t.Context(), tx.Hash())
	if err != nil || r.Status != types.ReceiptStatusSuccessful {
		c.t.Fatalf("deploy: %v %+v", err, r)
	}
	return r.ContractAddress
}

// call sends a token call; it is mined by the next Commit.
func (c *testChain) call(contract common.Address, method string, args ...any) {
	c.t.Helper()
	data, err := token.Pack(method, args...)
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(c.signed(c.nonce(), &contract, data, 1e9))
}

// mine commits n blocks (the first one includes pending transactions).
func (c *testChain) mine(n int) {
	for range n {
		c.backend.Commit()
	}
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
func (c *testChain) newCustomer() (customer.Customer, common.Address) {
	c.t.Helper()
	cust, _, err := customer.Create(c.t.Context(), pool, "Acme "+c.t.Name(), chain.NewAddresses(c.deriver).Assign)
	if err != nil {
		c.t.Fatal(err)
	}
	addrs, err := chain.CustomerAddresses(c.t.Context(), pool, cust.ID)
	if err != nil || len(addrs) != 1 || addrs[0].Chain != c.name {
		c.t.Fatalf("addresses: %v %+v", err, addrs)
	}
	return cust, common.HexToAddress(addrs[0].Address)
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

func wantDeposit(t *testing.T, cust customer.Customer, status string) chain.Deposit {
	t.Helper()
	ds := deposits(t, cust)
	if len(ds) != 1 || ds[0].Status != status {
		t.Fatalf("deposits = %+v, want one %s", ds, status)
	}
	return ds[0]
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

func blockHashOf(t *testing.T, d chain.Deposit) string {
	t.Helper()
	var h string
	if err := pool.QueryRow(t.Context(), `SELECT block_hash FROM deposits WHERE id = $1`, d.ID).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
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

// A deposit is visible as pending at once and credited only once it is
// `confirmations` blocks deep; later rounds change nothing.
func TestDepositCreditedAfterConfirmations(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	c.call(c.usdc, "transfer", addr, usdc(200))
	c.mine(1)

	if res := c.round(); res.Found != 1 || res.Confirmed != 0 {
		t.Fatalf("first round: %+v, want 1 found, 0 confirmed", res)
	}
	wantDeposit(t, cust, "pending")
	wantAvailable(t, cust, "0")

	c.mine(confirmations)
	if res := c.round(); res.Confirmed != 1 {
		t.Fatalf("after %d blocks: %+v, want 1 confirmed", confirmations, res)
	}
	d := wantDeposit(t, cust, "confirmed")
	wantAvailable(t, cust, "200")

	c.mine(2)
	c.round()
	if n := depositEntries(t, d); n != 1 {
		t.Fatalf("%d deposit entries, want 1", n)
	}
	wantAvailable(t, cust, "200")
}

// Failure table: "Chain reorg". The deposit's block is replaced by one in
// which the sender spent the same nonce elsewhere: the deposit never
// happened on the winning chain. It is orphaned and never credited.
func TestReorgOrphansDeposit(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	parent := c.backend.Commit() // an empty block to fork from
	nonce := c.nonce()
	c.call(c.usdc, "transfer", addr, usdc(200))
	c.mine(1)
	c.round()
	wantDeposit(t, cust, "pending")

	// Fork at the parent; the same nonce now pays someone else.
	if err := c.backend.Fork(parent); err != nil {
		t.Fatal(err)
	}
	data, err := token.Pack("transfer", common.HexToAddress("0x000000000000000000000000000000000000dEaD"), usdc(200))
	if err != nil {
		t.Fatal(err)
	}
	c.send(c.signed(nonce, &c.usdc, data, 2e9))
	c.mine(2 + confirmations) // longer than the old branch: canonical

	res := c.round()
	if !res.Rewound {
		t.Fatalf("round after reorg: %+v, want a rewind", res)
	}
	c.mine(confirmations)
	c.round()
	wantDeposit(t, cust, "orphaned")
	wantAvailable(t, cust, "0")
}

// After a reorg the same transfer is usually included again: nodes put
// the transactions of dropped blocks back into their mempool (the
// simulated chain does too). The deposit is orphaned with its old block,
// comes back as pending at its new block, and is credited once.
func TestReorgReincludedDepositCreditedOnce(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	parent := c.backend.Commit()
	c.call(c.usdc, "transfer", addr, usdc(200))
	c.mine(1)
	c.round()
	before := wantDeposit(t, cust, "pending")
	oldHash := blockHashOf(t, before)

	if err := c.backend.Fork(parent); err != nil {
		t.Fatal(err)
	}
	c.mine(2 + confirmations) // the transfer is re-mined on the new branch

	if res := c.round(); !res.Rewound {
		t.Fatalf("round after reorg: %+v, want a rewind", res)
	}
	c.mine(confirmations)
	c.round()
	after := wantDeposit(t, cust, "confirmed")
	if after.ID != before.ID || blockHashOf(t, after) == oldHash {
		t.Fatalf("deposit before %+v, after %+v: want the same deposit in a new block", before, after)
	}
	wantAvailable(t, cust, "200")
	if n := depositEntries(t, after); n != 1 {
		t.Fatalf("%d deposit entries, want 1", n)
	}
}

// Failure table: crash after storing deposits but before work is
// acknowledged. Scanning the same range again (cursor moved back) finds
// the same transfer and stores nothing new. Transfers of another token and
// to unknown addresses are ignored.
func TestRescanAndNoiseIgnored(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	fake := c.deploy() // same Transfer event, different contract
	c.call(fake, "mint", addr, usdc(500))
	c.call(c.usdc, "transfer", common.HexToAddress("0x000000000000000000000000000000000000bEEF"), usdc(5))
	c.call(c.usdc, "transfer", addr, usdc(50))
	c.mine(1)

	if res := c.round(); res.Found != 1 {
		t.Fatalf("found %d deposits, want 1 (only the real USDC to the customer)", res.Found)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE chain_cursors SET last_block = last_block - 2, last_hash = (SELECT hash FROM chain_blocks b WHERE b.chain = $1 ORDER BY number LIMIT 1) WHERE chain = $1`,
		c.name); err != nil {
		t.Fatal(err)
	}
	c.mine(confirmations)
	c.round()
	c.round()
	d := wantDeposit(t, cust, "confirmed")
	wantAvailable(t, cust, "50")
	if n := depositEntries(t, d); n != 1 {
		t.Fatalf("%d deposit entries, want 1", n)
	}
}

// A customer created before the chain was configured gets an address from
// Backfill and its deposits are watched from then on.
func TestBackfillAddresses(t *testing.T) {
	c := newChain(t)
	cust, _, err := customer.Create(t.Context(), pool, "Early "+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chain.NewAddresses(c.deriver).Backfill(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	addrs, err := chain.CustomerAddresses(t.Context(), pool, cust.ID)
	if err != nil || len(addrs) != 1 {
		t.Fatalf("addresses after backfill: %v %+v", err, addrs)
	}
	c.call(c.usdc, "transfer", common.HexToAddress(addrs[0].Address), usdc(10))
	c.mine(1 + confirmations)
	c.round()
	wantDeposit(t, cust, "confirmed")
}

// The last check before money moves: a deposit recorded with a block hash
// the chain no longer has (as when a lagging RPC node served logs from a
// dropped fork) is not credited. It is orphaned, the range is scanned
// again, and the transfer as it really is on the chain is credited once.
func TestCreditVerifiesBlockHash(t *testing.T) {
	c := newChain(t)
	cust, addr := c.newCustomer()
	c.call(c.usdc, "transfer", addr, usdc(75))
	c.mine(1)
	c.round()
	d := wantDeposit(t, cust, "pending")
	if _, err := pool.Exec(t.Context(),
		`UPDATE deposits SET block_hash = '0x'||repeat('ab', 32) WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}

	c.mine(confirmations)
	if res := c.round(); res.Orphaned != 1 || res.Confirmed != 0 {
		t.Fatalf("round with a stale hash: %+v, want 1 orphaned, 0 confirmed", res)
	}
	wantAvailable(t, cust, "0")

	c.round() // re-scan finds the transfer at its real block and credits it
	c.round()
	d = wantDeposit(t, cust, "confirmed")
	wantAvailable(t, cust, "75")
	if n := depositEntries(t, d); n != 1 {
		t.Fatalf("%d deposit entries, want 1", n)
	}
}

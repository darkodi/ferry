# Ferry

A security-first stablecoin payout engine in Go. A business deposits USDC on-chain, then requests a fiat payout to a bank account. Ferry quotes the payout through a fiat rail, holds the funds, submits the payout and settles a double-entry ledger.

The design, its invariants and its failure modes are in [`docs/design.md`](docs/design.md).

## Status

| Milestone | State |
| --- | --- |
| Repo scaffold, CI | done |
| M1 — Ledger core | done |
| M2 — Payout API + state machine | done |
| M3 — Events (outbox relay, workers, resolver) | done |
| M4 — Chains (EVM done, Solana next) | in progress |
| M5–M7 | planned |

What works today: the ledger core. `internal/ledger` posts balanced entries for every event in the design (deposit, top-up, hold, settle, release, bank return, correction), refuses overdrafts even under 100 concurrent requests, deduplicates replayed events, and checks its own invariants. The database rejects unbalanced entries, negative customer balances and any edit of posted history even if the Go code has a bug. On top of it, `internal/payout` runs a payout through quote, hold, submit, settle, release, expiry and bank return against a mock rail, with idempotent creates and safe retries after a rail timeout. `cmd/api` serves it over HTTP with per-customer API keys and `Idempotency-Key` handling. Confirming a payout returns at once with the money held; in the background the outbox relay publishes the event to an SQS queue (ElasticMQ locally), a worker submits the payout to the rail, and a resolver re-drives payouts whose submit timed out or whose rail update never arrived. Customers get their own EVM deposit address, derived from an xpub so Ferry never holds the key. `cmd/watcher` reads USDC transfers to those addresses, shows them as pending, and credits them after N confirmations. It handles reorgs by rewinding and orphaning, and re-checks every block hash before crediting. Solana deposits come next in M4; the Noah sandbox (M5) is not wired in yet, so the rail is a mock that settles at once.

## Run locally

Requires Go (version in `go.mod`), Docker and make.

```bash
cp .env.example .env   # adjust if needed
make up                # Postgres 16 on 127.0.0.1:5432, ElasticMQ (SQS) on 127.0.0.1:9324
make migrate           # apply migrations
make test              # go test -race ./... (integration tests need Docker; -short skips them)
make lint              # go vet, golangci-lint, gosec, govulncheck
make help              # all targets
```

The pinned tool versions are installed into `./bin` on first use.

### Try a payout

Set `FERRY_ADMIN_KEY` in `.env` (for example `openssl rand -hex 32`) and keep `FERRY_ENV=dev`. Then:

```bash
make up && make migrate   # Postgres, ElasticMQ and a local Anvil chain
make chain-setup          # deploy TestUSDC to Anvil (prints the address in .env.example)
make run                  # API; in dev also the relay, worker and resolver
make watcher              # in a second terminal: the EVM deposit watcher
```

In a third terminal:

```bash
source .env
B=http://127.0.0.1:8080/v1

# 1. Create a customer (operator). Note the id and the api_key; the key is shown once.
curl -s -X POST $B/customers -H "Authorization: Bearer $FERRY_ADMIN_KEY" -d '{"name":"Acme"}'
CID=...; KEY=ferry_sk_...

# 2. Fund it on-chain: send 200 TestUSDC from Anvil's first account to the
#    customer's deposit address. It shows as "pending", then "confirmed" two blocks
#    (about 4 s) later, when the balance becomes 200.
curl -s $B/customers/$CID/deposit-addresses -H "Authorization: Bearer $KEY"
TO=0x...   # the address above
docker compose exec -T anvil cast send --rpc-url http://localhost:8545 --unlocked \
  --from 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266 $FERRY_EVM_USDC_ADDRESS \
  "transfer(address,uint256)" $TO 200000000
curl -s $B/customers/$CID/deposits -H "Authorization: Bearer $KEY"
#    (Without a chain, POST $B/dev/deposits with the operator key still credits directly.)

# 3. Ask for a 100.00 USD payout. The quote authorizes up to 101.50 USDC.
curl -s -X POST $B/payouts -H "Authorization: Bearer $KEY" -H "Idempotency-Key: order-1" \
  -d '{"fiat_amount":10000,"fiat_currency":"USD","destination":"bank_acct_1"}'
PID=...

# 4. Confirm: holds 101.50 and returns "held". The worker then submits it to the mock rail,
#    which settles at once; a moment later the payout shows "settled".
curl -s -X POST $B/payouts/$PID/confirm -H "Authorization: Bearer $KEY" -H "Idempotency-Key: confirm-1"
curl -s $B/payouts/$PID -H "Authorization: Bearer $KEY"

# 5. Balances: 99.50 available (100 paid + 0.50 rail fee; the 1.00 buffer came back).
curl -s $B/customers/$CID/balances -H "Authorization: Bearer $KEY"
```

Sending step 3 again with the same `Idempotency-Key` returns the same payout with `Idempotent-Replayed: true`.

If port 5432 is taken on your machine, set `POSTGRES_PORT` and the port in `DATABASE_URL` in `.env` to a free one.

## License

See [LICENSE](LICENSE).

# Ferry

A security-first stablecoin payout engine in Go. A business deposits USDC on-chain, then requests a fiat payout to a bank account. Ferry quotes the payout through a fiat rail, holds the funds, submits the payout and settles a double-entry ledger.

The design, its invariants and its failure modes are in [`docs/design.md`](docs/design.md).

## Status

| Milestone | State |
| --- | --- |
| Repo scaffold, CI | done |
| M1 — Ledger core | done |
| M2 — Payout API + state machine | done |
| M3 — Events (outbox relay, workers, resolver) | in progress |
| M4–M7 | planned |

What works today: the ledger core. `internal/ledger` posts balanced entries for every event in the design (deposit, top-up, hold, settle, release, bank return, correction), refuses overdrafts even under 100 concurrent requests, deduplicates replayed events, and checks its own invariants. The database rejects unbalanced entries, negative customer balances and any edit of posted history even if the Go code has a bug. On top of it, `internal/payout` runs a payout through quote, hold, submit, settle, release, expiry and bank return against a mock rail, with idempotent creates and safe retries after a rail timeout. `cmd/api` serves it over HTTP with per-customer API keys and `Idempotency-Key` handling. `cmd/relay` publishes the outbox to an SQS queue (ElasticMQ locally); the worker that consumes it lands next in M3. Real chain deposits (M4) and the Noah sandbox (M5) are not wired in yet: deposits come from a dev-only endpoint and the rail is a mock that settles at once.

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

Set `FERRY_ADMIN_KEY` in `.env` (for example `openssl rand -hex 32`) and keep `FERRY_ENV=dev`, then start the server with `make run` and, in another terminal:

```bash
source .env
B=http://127.0.0.1:8080/v1

# 1. Create a customer (operator). Note the id and the api_key; the key is shown once.
curl -s -X POST $B/customers -H "Authorization: Bearer $FERRY_ADMIN_KEY" -d '{"name":"Acme"}'
CID=...; KEY=ferry_sk_...

# 2. Fund it (dev only; stands in for an on-chain deposit).
curl -s -X POST $B/dev/deposits -H "Authorization: Bearer $FERRY_ADMIN_KEY" \
  -d "{\"customer_id\":\"$CID\",\"amount\":\"200\"}"

# 3. Ask for a 100.00 USD payout. The quote authorizes up to 101.50 USDC.
curl -s -X POST $B/payouts -H "Authorization: Bearer $KEY" -H "Idempotency-Key: order-1" \
  -d '{"fiat_amount":10000,"fiat_currency":"USD","destination":"bank_acct_1"}'
PID=...

# 4. Confirm: holds 101.50, submits to the mock rail, which settles at once.
curl -s -X POST $B/payouts/$PID/confirm -H "Authorization: Bearer $KEY" -H "Idempotency-Key: confirm-1"

# 5. Balances: 99.50 available (100 paid + 0.50 rail fee; the 1.00 buffer came back).
curl -s $B/customers/$CID/balances -H "Authorization: Bearer $KEY"
```

Sending step 3 again with the same `Idempotency-Key` returns the same payout with `Idempotent-Replayed: true`.

If port 5432 is taken on your machine, set `POSTGRES_PORT` and the port in `DATABASE_URL` in `.env` to a free one.

## License

See [LICENSE](LICENSE).

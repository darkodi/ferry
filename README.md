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
| M4 — Chains (EVM and Solana deposits) | done |
| M5 — Noah rail (destinations, signed adapter, webhooks, returns; sandbox run pending a key) | done |
| M6 — Recon + ops (reconciliation, top-ups, metrics, traces, alerts, dashboard) | done |
| M7 | planned |

What works today: the ledger core. `internal/ledger` posts balanced entries for every event in the design (deposit, top-up, hold, settle, release, bank return, correction), refuses overdrafts even under 100 concurrent requests, deduplicates replayed events, and checks its own invariants. The database rejects unbalanced entries, negative customer balances and any edit of posted history even if the Go code has a bug. On top of it, `internal/payout` runs a payout through quote, hold, submit, settle, release, expiry and bank return against a mock rail, with idempotent creates and safe retries after a rail timeout. `cmd/api` serves it over HTTP with per-customer API keys and `Idempotency-Key` handling. Confirming a payout returns at once with the money held; in the background the outbox relay publishes the event to an SQS queue (ElasticMQ locally), a worker submits the payout to the rail, and a resolver re-drives payouts whose submit timed out or whose rail update never arrived. Customers get their own EVM deposit address, derived from an xpub so Ferry never holds the key. `cmd/watcher` reads USDC transfers to those addresses, shows them as pending, and credits them after N confirmations. It handles reorgs by rewinding and orphaning, and re-checks every block hash before crediting. On Solana, each customer gets a token account at a seed-derived address. The watcher creates it on-chain (signing with a key that can create accounts but cannot move funds), reads only finalized slots, and credits a deposit as soon as it sees it. The treasury sweep out of deposit addresses comes later.

Payouts go to saved bank accounts (`POST /v1/destinations`), which Ferry stores encrypted and shows only masked. With `FERRY_RAIL=noah`, Ferry is a client of the Noah Business API:
- It picks the payout channel, prices the payout, and sells against its pre-funded USDC balance, signing every request (ES384).
- `cmd/worker` runs as its own process.
- A refusal releases the hold only when Noah definitely created nothing.

There's no Noah sandbox key yet, so this runs against a fake Noah server built from the documented API (`make fake-noah`). `internal/rail/noah/sandbox_test.go` is ready for the real sandbox. The default rail is still the in-memory mock that settles at once.

## Run locally

Requires Go (version in `go.mod`), Docker and make.

```bash
cp .env.example .env   # adjust if needed
make up                # Postgres 16 on 127.0.0.1:5432, ElasticMQ (SQS) on :9324, Anvil on :8545, Solana validator on :8899
make migrate           # apply migrations
make test              # go test -race ./... (integration tests need Docker; -short skips them)
make lint              # go vet, golangci-lint, gosec, govulncheck
make help              # all targets
```

The pinned tool versions are installed into `./bin` on first use.

### Try a payout

Set `FERRY_ADMIN_KEY` (for example `openssl rand -hex 32`) and `FERRY_DATA_KEY` (`openssl rand -base64 32`) in `.env`, and keep `FERRY_ENV=dev`. Then:

```bash
make up && make migrate   # Postgres, ElasticMQ, a local Anvil chain and a Solana test validator
make chain-setup          # test USDC on both chains; prints the values to put in .env
make run                  # API; in dev also the relay, worker and resolver
make watcher              # in a second terminal: the EVM deposit watcher
make watcher-solana       # optional, in another terminal: the Solana deposit watcher
```

`make chain-setup` prints `FERRY_EVM_USDC_ADDRESS` (already in `.env.example`) and, for Solana, `FERRY_SOLANA_USDC_MINT`, `FERRY_SOLANA_AUTHORITY` and `FERRY_SOLANA_DEPOSIT_OWNER`. Copy the Solana values into `.env` and restart `make run`, so new customers also get a Solana address. The Solana keypairs are kept in `.dev/solana` (git-ignored), so the values stay the same after the chain restarts.

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
#    Or on Solana: the customer's solana-local address shows "provisioning" until the
#    watcher has created its token account (one round), then "active". Send to it from the
#    dev wallet; it is credited once its slot is finalized (about 15 s).
SOL_TO=...   # the solana-local address above
docker compose exec -T solana spl-token -u http://localhost:8899 transfer $FERRY_SOLANA_USDC_MINT 200 $SOL_TO
#    (Without a chain, POST $B/dev/deposits with the operator key still credits directly.)

# 3. Save the bank account to pay (a BankAch form, as Noah's channel asks for it). The
#    response shows only "BankAch ****6789"; the details are stored encrypted.
curl -s -X POST $B/destinations -H "Authorization: Bearer $KEY" -H "Idempotency-Key: dest-1" -d '{
  "country":"US","fiat_currency":"USD","payment_method_type":"BankAch",
  "form":{"AccountHolderAddress":{"Address":"1 Main St","City":"New York","State":"NY","PostalCode":"10001"},
          "BankDetails":{"AccountNumber":"000123456789","BankCode":"021000021","AccountType":"Checking"},
          "PaymentPurpose":"Invoice"}}'
DID=...

#    Ask for a 100.00 USD payout to it. The quote authorizes up to 101.50 USDC.
curl -s -X POST $B/payouts -H "Authorization: Bearer $KEY" -H "Idempotency-Key: order-1" \
  -d "{\"fiat_amount\":10000,\"fiat_currency\":\"USD\",\"destination_id\":\"$DID\"}"
PID=...

# 4. Confirm: holds 101.50 and returns "held". The worker then submits it to the mock rail,
#    which settles at once; a moment later the payout shows "settled".
curl -s -X POST $B/payouts/$PID/confirm -H "Authorization: Bearer $KEY" -H "Idempotency-Key: confirm-1"
curl -s $B/payouts/$PID -H "Authorization: Bearer $KEY"

# 5. Balances: 99.50 available (100 paid + 0.50 rail fee; the 1.00 buffer came back).
curl -s $B/customers/$CID/balances -H "Authorization: Bearer $KEY"
```

Sending step 3 again with the same `Idempotency-Key` returns the same payout with `Idempotent-Replayed: true`.

**Reconciliation.** `make recon` runs `cmd/recon` once. It checks the ledger's invariants, every configured chain's deposit addresses, and (with `FERRY_RAIL=noah`) Noah's transactions and balance, and records anything that disagrees as a break. List breaks with `GET /v1/admin/recon-breaks` (operator key); explain one with `POST /v1/admin/recon-breaks/{id}/resolve`. With Noah, book money that arrives in Ferry's Noah balance with `POST /v1/admin/treasury-topups {"rail_tx_id": "..."}`: Ferry reads the amount from Noah. With the fake, `curl -X POST localhost:18090/_control/credit/1000` simulates a top-up arriving.

**Observability.** `make obs-up` starts Prometheus (http://127.0.0.1:9090, with Ferry's alert rules), Grafana (http://127.0.0.1:3000, with the provisioned *Ferry* dashboard) and Jaeger (http://127.0.0.1:16686). Each binary serves `/metrics` on its own loopback port (api 9101, relay 9102, worker 9103, watchers 9104/9105). Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318` to send traces: one payout's trace runs from the API's confirm request through the outbox, queue and worker to the Noah HTTP call. Log lines carry `trace_id` to find it.

If port 5432 is taken on your machine, set `POSTGRES_PORT` and the port in `DATABASE_URL` in `.env` to a free one.

### Pay out through Noah (fake or sandbox)

The same flow with the Noah adapter and separate processes. Without a sandbox key, use the fake:

```bash
make fake-noah            # fake Noah on 127.0.0.1:18090
# in .env: FERRY_RAIL=noah, NOAH_BASE_URL=http://127.0.0.1:18090/v1, NOAH_API_KEY=fake-noah-key,
#          FERRY_RESOLVER_STALE=5s; leave FERRY_ENV empty so the API runs no in-process worker
make run                  # API: quotes through Noah
make relay                # outbox -> queue
make worker               # submits to Noah; the resolver polls Noah for status
```

Fund the customer as above (the dev deposit endpoint needs `FERRY_ENV=dev`; run the API once in dev mode for it), register a destination, create and confirm the payout. It reaches `submitted`. Then settle it at the fake:

```bash
curl -s localhost:18090/_control/transactions                # find the transaction ID
curl -s -X POST localhost:18090/_control/transactions/<ID>/settle  # or fail, review
```

Within `FERRY_RESOLVER_STALE` the worker re-fetches it and the payout shows `settled`, with 199.50 available from a 300 deposit.

With a sandbox key, point `NOAH_BASE_URL` at `https://api.sandbox.noah.com/v1` and set `NOAH_API_KEY` (and `NOAH_SIGNING_KEY_PATH` if your key requires signing). Then run the contract test first:

```bash
NOAH_API_KEY=... go test ./internal/rail/noah -run Sandbox -v          # channels + prepare
NOAH_API_KEY=... NOAH_SANDBOX_SELL=1 go test ./internal/rail/noah -run Sandbox -v  # also a 1 USD sale
```

**Webhooks.** Noah's webhooks settle payouts without waiting for the resolver. To see it with the fake, give it a webhook signing key and Ferry the public half:

```bash
openssl ecparam -name secp384r1 -genkey -noout -out /tmp/noah-webhook.pem
openssl ec -in /tmp/noah-webhook.pem -pubout -out /tmp/noah-webhook.pub.pem
FAKE_NOAH_WEBHOOK_URL=http://127.0.0.1:8080/v1/webhooks/noah FAKE_NOAH_WEBHOOK_KEY=/tmp/noah-webhook.pem make fake-noah
# in .env: NOAH_WEBHOOK_PUBLIC_KEY=/tmp/noah-webhook.pub.pem
```

Settling at the fake now moves the payout to `settled` within a second. `POST localhost:18090/_control/transactions/<ID>/reverse` sends a bank return, and the payout becomes `reversed`. Against the real sandbox, leave `NOAH_WEBHOOK_PUBLIC_KEY` empty (Noah's published key is built in) and expose `/v1/webhooks/noah` through a tunnel such as cloudflared.

## License

See [LICENSE](LICENSE).

# Ferry demo script

A 10-minute walkthrough for the video, run entirely on one machine. It shows:

- a deposit arriving on-chain;
- a payout through the Noah adapter with signed webhooks;
- a crash in the middle that loses nothing;
- a bank return;
- reconciliation;
- the dashboard and a trace.

Everything runs locally: Anvil stands in for the EVM testnet and the fake Noah server (built from Noah's documented API) for the sandbox.

## Before recording

```bash
cp .env.example .env
make up && make migrate     # Postgres, ElasticMQ, Anvil, Solana validator
make chain-setup            # test USDC; copy the printed Solana values into .env
make obs-up                 # Prometheus :9090, Grafana :3000, Jaeger :16686

# A webhook signing key for the fake, and its public half for Ferry.
openssl ecparam -name secp384r1 -genkey -noout -out /tmp/noah-webhook.pem
openssl ec -in /tmp/noah-webhook.pem -pubout -out /tmp/noah-webhook.pub.pem
```

In `.env`:

```bash
FERRY_ENV=                         # empty: the worker runs as its own process
FERRY_ADMIN_KEY=<openssl rand -hex 32>
FERRY_DATA_KEY=<openssl rand -base64 32>
FERRY_RAIL=noah
NOAH_BASE_URL=http://127.0.0.1:18090/v1
NOAH_API_KEY=fake-noah-key
NOAH_WEBHOOK_PUBLIC_KEY=/tmp/noah-webhook.pub.pem
FERRY_RESOLVER_STALE=30s
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
```

Open six terminals, one per process. Arrange them so the logs are visible:

```bash
FAKE_NOAH_WEBHOOK_URL=http://127.0.0.1:8080/v1/webhooks/noah FAKE_NOAH_WEBHOOK_KEY=/tmp/noah-webhook.pem make fake-noah
make run        # API
make relay      # outbox -> queue
make worker     # queue -> Noah; resolver
make watcher    # EVM deposits
```

The sixth terminal is for the commands below. Start it with:

```bash
source .env; B=http://127.0.0.1:8080/v1; ADMIN="Authorization: Bearer $FERRY_ADMIN_KEY"
```

## 1. The idea (30 s)

Show the README's architecture diagram. *"A business deposits USDC, asks for a bank payout, and Ferry makes sure money is never created, lost or paid twice, even when processes crash and messages repeat."*

## 2. A customer and a deposit (1.5 min)

```bash
curl -s -X POST $B/customers -H "$ADMIN" -d '{"name":"Acme"}'          # note id and api_key (shown once)
CID=...; KEY=...; AUTH="Authorization: Bearer $KEY"
curl -s $B/customers/$CID/deposit-addresses -H "$AUTH"                  # an EVM and a Solana address
TO=0x...
docker compose exec -T anvil cast send --rpc-url http://localhost:8545 --unlocked \
  --from 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266 $FERRY_EVM_USDC_ADDRESS \
  "transfer(address,uint256)" $TO 300000000
curl -s $B/customers/$CID/deposits -H "$AUTH"                           # pending, then confirmed
curl -s $B/customers/$CID/balances -H "$AUTH"                           # 300 available
```

> "The address is derived from a public key: the API can't move these funds. The deposit is credited only after enough confirmations, and a reorg would roll it back."

## 3. A payout (2 min)

Ferry pays out of its pre-funded balance at Noah, so the operator first moves part of the deposited USDC there. At the fake, a credit stands in for that transfer. Ferry books it by its Noah transaction id and reads the amount from Noah.

```bash
FUND=$(curl -s -X POST localhost:18090/_control/credit/200 | jq -r .ID)
curl -s -X POST $B/admin/treasury-topups -H "$ADMIN" -d "{\"rail_tx_id\":\"$FUND\"}"
```

```bash
curl -s -X POST $B/destinations -H "$AUTH" -H "Idempotency-Key: dest-1" -d '{
  "country":"US","fiat_currency":"USD","payment_method_type":"BankAch",
  "form":{"AccountHolderAddress":{"Address":"1 Main St","City":"New York","State":"NY","PostalCode":"10001"},
          "BankDetails":{"AccountNumber":"000123456789","BankCode":"021000021","AccountType":"Checking"},
          "PaymentPurpose":"Invoice"}}'                                   # shows only "BankAch ****6789"
DID=...
curl -s -X POST $B/payouts -H "$AUTH" -H "Idempotency-Key: order-1" \
  -d "{\"fiat_amount\":10000,\"fiat_currency\":\"USD\",\"destination_id\":\"$DID\"}"   # quoted
PID=...
curl -s -X POST $B/payouts/$PID/confirm -H "$AUTH" -H "Idempotency-Key: confirm-1"   # held
curl -s $B/payouts/$PID -H "$AUTH"                                       # submitted
curl -s $B/customers/$CID/balances -H "$AUTH"                            # the authorized maximum is held
```

Send the create again with the same `Idempotency-Key`. The response is the same payout, with `Idempotent-Replayed: true`.

Now settle it at the fake. Its webhook arrives signed, Ferry verifies it, re-fetches the transaction from Noah, and settles:

```bash
TX=$(curl -s localhost:18090/_control/transactions | jq -r ".Items[] | select(.ExternalID==\"$PID\") | .ID")
curl -s -X POST localhost:18090/_control/transactions/$TX/settle
curl -s $B/payouts/$PID -H "$AUTH"                                       # settled, within a second
curl -s $B/customers/$CID/balances -H "$AUTH"                            # 199.50: 100 paid + fee; the rest of the hold returned
```

> "Ferry held the most the payout could cost, then settled the actual amount and released the rest."

## 4. A crash in the middle (1.5 min)

Stop the worker (Ctrl-C), then create and confirm a second payout (`order-2`, `confirm-2`). It stays `held`: the event waits in the queue. Start `make worker` again, and it submits the payout.

> "Every step is written to the database before anything external happens, and the Nonce sent to Noah is saved before the call. If the worker dies after Noah received the payout but before Ferry recorded it, the retry reuses that Nonce, so Noah sees the same payout, not a new one."

## 5. A bank return (1 min)

```bash
curl -s -X POST localhost:18090/_control/transactions/$TX/reverse
curl -s $B/payouts/$PID -H "$AUTH"                                       # reversed
curl -s $B/customers/$CID/balances -H "$AUTH"                            # the returned amount is back
```

> "`settled` isn't final: banks return payments days later, and the ledger books the return as a new entry. Nothing is ever edited."

## 6. Reconciliation (1.5 min)

```bash
make recon                                                               # no breaks
TOPUP=$(curl -s -X POST localhost:18090/_control/credit/50 | jq -r .ID)    # 50 USDC arrives at Noah unannounced
go run ./cmd/recon -settle 0                                             # break: a top-up the ledger doesn't know
curl -s $B/admin/recon-breaks -H "$ADMIN"
curl -s -X POST $B/admin/treasury-topups -H "$ADMIN" -d "{\"rail_tx_id\":\"$TOPUP\"}"   # amount read from Noah
go run ./cmd/recon -settle 0                                             # break cleared
```

> "Reconciliation compares the ledger with the chain and with Noah, and reports what disagrees. It never changes money itself. The operator books the top-up by its Noah transaction id, and Ferry reads the amount from Noah, so nobody types a number."

## 7. Seeing it (1 min)

- **Grafana** (<http://127.0.0.1:3000>, *Ferry* dashboard): payouts by state, rail latency, outbox lag, queue depth, watcher lag, open breaks.
- **Jaeger** (<http://127.0.0.1:16686>, service `ferry-api`): open the confirm request. One trace runs API → outbox → queue → worker → Noah HTTP call.
- **Prometheus** (<http://127.0.0.1:9090/alerts>): the alert rules, all green.

## 8. Security and AWS (1 min)

- `docs/threat-model.md`: STRIDE per component, and a register of every residual risk (closed, accepted, later).
- `docs/self-audit.md`: issues found in Ferry's own code.
- `deploy/terraform`: every key in KMS, usable only by the services named in its key policy; one signed image deployed by digest. `make tf-check` runs validate, terraform test, tflint and checkov without an AWS account.

## After recording

```bash
make down           # stops the containers and deletes their volumes
```

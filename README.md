# Ferry

A security-first stablecoin payout engine in Go. A business deposits USDC on-chain, then requests a fiat payout to a bank account. Ferry quotes the payout through a fiat rail, holds the funds, submits the payout and settles a double-entry ledger.

The design, its invariants and its failure modes are in [`docs/design.md`](docs/design.md).

## Status

| Milestone | State |
| --- | --- |
| Repo scaffold, CI | done |
| M1 — Ledger core | in progress: schema, constraints and DB role done; Go posting logic next |
| M2–M7 | planned |

What works today: `make migrate` creates the ledger schema, and the database itself rejects unbalanced entries, negative customer balances and any edit or delete of posted history. Nothing runs end to end yet.

## Run locally

Requires Go (version in `go.mod`), Docker and make.

```bash
cp .env.example .env   # adjust if needed
make up                # Postgres 16 on 127.0.0.1:5432
make migrate           # apply migrations
make test              # go test -race ./... (integration tests need Docker; -short skips them)
make lint              # go vet, golangci-lint, gosec, govulncheck
make help              # all targets
```

The pinned tool versions are installed into `./bin` on first use.

## License

See [LICENSE](LICENSE).

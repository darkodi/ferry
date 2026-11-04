# Ferry

A security-first stablecoin payout engine in Go. A business deposits USDC on-chain, then requests a fiat payout to a bank account. Ferry quotes the payout through a fiat rail, holds the funds, submits the payout and settles a double-entry ledger.

The design, its invariants and its failure modes are in [`docs/design.md`](docs/design.md).

## Status

| Milestone | State |
| --- | --- |
| Repo scaffold, CI | done |
| M1 — Ledger core | done |
| M2–M7 | planned |

What works today: the ledger core. `internal/ledger` posts balanced entries for every event in the design (deposit, top-up, hold, settle, release, bank return, correction), refuses overdrafts even under 100 concurrent requests, deduplicates replayed events, and checks its own invariants. The database rejects unbalanced entries, negative customer balances and any edit of posted history even if the Go code has a bug. There is no API yet (M2), so nothing runs end to end.

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

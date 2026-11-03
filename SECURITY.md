# Security policy

Ferry is a portfolio project that runs only against testnets and rail sandboxes. It holds no real funds.

## Reporting a vulnerability

Please report privately through GitHub's **Report a vulnerability** button (Security → Advisories) on this repository. Do not open a public issue.

Include the affected component, steps to reproduce and the impact you expect. You can expect an acknowledgement within 7 days.

## Scope

- Ledger correctness: anything that creates, loses or double-spends money in the ledger.
- Idempotency and replay: duplicate payouts, webhook forgery or replay.
- Secrets handling and key management.
- Infrastructure code in `deploy/`.

Out of scope: third-party services (Noah, RPC providers) and testnet-only denial of service.

## How Ferry tracks its own findings

- Threat model: [`docs/threat-model.md`](docs/threat-model.md).
- Self-audit findings and fix PRs: [`docs/self-audit.md`](docs/self-audit.md).

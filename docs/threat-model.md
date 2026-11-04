# Threat model

STRIDE analysis per component and trust boundary. Each component adds its section when it is built (see `CLAUDE.md`).

## Trust boundaries

| # | Boundary | Crossed by | Since |
| --- | --- | --- | --- |
| TB1 | Ferry binaries → Postgres | SQL as the `ferry_app` role | M1 |
| TB2 | Operator / migrations → Postgres | SQL as the schema owner | M1 |

Later milestones add the public API, inbound rail webhooks, chain RPC and the queue.

## Ledger (M1)

**Assets:** account balances (`accounts.posted`) and the journal (`journal_entries`, `postings`), the source of truth for who owns what.

**Assumptions:**

- Application code may have bugs; the database must still refuse an invalid ledger.
- The `ferry_app` credentials may leak through an application compromise.
- The schema owner and Postgres superuser are trusted. They are used only by migrations and operators, never by the running binaries.

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing | A binary connects as the schema owner and bypasses grants | `ferry_app` is `NOLOGIN`; deployments create a separate login role that inherits it; owner credentials are not given to services | `TestAppRoleCannotLogIn` |
| **T**ampering: unbalanced entry | Bug or attacker posts debits ≠ credits, creating or destroying money | Deferred constraint trigger checks each entry at `COMMIT`: ≥ 2 postings, Σdebits = Σcredits per currency; also re-checks lines appended to old entries | `TestUnbalancedEntryRejectedAtCommit`, `TestEntryWithoutPostingsRejected`, `TestEntryWithOnePostingRejected`, `TestPostingAddedLaterToCommittedEntryRejected` |
| **T**ampering: rewriting history | Edit or delete a posting to hide a transfer | `ferry_app` has no `UPDATE`/`DELETE`/`TRUNCATE` on entries or postings; `BEFORE` triggers reject them for every role, including the owner | `TestAppRolePrivileges`, `TestEntriesAndPostingsAppendOnlyEvenForOwner` |
| **T**ampering: overdraft | Customer balance driven below zero | `PostEntry` locks the account rows (`FOR UPDATE`, ascending id) and refuses before writing; `CHECK (owner_id IS NULL OR posted >= 0)` as backstop | `TestCustomerAccountCannotGoNegative`, `TestInsufficientFundsWritesNothing` |
| **T**ampering: concurrent double spend | Two payouts race on one balance and both pass the check | Row locks serialise entries on the same account; ascending lock order prevents deadlocks | `TestConcurrentOverspend`, `TestConcurrentOverspendSerializable`, `TestNoDeadlockOppositeOrder`; mutation-checked by removing `FOR UPDATE` |
| **T**ampering: replayed event | A retried step or duplicated message posts the same entry twice | Unique `idempotency_key`; `PostEntry` returns the existing entry with `ErrDuplicateEntry` | `TestConcurrentSameKey`, `TestReleaseAtMostOnce`, property test `replay` action |
| **T**ampering: re-labelling an account | Change an account's `kind` or `owner_id` to move funds between customers | `ferry_app` may update only `posted` and `updated_at` | `TestAppRolePrivileges` |
| **T**ampering: malformed amounts | Zero, negative or overflowing amounts | `CHECK (amount > 0 AND amount <= 2^63-1)`; `posted` limited to the `int64` range | `TestPostingAmountBounds` |
| **T**ampering: wrong currency | A posting in a currency other than its account's | Composite FK `(account_id, currency)`; v1 allows only `USDC` | `TestPostingCurrencyMustMatchAccount`, `TestAccountKindAndCurrencyValid` |
| **R**epudiation | "That entry never happened" | Append-only journal; every entry has a unique deterministic `idempotency_key` and `created_at` | `TestIdempotencyKeyUnique` |
| **I**nformation disclosure | Balances read by an unauthorised party | DB not exposed outside the private network (local: bound to 127.0.0.1; AWS: private subnet, M7); API authorisation from M2 | — (M2, M7) |
| **D**enial of service | Hot house accounts serialise all payouts on their row locks | Accepted for v1; Evolution section describes sharding house accounts | — |
| **E**levation of privilege | `ferry_app` creates objects or grants itself rights | No `CREATE` on schema `public` (Postgres 15+ default) and no ownership | `TestAppRolePrivileges` ("create table") |

### Residual risks

- **`posted` can drift from the postings.** `ferry_app` can set `posted` to any non-negative value without a matching entry. The Go ledger updates it only inside `PostEntry`, and `ledger.CheckInvariants` (posted = Σ postings) detects drift (`TestInvariantsDetectDrift`). A database-level trigger tying `posted` to postings was rejected for v1 to keep the schema simple.
- **Double posting under a different key.** The database cannot tell that two entries with different `idempotency_key`s describe the same event. Deterministic keys built by `internal/ledger/rules.go` make this a code-review concern.
- **Owner and superuser are trusted.** They can drop the triggers. This is accepted: they run migrations only, and their use belongs in an audit log (M7).

## Payouts and the rail adapter (M2)

**Assets:** customer funds while a payout is in flight (`held`), Ferry's pre-funded rail balance (`noah_prefunded`), and the payout state itself.

**Assumptions:**

- The rail is a separate party that can time out, repeat itself or report events out of order, and may (by bug or compromise) report something impossible.
- Payout code runs as `ferry_app`; customer input reaches it only through the API (PR 2 of M2 adds authentication).
- In M2 the rail is the in-memory mock; the Noah adapter (M5) adds its own transport threats (signing, webhooks).

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing: another customer's payout | Customer B reads or confirms customer A's payout by id | Every read and confirm is scoped to the calling customer; a foreign id is reported as not found, so ids cannot be probed | `TestOtherCustomerCannotSeePayout` |
| **T**ampering: duplicate payout | A retried create makes a second payout | `(customer_id, idempotency_key)` unique on `payouts`; a replay returns the same payout, a changed request under the same key is refused | `TestReplayedCreateReturnsSamePayout` |
| **T**ampering: double send | A timeout leads to a second submit and the rail pays twice | Nonce stored before the call; retries reuse it; unknown outcomes keep the hold and set `needs_resolution` instead of releasing | `TestSubmitTimeoutKeepsHoldAndReusesNonce` |
| **T**ampering: release after the rail paid | Funds released while the rail actually executed the payout | Release only on a definite `ErrRejected` or a rail-reported `Failed`; any other error keeps the hold | `TestSubmitTimeoutKeepsHoldAndReusesNonce`, `TestRailRejectsSubmit` |
| **T**ampering: stale or replayed rail report | An old `Pending` overwrites `Settled`, or a duplicate settles twice | Reports applied only if their version is newer (`UPDATE … WHERE last_event_version < $v`); transitions guarded by status + version; ledger keys per payout step | `TestSettleReturnsUnusedHold` (repeated sync) |
| **T**ampering: impossible rail report | Rail reports a settlement above the hold, a return above the amount paid, or another payout's transaction | Rejected with `ErrRailMismatch`; nothing is applied and the payout waits for a human | — (code review; Noah adapter tests in M5) |
| **T**ampering: stale price | A payout executes on an expired quote | Expiry checked inside the hold transaction; lapsed quotes move to `expired` | `TestQuoteExpired` |
| **T**ampering: overspend | Hold larger than the available balance | Hold goes through `ledger.PostEntry` (row locks, overdraft refusal) | `TestInsufficientFunds` |
| **R**epudiation | "Ferry never told me it settled" | Every transition writes an outbox event in the same transaction; rail attempts keep nonce, rail id and last event version | — (relay in M3) |
| **I**nformation disclosure | API keys leaked from the database | Only SHA-256 hashes stored; keys are 32 random bytes, prefixed `ferry_sk_` so scanners recognise leaks | — |
| **D**enial of service | Payouts stuck in `held` with `needs_resolution` | Flag is visible on the payout; the M3 resolver retries with the same nonce | — (M3) |
| **E**levation of privilege | App edits quotes or deletes payouts | `ferry_app` may insert quotes but not update them, may update only status columns of payouts and rail attempts, and has no `DELETE` | — (grants in `00004_payouts.sql`) |

### Residual risks

- **`needs_resolution` needs a resolver.** Until M3, a payout whose submit timed out stays `held` until it is submitted again by hand.
- **Rail mismatch is not alerted yet.** `ErrRailMismatch` is returned and logged; the alert arrives with metrics in M6.
- **`accounts.owner_id` is not a foreign key to `customers`**, so the database alone does not stop accounts for unknown owners. `customer.Create` opens accounts in the same transaction as the customer row.

# Threat model

STRIDE analysis per component and trust boundary. Each component adds its section when it is built (see `CLAUDE.md`).

## Trust boundaries

| # | Boundary | Crossed by | Since |
| --- | --- | --- | --- |
| TB1 | Ferry binaries → Postgres | SQL as the `ferry_app` role | M1 |
| TB2 | Operator / migrations → Postgres | SQL as the schema owner | M1 |
| TB3 | Internet → API | HTTPS requests with a customer API key or the operator key | M2 |
| TB4 | Ferry binaries ↔ queue | SQS messages (ElasticMQ locally) carrying outbox events | M3 |
| TB5 | Chain RPC node → watcher | JSON-RPC responses: headers, block hashes, logs | M4 |

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

- ~~**`needs_resolution` needs a resolver.**~~ Resolved in M3: `payout.Resolver` resubmits stale `held` payouts with the stored nonce and re-fetches stale `submitted` ones.
- **Rail mismatch is not alerted yet.** `ErrRailMismatch` is returned and logged; the alert arrives with metrics in M6.
- **`accounts.owner_id` is not a foreign key to `customers`**, so the database alone does not stop accounts for unknown owners. `customer.Create` opens accounts in the same transaction as the customer row.

## Public API (M2)

**Assets:** customer API keys, the operator key, and every action the API can trigger (payouts, dev deposits).

**Assumptions:**

- Anyone on the internet can reach the API (TB3). TLS terminates in front of it (load balancer in M7; local dev is plain HTTP on 127.0.0.1).
- Customers are authenticated but untrusted: they may send malformed, oversized or replayed requests, or try other customers' ids.

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing: no or stolen key | Requests without a key, or with a guessed one | Bearer API key on every customer route; keys are 32 random bytes, stored as SHA-256 | `TestAuth` |
| **S**poofing: operator routes | A customer creates customers or fake deposits | Separate operator key (≥ 32 chars, from env), compared in constant time; customer keys are refused on operator routes and vice versa | `TestAuth` |
| **T**ampering: duplicate requests | A client retry creates two payouts | `Idempotency-Key` required; 100 identical concurrent requests produce one payout | `TestIdempotencyKey`, `TestConcurrentIdenticalCreates` |
| **T**ampering: malformed input | Unknown fields, floats for amounts, trailing JSON, bad currency | Strict JSON decoding (`DisallowUnknownFields`, single object), integer minor units, domain validation; amounts returned as decimal strings | `TestInputValidation` |
| **T**ampering: free money | Dev deposit route used in production | `/v1/dev/*` exists only when `FERRY_ENV=dev`, and needs the operator key | — (route not registered otherwise) |
| **R**epudiation | "I never sent that payout" | Every request is logged with request id, path, status and idempotency key; payout steps are in the outbox | — |
| **I**nformation disclosure: enumeration | Probing payout or customer ids | Another customer's resources return 404, the same as ids that do not exist; internal errors return only a request id | `TestAuth` |
| **D**enial of service | Huge bodies or slow clients | 64 KiB body limit; server read, write and header timeouts | — (rate limits in M7) |
| **E**levation of privilege | A customer key acting on another customer | The customer id comes from the key, never from the request body | `TestAuth` |

### Residual risks

- **No rate limiting or key rotation yet.** Rate limits and per-key scopes are planned for M7; a leaked key stays valid until `revoked_at` is set by hand.
- **Operator key is a single shared secret.** Acceptable for a portfolio deployment; a real one would use per-operator credentials and an audit log (M7).
- **Customer creation is not idempotent.** It is an operator action; a retried call creates a second customer with a new key.

## Outbox relay and queue (M3)

**Assets:** the events that drive payouts (`payout.held` makes a worker submit to the rail), and their delivery.

**Assumptions:**

- The queue is private infrastructure (TB4). On AWS only Ferry's IAM roles can send to or read from it (M7); locally ElasticMQ is bound to 127.0.0.1.
- Messages are hints, not authority: a consumer reloads the payout from Postgres and acts only if its state allows it. A message cannot move money by itself.

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing: forged message | Someone with queue access injects `payout.held` for a payout | Consumers act on the payout's state in Postgres, not on the message body; a payout that is not `held` is skipped, and a held one was already confirmed by its customer | `TestWorkerSubmitsOnceForDuplicateEvents` |
| **T**ampering: duplicates | Relay dies after publishing, before marking | At-least-once delivery by design; same message id on resend; `inbox` dedup plus state checks and the stored rail nonce | `TestRelayRepublishesAfterLostAck`, `TestHandleRunsOnce` |
| **T**ampering: lost events | Relay crashes mid-batch | Rows stay unpublished until marked; the lease expires and another round sends them | `TestRelayRepublishesAfterLostAck` |
| **R**epudiation | "The event was never sent" | `outbox.published_at` records when each event left; the row is never deleted | — |
| **I**nformation disclosure | Queue contents read by others | Payloads carry ids and states only, no keys or bank details; IAM-scoped queue on AWS | — |
| **D**enial of service: poison message | One bad message blocks the worker forever | After 5 deliveries SQS moves it to the DLQ; a non-empty DLQ alerts (M6) | `TestSQS/poison_message_goes_to_the_DLQ` |
| **D**enial of service: double publishing | Several relays send the same rows | Rows are claimed with `FOR UPDATE SKIP LOCKED` and a lease, so each relay takes different rows | `TestRelaysDoNotOverlap` |
| **E**levation of privilege | Relay role changes payouts | The relay only reads outbox rows and updates `published_at`/`claimed_until` (column grants) | Migration grants |

### Residual risks

- **Messages are not signed.** Anyone with queue access can inject or replay messages; they cannot move money a customer did not confirm, but they can trigger extra work. IAM-scoped queues (M7) are the control.
- **No alert on the DLQ or outbox lag yet.** Metrics and alerts arrive in M6.
- **Published outbox rows and inbox rows are never pruned.** Archiving or partitioning by time is on the Evolution list.

## Payout worker and resolver (M3)

**Assets:** held customer funds and the rail submission, which spends Ferry's pre-funded rail balance.

**Assumptions:**

- The worker trusts nothing in a message except as a pointer: it reloads the payout and acts only on its current state.
- The rail can time out after accepting a payout. Only the rail's own answer, fetched by Ferry, moves money.

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **T**ampering: double submit | Event delivered three times, or worker and resolver submit the same payout at once | Inbox skip; `Submit` acts only on `held`; one attempt row per payout with a stored nonce; rail dedups by nonce; transitions are version-guarded | `TestWorkerSubmitsOnceForDuplicateEvents`, `TestSubmitTimeoutKeepsHoldAndReusesNonce` |
| **T**ampering: releasing funds the rail may have spent | Timeout treated as failure, hold released, rail pays anyway | An unknown outcome keeps the hold (`needs_resolution`); release only after the rail confirms `Failed` | `TestResolverResolvesUnknownOutcome` |
| **R**epudiation | "The payout was never sent" | Each rail attempt is a `rail_transactions` row with nonce and rail id; worker and resolver log every outcome with `payout_id` | — |
| **D**enial of service: stuck payouts | Lost event, missed webhook, poison message | Resolver re-drives stale `held`/`submitted` payouts; bad messages go to the DLQ instead of blocking the queue | `TestResolverWaitsOutReviewThenSettles`, `TestWorkerDeadLettersBadMessages` |
| **E**levation of privilege: auto-cancel | Resolver gives up on a payout under compliance review and releases funds | The resolver never cancels or releases; only a rail-reported `Failed` releases | `TestResolverWaitsOutReviewThenSettles` |

### Residual risks

- **No SLA alert yet.** A payout stuck in review for days is re-fetched but nobody is paged; the alert on `submitted` beyond SLA comes with metrics in M6.
- **Resolver polls every stale `submitted` payout each round.** Fine at portfolio scale; at volume it needs backoff per payout (e.g. a `next_sync_at` column).
- **Dev mode runs everything in the API process.** A crash of the API stops the worker too. Separate binaries are used once the rail is shared (M5).

## Chain watchers (M4)

**Assets:** customer balances (a deposit credit creates spendable money), the deposit address keys, and the scan cursor.

**Assumptions:**

- The RPC node (TB5) is a third party. It can be wrong, lagging, or serving a fork that later loses, and on testnet it may be malicious.
- Anyone can send any token to a deposit address and emit any event from their own contract.
- Ferry holds only the account xpub. The private key that can move funds out of deposit addresses lives outside Ferry, for the sweep, which is not built yet.

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing: fake USDC | A contract emitting the same `Transfer(address,address,uint256)` event | Only logs from the configured USDC contract address are accepted; the log's address is re-checked even though the filter asked for it | `TestRescanAndNoiseIgnored` |
| **T**ampering: reorg | A deposit's block is replaced, and the transfer is double-spent elsewhere | Credit only after N confirmations; cursor-hash check, rewind and orphaning; nothing credited before confirmation | `TestReorgOrphansDeposit` |
| **T**ampering: lagging or lying RPC | Logs served from a fork the node later drops | The deposit's block hash is re-checked right before crediting; on mismatch it is orphaned and re-scanned | `TestCreditVerifiesBlockHash` |
| **T**ampering: double credit | A re-scan after a crash, a re-included transfer, or two watchers | Unique `(chain, tx_hash, log_index)`; the orphaned row is revived, not duplicated; confirm is a guarded `pending → confirmed` update with the ledger's idempotency key; one leader per chain | `TestRescanAndNoiseIgnored`, `TestReorgReincludedDepositCreditedOnce` |
| **T**ampering: amount overflow | A uint256 amount above the int64 range wraps negative | `ParseAmount` refuses zero and anything above int64 (never truncates); the database also checks the range | `TestParseAmount`, `FuzzParseAmount` |
| **R**epudiation | "My deposit never arrived" | Every detected transfer is stored with tx hash, log index and block; the status history shows pending → confirmed or orphaned | — |
| **I**nformation disclosure | Linking customers through their addresses | Each customer has their own address. Addresses are public on-chain by nature, and the API shows them only to their owner | `TestDepositAddresses` |
| **D**enial of service | Dust spam to deposit addresses, or a huge log range | Batches of at most 500 blocks; each dust transfer is one small row (credited as a real deposit) | — |
| **E**levation of privilege: key exposure | The xpub leaks | An xpub reveals addresses (privacy), not funds. `NewDeriver` refuses private keys, so the API and watcher config cannot hold one | `TestDeriverRejectsBadKeys` |

### Residual risks

- **Deep reorgs are only logged.** A reorg deeper than the confirmation depth that removes a credited deposit needs a human `correction`, and there is no alert until M6. Keep confirmations conservative on testnet (12 by default).
- **A single RPC provider.** The watcher trusts one node for liveness. Cross-checking several providers, or reconciliation against them (M6), would catch a node that consistently lies.
- **The topic filter lists every deposit address.** That is fine at portfolio scale; at volume, filter only by contract and match locally, or use a log indexer.
- **Dust deposits are credited.** Without a minimum deposit, spam creates rows and tiny credits; a minimum and a sweep threshold come with the sweep.
- **Funds in deposit addresses are not swept yet.** They are recorded under `chain_omnibus`, but moving them needs the sweep (deferred).

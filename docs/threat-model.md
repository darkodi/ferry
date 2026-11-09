# Threat model

STRIDE analysis per component and trust boundary. Each component adds its section when it is built (see `CLAUDE.md`).

## Trust boundaries

| # | Boundary | Crossed by | Since |
| --- | --- | --- | --- |
| TB1 | Ferry binaries → Postgres | SQL as the `ferry_app` role | M1 |
| TB2 | Operator / migrations → Postgres | SQL as the schema owner | M1 |
| TB3 | Internet → API | HTTPS requests with a customer API key or the operator key | M2 |
| TB4 | Ferry binaries ↔ queue | SQS messages (ElasticMQ locally) carrying outbox events | M3 |
| TB5 | Chain RPC node → watcher | JSON-RPC responses: headers, block hashes, logs (EVM); slots, signatures, transactions with token balances (Solana) | M4 |
| TB6 | Solana watcher → Solana cluster | Account-creation transactions signed with the authority key | M4 |
| TB7 | Ferry → Noah Business API | HTTPS requests with the API key and an ES384 request signature; customer bank details in prepare bodies | M5 |
| TB8 | Internet → webhook endpoint | Noah's Transaction webhooks: a public route authenticated only by the body's ECDSA signature | M5 |

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

## Solana deposits (M4)

The Solana watcher shares the engine above (cursor, idempotent credit, leader lock); this section covers what is Solana-specific. Unlike the EVM side, it holds a private key: the **authority**, which is the seed base of every deposit address and pays account rent (TB6).

**Assets:** the authority key and its SOL; the **deposit owner** (token owner of every deposit account, the only key that can move deposited USDC; Ferry holds only its public key); customer balances.

**Assumptions:**

- The RPC node (TB5) is untrusted, as for EVM. Ferry reads only `finalized` data, so a node can lag or lie but an honest node cannot roll a credited deposit back.
- Anyone can send SOL or any token to any address, and can call any program in a transaction that touches a deposit account.
- Only the operator creates customers, and each customer costs one account's rent (about 0.002 SOL) from the authority.

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing: fake USDC | A token from another mint, or from Token-2022, credited as USDC | A balance counts only if its mint is the configured USDC mint, its program is the Token program, and its owner is the deposit owner. A deposit account is created for the USDC mint and cannot hold another | `TestCredits` |
| **S**poofing: someone else's account at our address | An attacker creates the account first, with themselves as owner | `createWithSeed` addresses can only be created with the base's (authority's) signature. Provisioning still checks an existing account's program, size, mint, owner, state, delegate and close authority, and refuses to activate a mismatch | `TestProvisionOnce` |
| **T**ampering: rollback | A deposit's slot is dropped after it was credited | Only `finalized` commitment is read (slots, signatures, transactions, blocks) | — (property of the commitment level) |
| **T**ampering: lying RPC | A forged transaction or token balance | The returned transaction's first signature must match the one asked for, its slot must be in range and it must have succeeded. Token balances are checked per entry (index in range and listed once, mint, owner, program, canonical amounts) | `TestCredits`, `FuzzCredits`, `FuzzParseAmount` |
| **T**ampering: double credit | A re-scan, or one transaction touching two deposit accounts | Transfer identity is (signature, account index); the shared unique key, guarded update and ledger idempotency key apply | `TestDepositCreditedOnce` |
| **T**ampering: amount overflow | A u64 balance increase above int64 | Refused, never truncated; the database also checks the range | `TestCredits`, `FuzzCredits` |
| **D**enial of service: blocked address | A stranger sends SOL to a not-yet-created address, which makes `createAccountWithSeed` fail | A bare system account is topped up and allocated with `allocateWithSeed` | `TestProvisionPrefundedAddress` |
| **D**enial of service: drained authority | Rent for many customers, or fees for failing retries, empties the authority | Only the operator can create customers; provisioning is at most 20 addresses per round, and an address that fails is retried, not re-funded | — |
| **E**levation of privilege: authority key leak | The watcher host or its key file is compromised | The authority is not the token owner of any deposit account, so it cannot move USDC; the watcher refuses an owner equal to the authority. A leak exposes the authority's SOL and lets an attacker create accounts at Ferry's seeds, which provisioning detects as mismatches | `TestProvisionerRefusesAuthorityAsOwner` |
| **E**levation of privilege: wrong authority | The API and the watcher are configured with different authorities | The watcher recomputes each address from its own key and refuses on mismatch, so it never creates an account nobody watches | — |

### Residual risks

- **The authority key is a file in dev.** It is read from `FERRY_SOLANA_AUTHORITY_KEYPAIR` (git-ignored `.dev/` locally). On devnet keep it outside the repo with tight permissions, and move it to KMS in M7.
- **Squatting with a leaked authority.** Someone with the authority key could create an account at a future customer's address with themselves as owner. Provisioning would refuse to activate it, but the address would be lost and need an operator to re-derive it. KMS (M7) and keeping the key on the watcher host only limit this.
- **One RPC call per deposit account per round.** `getSignaturesForAddress` pages back from the newest finalized signature each round. That is fine at portfolio scale; at volume, remember the last signature per account (`until`) or use a webhook or indexer.
- **The validator container runs without seccomp.** Agave 3 needs `io_uring`, which Docker's default seccomp profile blocks. This applies only to the local and test validator, which binds to loopback; a custom profile that allows just `io_uring_*` would be tighter.
- **Dust deposits are credited**, as on EVM.

## Noah rail adapter and destinations (M5)

`internal/rail/noah` talks to Noah over TB7; `internal/destination` stores the bank accounts payouts go to. Webhooks (TB8) are below.

**Assets:**
- the Noah API key and request-signing key (together they can spend Ferry's pre-funded balance);
- customers' bank details (personal data);
- the data key that encrypts them;
- the payout hold, which must not be released while money is moving.

**Assumptions:**
- Noah is a trusted counterparty but can time out, repeat itself or answer inconsistently.
- The network path to Noah is TLS; anything that can read process memory or environment can read the keys.
- Database backups and read access are wider than application access.

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing: forged request to Noah | Someone with the API key alone moves money | Requests are signed (ES384 JWT bound to method, path, query and body hash, 5-minute life); production refuses to start without a signing key | `TestSignClaims`, `TestUnsignedRequestsRefused`, `TestClientConfig` |
| **T**ampering: request altered in transit | Body or query changed after signing | The signature covers a hash of the exact body bytes and the query; Noah (and the fake) reject a mismatch | `TestSignClaims` |
| **T**ampering: redirect | A redirect resends the API key and signed body elsewhere | The client never follows redirects | `TestNoRedirects` |
| **T**ampering: release while money moves | A refusal answer after a lost success releases the hold | 4xx is a rejection only after a lookup by `ExternalID` finds nothing; other errors keep the hold and resend the same Nonce | `TestNoahRefusedButCreatedIsNotReleased`, `TestNoahLostAnswerResentWithSameNonce` |
| **T**ampering: wrong settlement amount | Noah reports Settled with amounts that do not add up | `Breakdown` must sum to `Amount` and parse exactly; otherwise nothing is posted | `TestSettledBreakdownMustAddUp`, `TestNoahUnexplainedSettlementNotApplied` |
| **T**ampering: bank details swapped in the database | An attacker with DB write access points a destination at their account | AES-GCM authenticates each ciphertext with its row id as associated data; a moved or edited ciphertext does not decrypt, and quoting fails closed | `TestTamperedOrWrongKeyRefused` |
| **T**ampering: lax amounts | Noah sends `.5`, `+5` or 7 decimals | Strict parser: refused, never rounded | `TestAmounts`, `FuzzParseNoahAmount` |
| **R**epudiation | "Ferry sent a different amount" | Each sell carries prepare's authorized amount unchanged, a stored Nonce and `ExternalID = payout_id`; Noah's error `Instance` is kept in the error | — |
| **I**nformation disclosure: bank details at rest | A database dump or backup leaks account numbers | Encrypted with `FERRY_DATA_KEY`; only "BankAch ****6789" in clear; the idempotency store keeps only the masked response | `TestFormEncryptedAtRest` |
| **I**nformation disclosure: bank details in logs | Request bodies or errors logged | The Noah client never logs; the API never echoes the form; the manual run checked every log for the account number | manual e2e (README) |
| **I**nformation disclosure: another customer's destination | Probe ids to read or pay to someone's account | Every read and quote is scoped to the caller; a foreign id is "not found" | `TestOwnershipAndCheck`, API `TestInputValidation` |
| **E**levation of privilege: worker reads bank details | A compromised worker leaks the forms | The worker never gets `FERRY_DATA_KEY`; its store can check and remember payment methods only | `TestNoKeyStore` |
| **E**levation of privilege: app edits destinations | Change a destination's bank details | `ferry_app` may update only `rail_payment_method_id`, `status` and `updated_at`; new details mean a new destination | — (grants in `00009_destinations.sql`) |

### Residual risks

- **Keys are environment variables and files.** `NOAH_API_KEY`, the signing key file and `FERRY_DATA_KEY` sit in the process environment or on disk until KMS (M7). Anyone who can read them can spend the Noah balance or decrypt bank details.
- **One data key, no rotation yet.** `key_id` records which key encrypted each row, so rotation is possible, but there is no re-encryption job.
- **Sandbox behaviour is assumed, not observed.** Until a sandbox key exists, these come from the docs, encoded in the fake: what a resent Nonce returns, the authorized-amount format, `queryParams` typing in the signature, and Breakdown on settlement. `sandbox_test.go` checks them.
- **The lookup by `ExternalID` is bounded** to the last 7 days and 10 pages of 100 transactions. If the page limit is reached first, the outcome is reported as unknown and the hold is kept. A sale older than 7 days would not be found. That is unrealistic, because the resolver resends within minutes of an unclear answer.
- **The masked display can reveal the last four digits.** That is standard practice, but it is still personal data in the API response.

## Noah webhooks (M5)

`POST /v1/webhooks/noah` is public: anyone on the internet can reach it, and the only authentication is Noah's signature over the body (TB8).

**Assets:** payout state, and the customer balances it drives; the endpoint's availability (Noah retries only 7 times).

**Assumptions:**
- Attackers can send any bytes and replay any webhook they have seen.
- A real Noah webhook can arrive twice, out of order, or before Ferry has recorded the transaction id.
- Noah's signing key is safe; the published public keys are authentic (fetched from docs.noah.com and pinned in code).

### STRIDE

| Threat | Example | Mitigation | Verified by |
| --- | --- | --- | --- |
| **S**poofing: forged webhook | "Settled" for a payout Noah never paid | ECDSA P-384 signature over the raw body, checked before parsing, against Noah's pinned key; 401 and nothing stored | `TestWebhookSignatureRequired`, `TestNoahForgedWebhookRefused` |
| **S**poofing: config swaps the key | An attacker who can set env points verification at their key | Real environments use keys pinned in code; the override is a file path, for the fake | `TestPublishedWebhookKeys` |
| **T**ampering: a signed event that lies, or a compromised Noah key | A valid signature on a false "Settled" | A webhook never moves money: the worker re-fetches the transaction from Noah's API (TB7, signed) and applies that | `TestNoahForgedWebhookRefused` (signed half) |
| **T**ampering: replay | An old signed event resent | Unique `(rail, rail_tx_id, event_version)`; re-fetch queued only for a newer version; Sync is idempotent | `TestNoahWebhookReplayAndOrder` |
| **T**ampering: wrong payout | An event for another business's transaction | Matched by rail transaction id and rail; unmatched events are stored, not applied; the core also checks `ExternalID` on every apply | `TestNoahWebhookUnknownTransaction` |
| **R**epudiation | "We never got the webhook" | Every verified event is stored with its body before 2xx | `TestNoahWebhookSettlesPayout` (stored rows) |
| **D**enial of service: floods and big bodies | Many or huge requests | 1 MiB limit; verification before any database work; invalid signatures cost one ECDSA check | `TestWebhookSignatureRequired` |
| **D**enial of service: lost webhooks | Noah gives up after 7 tries during an outage | The resolver polls stale `submitted` payouts, so webhooks only make things faster | `TestResolverWaitsOutReviewThenSettles` |
| **E**levation of privilege | The webhook route reaches customer or admin routes | It is outside both auth groups and calls only `RecordRailEvent` | — |

### Residual risks

- **No rate limit or IP allowlist yet.** Noah publishes webhook IPs; an allowlist at the load balancer (M7) would cut the noise before the signature check.
- **A large reversal amount stops for a human.** Noah reports a reversal at the market rate, which may exceed what was spent; Ferry then logs `ErrRailMismatch` and leaves the payout `settled`. The alert arrives with M6.
- **The step after a reversal is unverified.** For pre-funded payouts, it's not yet known whether Noah's follow-up on-chain `AdjustmentFor` transaction ever moves Ferry's balance. Until the sandbox shows it, such transactions are stored, not applied.
- **Return lookup is bounded** to 10 pages of transactions after the sale. A return Noah books much later and buried under heavy traffic would be missed until reconciliation (M6).

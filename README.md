# custody-ledger

[![ci](https://github.com/vaibenz/custody-ledger/actions/workflows/ci.yml/badge.svg)](https://github.com/vaibenz/custody-ledger/actions/workflows/ci.yml)

A custodial wallet service in Go, built on a double entry ledger in PostgreSQL.

It holds customer balances across assets, moves value between them, and decides whether each withdrawal leaves custody automatically, waits for two humans to approve it, or is refused. The focus is correctness under concurrency and accountability:

* Retried or simultaneous requests can never double spend.
* The ledger can never fall out of balance.
* Every change is attributable to a named person in an append only audit log.

```
go test -race ./...   # unit + integration tests against real Postgres, 84% statement coverage
```

## Design

### Double entry ledger

Every movement of value is a **transaction** made of **entries** that sum to zero. A deposit of 5,000 sats writes `external −5000, alice +5000`. Each asset has three kinds of account:

| Kind | Purpose |
|---|---|
| `user` | A customer's balance. Can never go negative (a `CHECK` constraint). |
| `external` | The outside world: the counterparty for deposits and settled withdrawals, so the only kind that goes negative. Sharded, see below. |
| `hold` | Withdrawals waiting for review. Funds sit here so the customer cannot spend them twice. |

The zero sum rule is enforced **by Postgres, not just by Go**. A deferred constraint trigger checks every transaction at commit and aborts it if its entries do not balance. Cached balances are updated in the same database transaction as the entries, and the tests assert after every scenario that each cached balance equals the sum of its entries.

All amounts are `int64` in the asset's smallest unit. Floating point never touches money.

### Idempotency

Every money moving call takes an `Idempotency-Key`. The posting engine inserts the transaction row first with `ON CONFLICT (idempotency_key) DO NOTHING`:

* **Retry after success:** returns the original transaction with `replayed: true` (HTTP 200 instead of 201), and nothing moves.
* **Concurrent retries:** the second insert blocks on the unique index until the first commits, then sees the conflict. `TestConcurrentRetriesApplyOnce` fires 50 identical requests at once and checks exactly one applies.
* **Same key, different request:** a SHA 256 of the request is stored with the key, so this is a `409`, not a silent replay of the wrong thing.
* **Failed request:** the whole database transaction rolls back, including the key, so the client can retry.

### Locking and double spend prevention

Every touched account is locked with `SELECT ... FOR UPDATE` in one global order: customer accounts first, then system accounts, then by id. Transfers between the same pair in opposite directions therefore cannot deadlock. `TestConcurrentSpendsCannotDoubleSpend` races 40 spends of 100 against a balance of 1,000 and asserts exactly 10 succeed. `TestConcurrentRandomTransfersConserveValue` runs 1,000 random two way transfers on 16 workers and asserts value is conserved.

### Authentication and authorisation

Every route needs `Authorization: Bearer <token>`. There are three kinds of caller:

| Credential | Can | Cannot |
|---|---|---|
| **Customer** API key (`ck_...`) | open accounts, read and spend **their own** accounts, save addresses, withdraw | credit deposits, review, see anyone else's accounts |
| **Operator** token (`op_...`), one per person | credit deposits (the chain watcher), review withdrawals, read the audit log | open accounts, issue keys |
| **Root** (`ADMIN_TOKEN`) | create operators, issue and revoke customer keys | move money or review anything |

Keys are 256 bit random values. Only a SHA 256 of each is stored, so a database leak does not leak usable keys. A customer asking for someone else's account gets **404, not 403**, so account ids cannot be probed. `TestCustomersCannotTouchOthersAccounts` and `TestRolesAreEnforced` check every combination.

### Withdrawal risk engine

`internal/risk` is a pure function, table tested without a database:

| Rule | Outcome |
|---|---|
| Rolling 24h total would exceed the asset's daily limit | reject |
| Too many withdrawal attempts in the last hour (account takeover signal) | reject |
| Destination is not on the account's saved address list | review |
| Destination was saved less than 24h ago | review |
| Single amount above the review threshold | review |

The customer's account row is locked **before** withdrawal history is read, so 20 concurrent withdrawals cannot jointly slip past a daily limit. Re-adding a saved address keeps its original timestamp, so it cannot reset the hold.

### Four-eyes review

A held withdrawal settles only after **two different operators** approve it. The count is configurable per asset and is copied onto each withdrawal when it is requested. Approvals are keyed by `(withdrawal, reviewer)`, so one person can never count twice, and a single rejection releases the funds back to the customer. The reviewer is always taken from the credential, never from the request body.

```
                 ┌── approve ──> user → external                          [approved]
request ─ risk ──┼── review ───> user → hold ──┬─ 2 approvals ─> hold → external [approved]
                 │                             └─ any reject ──> hold → user     [rejected]
                 └── reject ───> nothing moves, attempt recorded           [rejected]
```

`TestConcurrentApprovalsSettleOnce` has ten operators approve at the same instant. That test found a real race. The first version read the existing approvals in the same statement that took the row lock. Under READ COMMITTED, a statement that waits for a row lock re-checks that row but still evaluates its subqueries against the snapshot from before the wait. A reviewer who waited therefore missed an approval committed in the meantime, and a third approval got in. The fix is to lock the row first, then read the approvals in a fresh statement.

### Transactional outbox

Approving a withdrawal, automatically or on the second review, inserts a `withdrawal.send` event in the **same database transaction**. So "approved" and "tell the signer to send it" either both happen or neither does. `cmd/relay` delivers events:

* It claims due events with `FOR UPDATE SKIP LOCKED`, so several relays can run at once without handing out the same event. A test has four relays drain 300 events, each delivered exactly once.
* Failures retry with capped exponential backoff without blocking the events behind them.
* Delivery is at least once, and the withdrawal id is the consumer's idempotency key.

In this repo the relay logs each event. In a real custodian it would call the signing service.

### Audit log

Every state change writes a row to `audit_log` in the same transaction: account opened, deposit, transfer, address added, withdrawal requested, each approval, the decision, and key or operator issued or revoked. Each row records who did it (`customer:alice`, `operator:ops-1`). A trigger refuses `UPDATE` and `DELETE`, so history cannot be rewritten even by application code. Operators read it with `GET /v1/audit/{subject}`.

### Sharded external account

Every deposit and settlement used to lock the asset's single external row, so parallel deposits queued behind one lock. Each asset's external account is now spread over 8 rows; a posting picks one at random and `ExternalPosition` sums them.

## API

| Method | Path | Who | |
|---|---|---|---|
| `POST` | `/v1/accounts` | customer | `{"asset":"BTC"}`, owner comes from the key |
| `GET` | `/v1/accounts` · `/v1/accounts/{id}` · `/{id}/entries` | customer | own accounts only |
| `POST` | `/v1/accounts/{id}/addresses` | customer | save a withdrawal address |
| `POST` | `/v1/transfers` | customer | `{"from","to","amount"}`, from must be yours |
| `POST` | `/v1/withdrawals` | customer | → 201 approved, 202 held, 422 refused |
| `GET` | `/v1/withdrawals/{id}` | customer, operator | |
| `POST` | `/v1/deposits` | operator | credit a confirmed on chain deposit |
| `GET` | `/v1/reviews` | operator | held withdrawals |
| `POST` | `/v1/reviews/{id}/approve` · `/reject` | operator | |
| `GET` | `/v1/audit/{subject}` | operator | |
| `POST` | `/v1/admin/operators` · `/v1/admin/keys` | root | returns the secret once |
| `DELETE` | `/v1/admin/keys/{id}` | root | revoke |

Money moving endpoints require an `Idempotency-Key` header.

## Running

```bash
export ADMIN_TOKEN=$(openssl rand -hex 32)
docker compose up --build -d     # Postgres, API on :8080, outbox relay
make test                        # needs DATABASE_URL (defaults to the compose db)
make bench
```

Integration tests create a fresh Postgres schema per test and drop it afterwards, so they run in parallel against one database. Without `DATABASE_URL` they are skipped.

## Benchmarks

Full end to end operations (row locks, entries, balance updates, audit row and the commit time balance check) from 8 parallel clients, on a 2 vCPU cloud VM with PostgreSQL 16 on the same machine and `fsync` on:

| Operation | Throughput |
|---|---|
| Transfer between customers | about 930/s |
| Deposit, 1 external shard | about 380/s |
| Deposit, 8 external shards | 790 to 860/s |

Sharding the external account roughly doubles deposit throughput. Transfers do not touch it, and level off at the commit rate the disk allows.

## Deploying to AWS

See [`deploy/aws.md`](deploy/aws.md). A CloudFormation stack creates one EC2 instance running everything behind Caddy with automatic HTTPS, an ECR repository, and an OIDC role for GitHub Actions. After CI passes on `main`, the deploy workflow ships the new images with SSM Run Command and checks `/healthz`. No AWS keys are stored anywhere.

## Limits and next steps

* **Address validation** is a loose format check. Real validation is per chain (bech32, EIP 55 checksums) and belongs in a chain adapter.
* **One instance.** The deploy trades high availability for cost. Production would run Postgres on RDS Multi-AZ and the API behind a load balancer.
* **Operators and keys** are managed through the API. A real system would put operators behind SSO with hardware second factors.

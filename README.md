# custody-ledger

A custodial wallet service in Go, built on a double entry ledger in PostgreSQL.

It holds balances for customers across assets, moves value between them, and decides whether each withdrawal can leave custody automatically, needs a human to look at it, or must be refused. The focus is correctness under concurrency. Retried or simultaneous requests can never double spend a balance, and the ledger can never fall out of balance.

```
go test -race ./...   # unit + integration tests against real Postgres
```

## Design

### Double entry ledger

Every movement of value is a **transaction** made of **entries** that sum to zero. A deposit of 5,000 sats writes `external −5000, alice +5000`. Each asset has three kinds of account:

| Kind | Purpose |
|---|---|
| `user` | A customer's balance. Can never go negative (a `CHECK` constraint). |
| `external` | The outside world. The counterparty for deposits and settled withdrawals, so it is the only account that goes negative. |
| `hold` | Withdrawals waiting on a reviewer. Funds sit here so the customer cannot spend them twice. |

The zero sum rule is enforced **by Postgres, not just by Go**. A deferred constraint trigger checks every transaction at commit and aborts it if its entries do not balance. Cached balances on `accounts` are updated in the same database transaction as the entries, and the test suite asserts after every scenario that each cached balance equals the sum of its entries.

All amounts are `int64` in the asset's smallest unit. Floating point never touches money.

### Idempotency

Every money moving call takes an `Idempotency-Key`. The posting engine inserts the transaction row first with `ON CONFLICT (idempotency_key) DO NOTHING`:

* **First request:** the insert succeeds and the transfer proceeds.
* **Retry after success:** the insert conflicts, and the original transaction is returned with `replayed: true` (HTTP 200 instead of 201). Nothing moves.
* **Concurrent retries:** the second insert blocks on the unique index until the first commits, then sees the conflict. `TestConcurrentRetriesApplyOnce` fires 50 identical requests at once and checks exactly one applies.
* **Same key, different request:** a SHA 256 of the request is stored with the key, so reusing a key for a different amount is a `409` rather than a silent replay of the wrong thing.
* **Failed request:** the whole database transaction rolls back, including the key, so the client can retry.

### Locking and double spend prevention

Before checking balances, the engine locks every account it touches with `SELECT ... FOR UPDATE`. Locks are always taken in one global order: customer accounts before system accounts, then by id. Two transfers between the same pair in opposite directions therefore cannot deadlock. A Postgres deadlock, should one ever happen, is retried.

`TestConcurrentSpendsCannotDoubleSpend` races 40 spends of 100 against a balance of 1,000 and asserts exactly 10 succeed. `TestConcurrentRandomTransfersConserveValue` runs 1,000 random two way transfers across 10 accounts on 16 workers and asserts total value is conserved.

### Withdrawal risk engine

`internal/risk` is a pure function, so every rule is table tested without a database:

| Rule | Outcome |
|---|---|
| Rolling 24h total would exceed the asset's daily limit | reject |
| Too many withdrawal attempts in the last hour (account takeover signal) | reject |
| Destination is not on the account's saved address list | review |
| Destination was saved less than `NewAddressHold` ago (default 24h) | review |
| Single amount above the review threshold | review |

The most severe outcome wins, and every reason is returned so a reviewer sees the full picture. The ledger locks the customer's account row **before** reading withdrawal history, so 20 concurrent withdrawals cannot jointly slip past a daily limit (`TestConcurrentWithdrawalsRespectDailyLimit`). Re-adding a saved address keeps its original timestamp, so it cannot be used to reset the hold.

```
                 ┌── approve ──> user → external                 [approved]
request ─ risk ──┼── review ───> user → hold ──┬─ approve ─> hold → external [approved]
                 │                             └─ reject ──> hold → user     [rejected]
                 └── reject ───> nothing moves, attempt recorded  [rejected]
```

## API

Money moving endpoints require an `Idempotency-Key` header. Review endpoints require `Authorization: Bearer $ADMIN_TOKEN`, compared in constant time.

| Method | Path | |
|---|---|---|
| `POST` | `/v1/accounts` | `{"owner","asset"}` |
| `GET` | `/v1/accounts/{id}` | balance |
| `GET` | `/v1/accounts/{id}/entries` | ledger history |
| `POST` | `/v1/accounts/{id}/addresses` | save a withdrawal address |
| `POST` | `/v1/deposits` | `{"account_id","amount"}` |
| `POST` | `/v1/transfers` | `{"from","to","amount"}` |
| `POST` | `/v1/withdrawals` | `{"account_id","address","amount"}` → 201 approved, 202 held, 422 refused |
| `GET` | `/v1/withdrawals/{id}` | |
| `GET` | `/v1/reviews` | admin: held withdrawals |
| `POST` | `/v1/reviews/{id}/approve` · `/reject` | admin: `{"reviewer"}` |

```bash
docker compose up --build -d
A=$(curl -s -XPOST localhost:8080/v1/accounts -d '{"owner":"alice","asset":"BTC"}' | jq -r .id)
curl -s -XPOST localhost:8080/v1/deposits -H 'Idempotency-Key: dep-1' \
     -d "{\"account_id\":\"$A\",\"amount\":50000}"
```

## Running

```bash
docker compose up --build -d     # Postgres + API on :8080
make test                        # needs DATABASE_URL (defaults to the compose db)
make bench
```

Integration tests create a fresh Postgres schema per test and drop it afterwards, so they run in parallel against one database. Without `DATABASE_URL` they are skipped and only the unit tests run.

## Benchmark

`BenchmarkTransfer` runs full transfers (one database transaction each, with row locks, two entries, two balance updates and the commit time balance check) from parallel clients over 100 funded accounts:

| Parallel clients | Throughput | Mean latency |
|---|---|---|
| 1 | 342 transfers/s | 2.9 ms |
| 8 | 1,108 transfers/s | 0.9 ms |
| 16 | 1,087 transfers/s | 0.9 ms |

Measured on a 2 vCPU cloud VM with PostgreSQL 16 on the same machine and default durability settings (`fsync` on). Throughput levels off at the commit rate the disk allows.

## Deploying to AWS

See [`deploy/aws.md`](deploy/aws.md): the image goes to ECR, runs on App Runner, and connects to RDS for PostgreSQL, with secrets in Secrets Manager.

## Limits and next steps

* **Hot system accounts.** Every deposit locks the asset's single external account. At real exchange volume it would be split into N shards, picked at random per posting and summed for reporting.
* **On chain broadcast.** An approved withdrawal moves value to `external` but does not sign or broadcast anything. The next step is a transactional outbox: write a `withdrawal_approved` event in the same database transaction, and let a separate signer service consume it.
* **Address validation** is a loose format check. Real validation is per chain (bech32, EIP 55 checksums) and belongs in a chain adapter.
* **Authentication** for customer endpoints is out of scope. Only the review endpoints are protected.

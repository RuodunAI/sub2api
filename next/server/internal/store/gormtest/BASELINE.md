# Hot-path database baseline (pgx, before GORM)

Batch 4 compares its GORM versions against these numbers. Rerun the same
command on the same machine before and after; **allocs/op is the number to
compare** (it is stable to the unit), ns/op is dominated by PostgreSQL
round trips and commit fsyncs and varies with load.

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 \
  ./internal/apikey/ ./internal/billing/ ./internal/usage/
```

Recorded 2026-11-15 at 06c2f18f. Machine: 4 vCPU Xeon @ 2.30GHz, local
PostgreSQL 16.15 (fsync and synchronous_commit on), Go 1.27.0. Other test
runs shared the server, so ns/op is noisy. Values are medians of 5.

| Benchmark | Measures | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|
| `apikey` BenchmarkHotKeyLookup | key lookup, 1 statement | 109 535 | 4 544 | 88 |
| `billing` BenchmarkHotPrecharge | precharge tx: 1 batch + event | 737 006 | 8 796 | 277 |
| `usage` BenchmarkHotSettlerInsertFree | free row: insert + release + event | 726 848 | 11 093 | 234 |
| `usage` BenchmarkHotSettlerInsertPending | billable row: insert | 625 724 | 5 524 | 102 |
| `usage` BenchmarkHotSettle | settle tx: lock, ledger batch, update, event | 1 574 325 | 35 046 | 863 |

HotSettle also prices the record with the expression engine; only its
difference before and after the migration means anything.

## Batch 1: the helper history prefix probe

`helperhistory.(*Service).anyPrefix` runs for every gateway request that
carries helper prefixes (`Lookup`, before any lock), most of which it
answers Unknown. It stayed on pgx under `//gormguard:allow-pgx` until batch
5b (the GORM form of batch 1, `Raw("SELECT EXISTS(?)", withPrefixes(...))`,
took 130 allocs/op against 12). It is on GORM now: the same SQL through
`store.G(...).Raw(...).Row().Scan` (`prefixProbe`), measured on a miss for
an owner that holds one chain:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/helperhistory/
```

Recorded 2026-11-15 on gorm-5b, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5. The pgx row is the
statement as it ran before batch 5b, kept in the benchmark for the
comparison; the gorm row is the production code.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotHelperPrefix/pgx (before) | 30 667 | 549 | 12 |
| BenchmarkHotHelperPrefix/gorm (after) | 32 036 | 3 789 | 52 |

4.3 times the allocations (batch 1's form: 10.8). The locked reads of the
same records inside Lookup's transaction (`discoverPrefixes`, only for a
request that hits) were already on GORM.

## Batch 2a: the account reads of the request path

The gateway reaches two account reads, both behind a 30 s cache
(`snapshotTTL`), so a request runs them on a miss only: the group snapshot
(← `Candidates`, the schedulable accounts of the group) and the row of
`Load` (`loadLive`). They stayed on pgx under `//gormguard:allow-pgx` until
batch 5b (batch 2a's GORM forms, a `Table(...).Select(...)` chain scanned
into a struct, took 610 and 206 allocs/op against 250 and 63). They are on
GORM now as Raw SQL scanned into local variables: `readGroupSnap` over
`groupMembersQuery`'s `Rows()`, and `scanRow` over `liveRowQuery`'s
`Row()`. Measured on a group of ten accounts:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/account/
```

Recorded 2026-11-15 on gorm-5b, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5. The pgx row is the
statement as it ran before batch 5b, kept in the benchmark for the
comparison; the gorm row is the production code.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotGroupSnapshot/pgx (before) | 126 994 | 22 247 | 250 |
| BenchmarkHotGroupSnapshot/gorm (after) | 131 832 | 27 192 | 419 |
| BenchmarkHotLoadRow/pgx (before) | 51 376 | 6 435 | 63 |
| BenchmarkHotLoadRow/gorm (after) | 108 560 | 13 994 | 186 |

1.7 and 3.0 times the allocations. The account list's page and total (a
pgx.Batch of two statements until batch 5b) are two GORM statements now,
and the list's usage overview (a pgx.Batch of `usageTotals`, `usageIn` and
`usageLimits`) runs those three GORM reads one after the other: both serve
the console, not the request path.

## Batch 2b: the CCGateway shared-runtime account read

`ccgateway.(*Service).sharedAccountType` runs for every model request
forwarded to the shared runtime (a configuration saved without per-account
runtimes; `attempt.resolveShared`), with no cache in front. With per-account
runtimes the request path reads the configuration, the account state, its
feature policy and request log level through the request cache
(`requestCacheTTL`, dropped on change), so those statements only run on a
miss and are on GORM. sharedAccountType stayed on pgx under
`//gormguard:allow-pgx` until batch 5b (batch 2b's form,
`Model(&model.Account{}).Select("type").Where(...).Take`, took 65 allocs/op
against 8); it is on GORM now as `Raw(...).Row().Scan`
(`sharedAccountTypeQuery`):

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/ccgateway/
```

Recorded 2026-11-15 on gorm-5b, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5. The pgx row is the
statement as it ran before batch 5b, kept in the benchmark for the
comparison; the gorm row is the production code.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotSharedAccountType/pgx (before) | 29 218 | 445 | 8 |
| BenchmarkHotSharedAccountType/gorm (after) | 48 485 | 3 349 | 43 |

## Batch 2c: the plugin grant check

`grants.Holds` runs for every HostService call of a plugin instance that
needs a host permission (`hostServer.require`), so for every plugin-backed
gateway request that calls the host (account credentials, KV, locks,
prices ...). It stayed on pgx under `//gormguard:allow-pgx` until batch 5b
(batch 2c's form, `Raw` over the named-parameter `versionRows`, took 60
allocs/op against 11). It is on GORM now as `Raw(...).Row().Scan`
(`holdsQuery`) with positional parameters, which spare the map and the
named-parameter parsing: the key and version follow VersionRows's
placeholder order (`TestVersionRowsPlaceholderOrder`) and each repeated
argument is boxed once. Measured on a granted permission with a scope:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/plugin/grants/
```

Recorded 2026-11-15 on gorm-5b, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5. The pgx row is the
statement as it ran before batch 5b, kept in the benchmark for the
comparison; the gorm row is the production code. The pgx row binds constant strings, which box
without allocating; batch 2c measured 11 allocs/op for the same statement
called through Holds.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotGrantHolds/pgx (before) | 24 360 | 501 | 8 |
| BenchmarkHotGrantHolds/gorm (after) | 56 611 | 5 045 | 60 |

With the arguments of the pgx row (constants), the named form took 61
allocs/op and the positional one 58; boxing each use of a repeated
argument apart cost 4 more. The other grant statements (instance start, consent, rollout
commit, revocation) were already on GORM.

The provider resource statements (`internal/providerresources`) run only
for requests that carry or return provider files, containers or skills,
each inside a transaction that already takes advisory locks; they are on
GORM without a benchmark.

## Batch 4a: the plugin execution records

A request served through an Execute plugin (`gateway` `canExecute`) writes
its plugin_executions row on every attempt (`usage.BeginExecution`, the
intent, and `ObserveExecution`, the observation) and commits it inside the
usage transaction (`CommitExecution`: `lockExecution`, the row locked FOR
UPDATE, and `markExecutionCommitted`).

**Migrated to GORM in batch 5a.** The four statements are `store.G`
`Exec` / `Raw(...).Row().Scan` with the SQL they had on pgx (placeholders
`?`, the documents bound as `store.Bytes`), the form that allocates least;
the `//gormguard:allow-pgx` comments are gone. The benchmark measures the
production function (gorm) next to the pgx statement it replaced (pgx):

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench HotExecution -benchmem -benchtime 1000x -count 5 ./internal/usage/
```

Recorded 2026-11-15 on gorm-5a, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotExecution/begin/pgx (before) | 328 471 | 5 525 | 19 |
| BenchmarkHotExecution/begin/gorm (after) | 349 961 | 8 556 | 54 |
| BenchmarkHotExecution/observe/pgx (before) | 369 706 | 5 532 | 20 |
| BenchmarkHotExecution/observe/gorm (after) | 347 069 | 8 721 | 55 |
| BenchmarkHotExecution/commit/pgx (before) | 476 277 | 1 114 | 21 |
| BenchmarkHotExecution/commit/gorm (after) | 1 631 682 | 7 495 | 104 |

The gorm-4a GORM forms (`Create` of the model, `Updates` with a map) took
121, 86 and 161 allocs/op; Raw with the pgx SQL takes 54, 55 and 104.

## Batch 4b: the settler's statements

Per billable request the settler runs the `usage_logs` insert
(`insertAtomic`), `settleTx`'s row lock (`lockPendingRow`: `SELECT
billing_status FROM usage_logs WHERE request_id = ? FOR UPDATE [SKIP
LOCKED]`), the ledger statements (package billing, Batch 4c) and its write
of the billing fields (`billUsageRow`). A request a plugin reserves runs
`reserveTx` instead: the write of the estimate (`reserveUsageRow`) and the
`pending_settlements` insert (`insertSettlementEntry`, `ON CONFLICT
(plugin_key, ref_id) DO NOTHING RETURNING id`).

**Migrated to GORM in batch 5a.** The row lock and the reconcile entry
(allow-pgx until then) are `Raw(...).Row().Scan`. The `usage_logs` insert
was a pgx.Batch of one `INSERT ... ON CONFLICT (request_id) DO NOTHING
RETURNING id` per record; it is now one multi-row INSERT per power-of-two
chunk of the batch (`insertUsageSQL[k]`, 2^k rows, k ≤ 7; RETURNING `id,
request_id`), in batch order, inside the same transaction. A request id
already stored, or taken by an earlier record of the batch (in the same
statement or an earlier chunk), is skipped as before and the first record
of an id keeps its row (TestInsertBatchChunksAndDuplicates). The chunk
sizes bound the statement texts a connection prepares to eight; a batch
of n records takes one round trip per bit set in n (200: 128 + 64 + 8)
where the pgx batch took one. Since gorm-5rtt every batch is one statement
again (see "Round trips after batch 5a" below).

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench 'HotStmt|HotSettle' -benchmem -benchtime 1000x -count 5 ./internal/usage/
go test -run '^$' -bench HotSettlerInsertBatch -benchmem -benchtime 100x -count 5 ./internal/usage/
```

Recorded 2026-11-15 on gorm-5a (before: f99d1a3a, same machine, same
day). Medians of 5. The statement benchmarks run the production function
(gorm) next to the pgx statement it had (pgx):

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotStmtSettleLock/pgx (before) | 217 138 | 461 | 9 |
| BenchmarkHotStmtSettleLock/gorm (after) | 236 966 | 3 493 | 45 |
| BenchmarkHotStmtSettleUpdate/pgx | 382 972 | 4 600 | 129 |
| BenchmarkHotStmtSettleUpdate/gorm (on GORM since 4b) | 447 916 | 8 288 | 169 |
| BenchmarkHotStmtReserveUpdate/pgx | 357 775 | 3 925 | 98 |
| BenchmarkHotStmtReserveUpdate/gorm (on GORM since 4b) | 338 052 | 6 862 | 132 |
| BenchmarkHotStmtReserveEntry/pgx (before) | 228 760 | 645 | 14 |
| BenchmarkHotStmtReserveEntry/gorm (after) | 252 477 | 4 428 | 60 |

The whole chain, at f99d1a3a (before) and on gorm-5a (after); the ledger
statements of BenchmarkHotSettle moved too (Batch 4c):

| Benchmark | before ns/op | before B/op | before allocs/op | after ns/op | after B/op | after allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| BenchmarkHotSettlerInsertFree | 968 400 | 11 088 | 233 | 1 107 144 | 30 228 | 369 |
| BenchmarkHotSettlerInsertPending | 497 070 | 5 525 | 102 | 477 084 | 17 411 | 192 |
| BenchmarkHotSettlerInsertBatch (200 rows) | 22 279 814 | 2 315 887 | 29 001 | 24 890 346 | 5 483 727 | 51 014 |
| BenchmarkHotSettle | 1 598 464 | 38 783 | 903 | 1 606 078 | 57 296 | 1 046 |

### Async tasks

The task registry is on the request path, not behind it: the gateway's
task endpoints call it synchronously. A submission runs `BeginTask` (the
receipt insert) before it goes upstream, `ReceiveTask` (the upstream
answer onto the receipt; gateway `tasks.go`, and `execute.go` for an
Execute plugin) when it comes back, and `RegisterTask` (or
`CommitExecution` with a task) before the client gets its public ID:
`recordTask`, in the usage transaction, checks the receipt (`SELECT ...
FOR UPDATE`), inserts the `async_tasks` row, links the settlement entry
(`UPDATE pending_settlements`) and closes the receipt (`UPDATE
task_submission_receipts`). Every poll of a client runs `FindTask`
(`readTask`), the most frequent of them.

**Migrated to GORM in batch 5a**, as `Raw` / `Exec` with the SQL they had
on pgx (`readTask`'s `taskByID`, `$5` bound three times; the receipt body
and the snapshot as `store.Bytes`). The benchmark measures each production
function (gorm) next to the pgx statements it replaced (pgx), `recordTask`
as its four statements in a transaction of their own:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench HotTask -benchmem -benchtime 1000x -count 5 ./internal/usage/
```

Recorded 2026-11-15 on gorm-5a, same machine. Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotTaskFind/pgx (before) | 71 007 | 1 263 | 19 |
| BenchmarkHotTaskFind/gorm (after) | 96 019 | 6 448 | 92 |
| BenchmarkHotTaskBegin/pgx (before) | 294 587 | 394 | 10 |
| BenchmarkHotTaskBegin/gorm (after) | 423 335 | 4 090 | 49 |
| BenchmarkHotTaskReceive/pgx (before) | 220 572 | 252 | 5 |
| BenchmarkHotTaskReceive/gorm (after) | 249 450 | 3 429 | 40 |
| BenchmarkHotTaskRegister/pgx (before) | 664 958 | 1 705 | 47 |
| BenchmarkHotTaskRegister/gorm (after) | 891 045 | 12 230 | 176 |

The receipt insert takes 49 allocs/op as `Exec` where the gorm-4b
`Model(...).Create(map)` took 129. The lazy import of a legacy task
(`importLegacyTask`, once per legacy upstream ID that a poll names) and
the receipt cleanup were on GORM before.

The rest of the settler (`markFailed`, `RetryPending`,
`closeUnreconcilable`, the reconcile loop, the task reconciler and the
hourly rollup) runs off the request path or on failures only and is on
GORM without a benchmark. The rollup's `INSERT INTO usage_hourly ...
SELECT` keeps its `$n` placeholders and runs through `execNumbered`,
because its measures hold the jsonb `?` operator (`anomalousRow`), which
GORM takes for a placeholder.

## Batch 4c: billing

The ledger writes of the request path: `prechargeTx` (← `Precharge`, every
billable request), `SettleTx` (settler, every billable request),
`ReleasePrechargeTx` (settler, every free request it records, most of
which hold no reservation; and the expiry sweeper) and `ApplyTx`.

**Migrated to GORM in batch 5a.** They were pgx batches (and
`ReleasePrechargeTx` a pgx statement under allow-pgx); every statement now
runs through `store.G` on the caller's transaction, in the order the batch
sent them, so the lock order is unchanged: precharge `SET LOCAL
lock_timeout`, `ensureBalanceSQL`, `SELECT user_id FROM user_balances ...
FOR UPDATE`, `prechargeSQL`; settlement `ensureBalanceSQL`, `releaseSQL`,
`applySQL`; ApplyTx `ensureBalanceSQL`, `applySQL`. The CTE statements
keep their SQL and their `$n` numbering; `ledgerStmt` rewrites each `$n`
into a `?` once at start-up and binds the argument again wherever the
number repeats (snapshots: TestSQLLedger). Transaction boundaries, FOR
UPDATE, idempotency keys, ON CONFLICT, RETURNING and the
numeric / `decimal.Decimal` amounts are unchanged; the console's price
rule writes (`applyChanges`, a batch before) run one statement each in
the same order. New real-PostgreSQL tests cover concurrent replays of a
settlement, of an idempotency key and of a precharge
(ledger_concurrency_test.go, usage TestSettleConcurrentSameRow).

What a batch saved is now a round trip per statement: Precharge is 7
flushes (BEGIN, lock_timeout, balance row, lock, reservation, event,
COMMIT) where it was 4, a settlement's ledger part 3 where it was 1, ApplyTx
2 where it was 1 (TestPrechargeRoundTrips, TestSettleRoundTrips). Under the
user's balance lock a settlement now waits for the charge statement after
the release; a precharge's lock is held from its lock statement through
prechargeSQL, the event and COMMIT, as before. gorm-5rtt brought all three
back to the pgx batch's count (see "Round trips after batch 5a" below).

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/billing/
```

Recorded 2026-11-15 on gorm-5a (before: f99d1a3a, same machine, same day;
the server was shared with other test runs, so ns/op is noisy). Medians of
5. BenchmarkHotReleasePrecharge runs the production statement (gorm,
`releaseStmt`) next to the pgx statement it replaced (pgx):

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotReleasePrecharge/miss/pgx (before) | 80 832 | 1 213 | 30 |
| BenchmarkHotReleasePrecharge/miss/gorm (after) | 106 727 | 8 437 | 76 |
| BenchmarkHotReleasePrecharge/hit/pgx (before) | 276 307 | 1 343 | 37 |
| BenchmarkHotReleasePrecharge/hit/gorm (after) | 451 656 | 8 535 | 81 |
| BenchmarkHotPrecharge (f99d1a3a, before) | 790 831 | 8 684 | 277 |
| BenchmarkHotPrecharge (gorm-5a, after) | 881 849 | 24 017 | 502 |

On GORM since batch 4c, because they do not run for every request:

- `findLedger` (the ledger row of an idempotency key): only after an
  insert lost a race on the key, or for a duplicate settlement refund;
- `Balance` (← `CheckBalance`): behind the Redis balance cache, a miss only;
- `loadSettings` (← `Settings`) and the price snapshot (`snapshot`:
  `model_prices` and `loadRules`, ← `Resolve`): cached for `cacheTTL`
  (1 min) and dropped on change, a miss only;
- the expiry sweep's select, the console (prices, group and segment price
  rules, price sources and their sync, ledger lists, `/me/balance`,
  `/me/prices`, `/key/prices`, quotes).

## Round trips after batch 5a (gorm-5rtt)

Batch 5a ran each statement of a pgx batch on its own, so every one became
a round trip; at a remote PostgreSQL (milliseconds each) that is request
latency, and in the ledger it is time the user's balance row stays locked.
gorm-5rtt merges them back, with the same SQL semantics (lock order, FOR
UPDATE, idempotency keys, ON CONFLICT, RETURNING, numeric amounts, errors,
lock timeout); the reasoning is in the comments of `settleSQL`,
`prechargeSQL`, `resolvePrecharge` and `insertUsageSQL`:

- Billing no longer runs `ensureBalanceSQL` before every change. The ledger
  statements report whether they found the balance row; only when they did
  not (they then wrote nothing and hold no row lock) does the caller create
  it and run the statement again, the old sequence. For an existing row
  that INSERT was a no-op.
- Precharge is one statement: `set_config('lock_timeout', '3s', true)` as
  the lock CTE's condition (evaluated on the row before it is locked; it
  bounds every row wait as SET LOCAL did, but not a wait for a table lock,
  which only DDL causes), the FOR UPDATE lock, the reservation and its
  ledger row. The idempotency key, which the old statement read after the
  lock, is read in a snapshot taken before it; the statement reports
  whether the locked row is still the snapshot's version (ctid), and when
  it is not, the cases without an insert re-read the key
  (`resolvePrecharge`), so a replay that waited behind its first precharge
  still answers success (TestPrechargeReplayAfterLockWait).
- A settlement's release and charge are one statement (`settleSQL`): one
  FOR UPDATE, the refund row before the charge row, one balance UPDATE to
  the final value.
- The `usage_logs` insert of a batch is one statement of 47 array
  parameters (`unnest ... WITH ORDINALITY ... ORDER BY ord`, one text for
  every size); a batch of one binds its values directly
  (`insertUsageRowSQL`), which allocates less. Both store what the pgx
  VALUES insert stored, column by column (TestInsertBatchMatchesValuesInsert).

Round trips (flushes; TestPrechargeRoundTrips, TestSettleRoundTrips,
TestInsertBatchOneStatement):

| Path | pgx batch | batch 5a | gorm-5rtt | under the balance lock (gorm-5rtt) |
|---|---:|---:|---:|---|
| Precharge (BEGIN … COMMIT) | 4 | 7 | 4 | statement tail, event, COMMIT |
| SettleTx ledger statements | 1 | 3 | 1 | statement tail (then event, caller's statements, COMMIT) |
| ApplyTx statements | 1 | 2 | 1 | statement tail, event, COMMIT |
| usage insert of n records | 1 | popcount-chunks (200: 3) | 1 | — |

A user's first balance change (no balance row yet) takes two more: the
row's insert and the statement again.

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench 'HotPrecharge|HotReleasePrecharge' -benchmem -benchtime 1000x -count 5 ./internal/billing/
go test -run '^$' -bench 'HotSettle$|HotSettlerInsertFree|HotSettlerInsertPending' -benchmem -benchtime 1000x -count 5 ./internal/usage/
go test -run '^$' -bench HotSettlerInsertBatch -benchmem -benchtime 100x -count 5 ./internal/usage/
```

Recorded 2026-11-15, the three trees one after the other on the same
machine and local server (pgx: 25b7ba75, the last commit before batch 5a,
with batch 5a's usage benchmark file for the 200-row batch; batch 5a:
493c046d; gorm-5rtt). Medians of 5; the server is local, so a round trip
costs tens of microseconds here, not the milliseconds of a remote one, and
ns/op is noisy (shared server). ReleasePrecharge's statement did not change
(its gorm row is the production code in all three).

| Benchmark | pgx ns/op | pgx B/op | pgx allocs | 5a ns/op | 5a B/op | 5a allocs | 5rtt ns/op | 5rtt B/op | 5rtt allocs |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `billing` BenchmarkHotPrecharge | 836 038 | 10 830 | 291 | 903 226 | 26 018 | 516 | 766 509 | 23 374 | 451 |
| `billing` BenchmarkHotReleasePrecharge/miss/gorm | 118 301 | 8 437 | 76 | 109 050 | 8 437 | 76 | 95 160 | 8 437 | 76 |
| `billing` BenchmarkHotReleasePrecharge/hit/gorm | 363 943 | 8 535 | 81 | 281 862 | 8 535 | 81 | 367 379 | 8 535 | 81 |
| `usage` BenchmarkHotSettle | 1 524 456 | 43 219 | 928 | 1 668 416 | 61 598 | 1 071 | 1 470 025 | 60 635 | 1 013 |
| `usage` BenchmarkHotSettlerInsertFree | 1 024 114 | 13 135 | 248 | 835 875 | 32 268 | 383 | 840 445 | 32 257 | 381 |
| `usage` BenchmarkHotSettlerInsertPending | 486 729 | 5 525 | 102 | 497 067 | 17 410 | 192 | 472 504 | 17 420 | 192 |
| `usage` BenchmarkHotSettlerInsertBatch (200 rows) | 23 155 843 | 2 387 014 | 28 350 | 25 415 379 | 5 554 823 | 50 363 | 20 420 555 | 3 492 263 | 37 703 |

The 47 one-element arrays of a batch of one cost 467 allocs/op and
37 440 B/op in BenchmarkHotSettlerInsertPending (one round trip either
way), hence `insertUsageRowSQL` for that case.

## Batch 4d: the gateway settings and sticky rules, the account snapshots

The gateway package runs no statement of its own for each request (forward,
pipeline and hooks run none at all). The request path reaches its settings
rows (`loadSettingsRow`, `loadFailurePolicy`) through the settings cache
(`settingsTTL`, 10 s) and the sticky rules (`listRules`) through the rules
cache (`rulesTTL`, 30 s), so a request runs them on a miss only.

In package store, `ApplyAccountQuotaObservation` runs for every upstream
answer that carries a quota observation (gateway `applyQuotaObservation`),
with no cache in front, and `ReadAccountQuotas` follows each one that
applied (the quota mirror reloads the account). `AccountBalances` was
listed as hot, but only the console's account list reads it, behind Redis
(`cachedAccountBalances`); it is measured for the record.

The settings read (`settingValue`), `listRules`,
`ApplyAccountQuotaObservation` and `ReadAccountQuotas` stayed on pgx under
`//gormguard:allow-pgx` until batch 5b (batch 4d measured their GORM forms,
a `model.Setting` `Take`, a `model.StickyRule` `Find` plus the conversion,
and the same SQL through `store.G`, at 69, 210, 47 and 104 allocs/op). They
are on GORM now as Raw SQL scanned into local variables (`settingValueQuery`
and `rulesQuery` with `scanRule`; `quotasAt` with `readQuotaRows`) and
`Exec` (`applyQuotaObservation`); package store cannot use the models:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/gateway/ ./internal/store/
```

Recorded 2026-11-15 on gorm-5b, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5. The pgx row is the
statement as it ran before batch 5b, kept in the benchmark for the
comparison; the gorm row is the production code. AccountBalances was on GORM before batch 5b; its
pgx row is the statement it had before batch 4d.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `gateway` BenchmarkHotSettingsRow/pgx (before) | 33 006 | 893 | 9 |
| `gateway` BenchmarkHotSettingsRow/gorm (after) | 42 181 | 4 181 | 46 |
| `gateway` BenchmarkHotStickyRules/pgx (before) | 56 996 | 3 224 | 51 |
| `gateway` BenchmarkHotStickyRules/gorm (after) | 68 348 | 8 054 | 141 |
| `store` BenchmarkHotQuotaObservation/pgx (before) | 392 403 | 418 | 9 |
| `store` BenchmarkHotQuotaObservation/gorm (after) | 544 045 | 5 197 | 50 |
| `store` BenchmarkHotReadAccountQuotas/pgx (before) | 44 118 | 2 861 | 46 |
| `store` BenchmarkHotReadAccountQuotas/gorm (after) | 56 928 | 7 604 | 105 |
| `store` BenchmarkHotAccountBalances/pgx | 33 832 | 997 | 19 |
| `store` BenchmarkHotAccountBalances/gorm | 50 366 | 4 901 | 70 |

The other statements of both packages were already on GORM: the sticky
rule console writes, the start-up sync of the built-in rules and the
failure policy lock in the gateway; the other balance, quota, credential
refresh and settings statements of package store, which the console, the
refresh jobs and the debounced passive quota writer (`SaveAccountQuota`, at
most every 30 s per account) run.

## Batch 5b: the API key lookup and the remaining batches

`apikey.(*Service).lookup` (`lookupSQL`) is the first statement of every
gateway request and is not cached. It stayed on pgx under
`//gormguard:allow-pgx` until batch 5b; it is on GORM now as
`Raw(...).Row().Scan` into local variables (`lookupQuery`; the jsonb
columns through `store.JSON`, the IP list through `store.Strings`):

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/apikey/
```

Recorded 2026-11-15 on gorm-5b, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5. The pgx row is the
statement as it ran before batch 5b, kept in the benchmark for the
comparison; the gorm row is the production code.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotKeyLookup/pgx (before) | 100 239 | 4 543 | 88 |
| BenchmarkHotKeyLookup/gorm (after) | 128 383 | 11 162 | 150 |

1.7 times the allocations. Most of the difference is the GORM bridge
(store's `bridgeRows`): one decoded value per column handed to
database/sql (a string for each text and numeric column, a boxed int64 or
time), and GORM's statement (clone, SQL builder, Row's database/sql
connection). Those are store-wide costs, not the lookup's.

Batch 5b also replaced the pgx.Batch and CopyFrom writes outside billing
and usage, statement for statement and in the same transaction:

- `event.Publisher.Emit`: one multi-row `INSERT INTO events` in the
  caller's transaction (ids in event order), one round trip as before;
- `plugin/egress` `insertOpen` (a batch of `INSERT ... RETURNING id`) and
  `insertDone` (CopyFrom): multi-row `Create` of `model.PluginEgressLog`
  with `RETURNING id`, at most 5461 rows (65535 / 12 parameters) per
  statement, several statements in one transaction; a failed write returns
  no ids, as the batch's implicit transaction had written none;
- `webui` `Publish`: the two writes of each file, in file order, in the
  publishing transaction;
- `account` list page and usage overview (above, batch 2a).

The multi-row event insert and the round trips the last two items and
`billing` `applyChanges` had gained were reworked by gorm-5fix (below).

## gorm-5fix: the event insert's text, the console's round trips

`event.Publisher.Emit` runs in the settler's transaction, under the user's
balance lock when it carries a ledger change (`usage` `insertAtomic`).
Batch 5b's multi-row `VALUES` insert had a text per event count: pgx's
statement cache prepares each new text, a Prepare round trip under that
lock, and two parameters per event capped a call at 32767 events. Several
events are now `insertEventsSQL`, two array parameters (`unnest ... WITH
ORDINALITY ... ORDER BY`, ids in event order) and one text for any count;
one event keeps the one-row `VALUES` insert (`insertEventSQL`, the batch 5b
text and arguments), as `insertUsageRowSQL` does for a batch of one.
TestEmitManyAsOne checks that both store the same documents,
TestEmitPastParameterLimit 40 000 events in one call,
TestEmitPreparesTwoTexts the prepares:

| Emit, counts 1 to 12 on one connection | batch 5b | gorm-5fix |
|---|---:|---:|
| `INSERT INTO events` texts prepared | 12 | 2 |

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench HotEmit -benchmem -benchtime 2000x -count 5 ./internal/event/
```

Recorded 2026-11-15, d6ad15ef (before) and gorm-5fix (after), same
machine and local server, the after run beside other test runs: ns/op is
noise (the one-event path is the same code in both). Medians of 5; one
transaction holds every iteration, so the statement is prepared once.

| BenchmarkHotEmit | before ns/op | before B/op | before allocs | after ns/op | after B/op | after allocs |
|---|---:|---:|---:|---:|---:|---:|
| 1 event | 32 645 | 2 794 | 45 | 65 967 | 2 794 | 45 |
| 2 events | 43 326 | 3 403 | 66 | 80 799 | 3 387 | 70 |
| 10 events | 122 351 | 10 177 | 219 | 153 176 | 7 357 | 193 |

The ledger's hot paths after gorm-5fix (its ledger statements are built
from shared fragments, the same SQL; a settlement emits two events):
BenchmarkHotPrecharge 451 allocs/op, BenchmarkHotReleasePrecharge/gorm 76
(miss) and 81 (hit), BenchmarkHotSettle 1 019 (gorm-5rtt 1 013),
BenchmarkHotSettlerInsertFree 381, BenchmarkHotSettlerInsertPending 192.

The console statements batch 5a and 5b had split are one statement again,
with the same rows, locks and results (snapshots TestSQLAccountList,
TestSQLAccountUsageOverview, TestSQLWriteRules, TestSQLAssetPut):

| Path | pgx batch | before (d6ad15ef) | gorm-5fix |
|---|---:|---:|---:|
| `account` `listPage` | 1 | 2 | 1 (2 for a page past the end, which has no row to carry the total) |
| `account` `usageOverview` (the list's usage cell) | 1 | 3 | 1 |
| `billing` `applyChanges` (PUT group model prices) | 1 | one per change, one more per custom price | 1 |
| `webui` `Publish`, file writes | 1 | 2 per file | 1 |

- `listPage`: the page carries the total as a scalar subquery (`listRows`),
  run once, in the page's snapshot.
- `usageOverview`: `usageOverviewSQL` reads each account's totals and limit
  (LEFT JOIN, with whether there is one) and its window from midnight
  (LATERAL sums), one row per account in list order;
  TestUsageOverviewMatchesItsReads compares it with the three reads.
- `applyChanges`: `writeRulesSQL` takes the changes as arrays, one row per
  rule (several changes of one rule collapsed into removed and saved), and
  runs history, ordered `FOR UPDATE` of the existing rules, deletes, the
  group's upserts and the segments' upserts one after the other (each step
  reads the previous one in a one-time condition). TestWriteRulesMatchesSequential
  compares it with the statements one by one, TestWriteRulesConcurrentNoDeadlock
  fails without the ordered lock step.
- `Publish`: `putAssetsSQL` writes every blob and lists its path in the
  build in one statement of arrays, in file order; a file whose path it
  did not return is the immutable collision that fails the publication.

## Batch 3a: console authentication (iam)

From batch 3 on every statement moves to GORM, hot or not; the numbers are
recorded, no longer a reason to stay on pgx. The console's request path
reads three statements of package iam:

- `isActiveWithVersion` (← `VerifySession`, every request with an access
  token): the user's status and token_version, behind the per-node status
  cache (`statusCacheTTL`, 30 s, dropped on change), so on a miss only;
- `VerifyAccessKey` (every request with an access key, no cache): the key
  by its hash (`accessKeyByHash`), and at most once a minute per key the
  `last_used_at` write (`touchAccessKey`);
- `getUser` (GET /me, and every login and refresh in `issueTokens`): the
  user with roles, segment and balance (`userByID`).

Each is `Raw(...).Row().Scan` into local variables, the form with the
fewest allocations, with the SQL text of the pgx statement. The benchmark
measures the pgx statement next to that GORM form, for a user with one
role and one access key:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/iam/
```

Recorded 2026-11-15 on gorm-3a, same machine and server as above (shared
with other test runs, so ns/op is noisy). Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotUserStatus/pgx (before) | 57 350 | 469 | 6 |
| BenchmarkHotUserStatus/gorm (after) | 63 208 | 3 525 | 43 |
| BenchmarkHotAccessKey/pgx (before) | 54 281 | 981 | 24 |
| BenchmarkHotAccessKey/gorm (after) | 57 158 | 4 221 | 66 |
| BenchmarkHotGetUser/pgx (before) | 55 633 | 1 517 | 27 |
| BenchmarkHotGetUser/gorm (after) | 53 565 | 7 173 | 94 |

GORM takes 7.2, 2.8 and 3.5 times the allocations, about 3 to 6 KB more
per statement; the round trip dominates the time either way. The other
iam statements (logins, refresh rotation, sessions, two-factor, passkeys,
step-up, users, access key management) run once per such action, not per
request, and are on GORM without a benchmark.

## Batch 3b: the authorization reads

Every authorization check (`Can`, `PermissionSet`) answers from the
node's cache while the user's set and the catalog are at the current
authz version; any authz change drops them, and the next request of each
user reloads them: `loadPermissionSet` (a read-only repeatable-read
transaction: `SELECT version FROM authz_meta` and one row per grant of each
of the user's roles) and `ensureCatalog` (the same transaction around
`SELECT key, status FROM permissions`). `currentVersion` is the version
poll's and `committedLater`'s read. From batch 3 the statements move to
GORM whatever the numbers (`Raw(...).Rows()` read by position, the
version with `Row().Scan`), so the table records the cost only. Before
is bddebe3a (pgx) with the benchmark added, after is gorm-3b:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/authz/
```

Recorded 2026-11-15, same machine; the server was shared with other test
runs, so ns/op is noisy. Medians of 5. The user holds the built-in admin
role; the catalog is the core one.

| Benchmark | before ns/op | B/op | allocs/op | after ns/op | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| BenchmarkHotAuthz/permission_set | 320 354 | 9 081 | 283 | 362 696 | 13 640 | 201 |
| BenchmarkHotAuthz/catalog | 154 157 | 15 362 | 312 | 256 559 | 21 027 | 435 |
| BenchmarkHotAuthz/version | 16 968 | 413 | 6 | 42 156 | 3 213 | 38 |

The permission set allocates less than on pgx: pgx read the nullable
grant key into a `*string`, one allocation per row, where the GORM form
reads it into a `sql.NullString`. The catalog (1.4 times) and the version
(6.3 times, about 30 allocations of fixed cost per GORM statement) cost
more.

## Batch 6: GORM-owned connections

Batch 6 removed the forwarding driver: GORM (gorm.io/driver/postgres, pgx's
database/sql driver underneath) owns the pool and the transactions, and
store's array types are written and read as PostgreSQL's array text
instead of through pgtype. Every hot-path benchmark ran on the base of the
batch (72d1ab3f, GORM over the forwarding driver) and on the batch,
package by package, base first:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -skip HotSettlerInsertBatch -benchmem -benchtime 1000x -count 5 \
  ./internal/apikey/ ./internal/billing/ ./internal/usage/ ./internal/account/ \
  ./internal/helperhistory/ ./internal/iam/ ./internal/store/ ./internal/gateway/ \
  ./internal/ccgateway/ ./internal/plugin/grants/ ./internal/authz/
# and HotSettlerInsertBatch with -benchtime 100x
```

Recorded 2026-11-15, same machine and server as above. Medians of 5.
allocs/op is stable to the unit; ns/op is not: the "pgx"/"sql" variants,
the same SQL on pgx before and on database/sql after, moved by -54% to
+194% between the two runs, and a second pass over the read paths
(interleaved, 6 runs each, at two to three times the absolute times)
put the GORM forms of the single-row reads within 20% of their base. The
table holds the production (GORM) forms; the raw variants stay in the
benchmarks for comparison.

| Benchmark | base ns/op | B/op | allocs/op | batch 6 ns/op | B/op | allocs/op | allocs |
|---|---:|---:|---:|---:|---:|---:|---:|
| `apikey` HotKeyLookup/gorm | 69 068 | 11 162 | 150 | 79 898 | 9 812 | 141 | -6% |
| `billing` HotPrecharge | 960 183 | 23 255 | 451 | 1 427 114 | 25 552 | 468 | +4% |
| `billing` HotReleasePrecharge/miss/gorm | 113 239 | 8 437 | 76 | 96 275 | 7 808 | 70 | -8% |
| `billing` HotReleasePrecharge/hit/gorm | 508 232 | 8 536 | 81 | 976 806 | 7 888 | 75 | -7% |
| `usage` HotExecution/begin/gorm | 424 495 | 8 563 | 54 | 713 657 | 8 079 | 49 | -9% |
| `usage` HotExecution/observe/gorm | 648 947 | 8 724 | 55 | 566 341 | 8 228 | 50 | -9% |
| `usage` HotExecution/commit/gorm | 647 935 | 7 493 | 104 | 686 440 | 9 692 | 121 | +16% |
| `usage` HotSettlerInsertFree | 1 585 039 | 32 272 | 381 | 1 582 580 | 34 216 | 396 | +4% |
| `usage` HotSettlerInsertPending | 986 123 | 17 417 | 192 | 1 496 907 | 20 213 | 214 | +11% |
| `usage` HotSettle | 1 882 817 | 60 705 | 1 013 | 1 641 709 | 61 414 | 1 020 | +1% |
| `usage` HotStmtSettleLock/gorm | 351 718 | 3 493 | 45 | 266 536 | 2 960 | 39 | -13% |
| `usage` HotStmtSettleUpdate/gorm | 334 208 | 8 288 | 169 | 412 487 | 7 814 | 164 | -3% |
| `usage` HotStmtReserveUpdate/gorm | 393 025 | 6 862 | 132 | 423 456 | 6 390 | 127 | -4% |
| `usage` HotStmtReserveEntry/gorm | 275 849 | 4 428 | 60 | 309 603 | 3 896 | 54 | -10% |
| `usage` HotTaskFind/gorm | 76 382 | 6 458 | 92 | 48 861 | 5 658 | 86 | -7% |
| `usage` HotTaskBegin/gorm | 226 171 | 4 090 | 49 | 303 276 | 3 616 | 44 | -10% |
| `usage` HotTaskReceive/gorm | 267 247 | 3 429 | 40 | 304 976 | 2 938 | 35 | -12% |
| `usage` HotTaskRegister/gorm | 836 999 | 12 229 | 176 | 1 837 374 | 13 852 | 188 | +7% |
| `usage` HotSettlerInsertBatch | 21 356 895 | 3 492 361 | 37 704 | 25 216 213 | 4 967 922 | 55 220 | +46% |
| `account` HotGroupSnapshot/gorm | 156 987 | 27 197 | 419 | 182 352 | 26 096 | 399 | -5% |
| `account` HotLoadRow/gorm | 86 034 | 13 988 | 186 | 76 944 | 11 819 | 172 | -8% |
| `helperhistory` HotHelperPrefix/gorm | 38 550 | 3 789 | 52 | 59 929 | 5 966 | 90 | +73% |
| `iam` HotUserStatus/gorm | 21 410 | 3 525 | 43 | 22 206 | 2 960 | 37 | -14% |
| `iam` HotAccessKey/gorm | 38 796 | 4 221 | 66 | 43 580 | 3 456 | 55 | -17% |
| `iam` HotGetUser/gorm | 60 613 | 7 173 | 94 | 89 640 | 6 128 | 83 | -12% |
| `store` HotQuotaObservation/gorm | 652 542 | 5 197 | 50 | 789 608 | 4 723 | 45 | -10% |
| `store` HotReadAccountQuotas/gorm | 59 106 | 7 615 | 105 | 123 010 | 7 467 | 130 | +24% |
| `store` HotAccountBalances/gorm | 47 664 | 4 901 | 70 | 72 745 | 4 939 | 95 | +36% |
| `gateway` HotSettingsRow/gorm | 19 315 | 4 181 | 46 | 28 333 | 3 649 | 40 | -13% |
| `gateway` HotStickyRules/gorm | 46 777 | 8 054 | 141 | 62 568 | 7 131 | 130 | -8% |
| `ccgateway` HotSharedAccountType/gorm | 46 907 | 3 349 | 43 | 19 783 | 2 816 | 37 | -14% |
| `plugin/grants` HotGrantHolds/gorm | 45 270 | 5 045 | 60 | 59 058 | 4 512 | 54 | -10% |
| `authz` HotAuthz/permission_set | 257 189 | 13 640 | 201 | 350 702 | 16 166 | 221 | +10% |
| `authz` HotAuthz/catalog | 184 519 | 21 027 | 435 | 260 602 | 23 561 | 455 | +5% |
| `authz` HotAuthz/version | 51 359 | 3 213 | 38 | 48 263 | 2 680 | 32 | -16% |

Without the forwarding driver a statement costs 5 to 17% fewer
allocations: the bridge's per-column decoding into driver values and its
rows wrapper are gone. What costs more is an array:

- An array parameter (store.Int64s, Strings, Times, Bools) is now its
  text form ("{1,2}"), which pgx parses again into the parameter's binary
  form. The array-parameter reads went up: HotHelperPrefix (`= ANY($3)`
  with a text[]) +73% allocations, HotReadAccountQuotas +24%,
  HotAccountBalances +36%, and the settler's batch insert (`unnest` of 47
  array parameters) +46% allocations and +42% bytes. In the interleaved
  pass HotHelperPrefix was 28% and HotReadAccountQuotas 34% slower than
  base, HotGroupSnapshot (a text[] column read through store.Strings)
  27%. These are the paths above 20%.
- The write transactions (HotPrecharge, HotSettlerInsertPending,
  HotTaskRegister, HotExecution/begin) also show +49% to +120% ns/op in
  the table, with allocations within +11%; their raw variants moved the
  same way between the runs (HotTaskRegister/sql +157%), which puts it on
  the server's commit fsyncs rather than on the code.

A Value that handed pgx the plain slice instead of its text would remove
the array cost (pgx encodes a []int64 natively), at the price of a Value
that is not a database/sql driver.Value; batch 6 keeps the text form.

## What every request runs

Read from the code at 06c2f18f. The gateway package itself runs no
statement per request; it reaches the database through the services below.

| When | Function | SQL |
|---|---|---|
| every request, synchronous | `apikey.(*Service).lookup` ← `Authenticate` ← gateway `call.run` | `lookupSQL`: `SELECT k.id, … FROM api_keys k JOIN users u … JOIN segments s … LEFT JOIN groups g … LEFT JOIN segment_groups sg … WHERE k.key_hash = $1 AND k.deleted_at IS NULL` (not cached, on purpose) |
| billable request, synchronous | `billing.(*Service).Precharge` → `prechargeTx` ← gateway `call.precharge` | BEGIN; `prechargeSQL` (CTE: `set_config('lock_timeout', '3s', true)` and the `user_balances` row FOR UPDATE, insert `balance_ledger` ON CONFLICT DO NOTHING RETURNING, insert `request_precharges`, update `user_balances`; a pgx batch with SET LOCAL, the balance row insert and the lock as statements of their own until batch 5a, one statement each in batch 5a); a user's first precharge also `prechargeEnsureSQL` and `prechargeSQL` again; `INSERT INTO events`; COMMIT |
| billable request, Redis miss only | `billing.(*Service).Balance` ← `CheckBalance` | `SELECT COALESCE((SELECT balance FROM user_balances WHERE user_id = $1), 0), COALESCE((SELECT max(id) FROM balance_ledger WHERE user_id = $1), 0)` |
| every request, settler (async) | `usage.(*Service).insertAtomic` ← `insert` ← `process` / `Submit` | BEGIN; `INSERT INTO usage_logs (47 columns) SELECT … FROM unnest($1::varchar[], …) WITH ORDINALITY … ORDER BY ord ON CONFLICT (request_id) DO NOTHING RETURNING id, request_id`, one statement for the batch (`VALUES (…)` for a batch of one; a pgx batch of one INSERT per row until batch 5a, one INSERT per power-of-two chunk in batch 5a); free rows: `releaseSQL` (`ReleasePrechargeTx`) and `INSERT INTO events`; COMMIT |
| billable request, settler (async) | `usage.(*Service).settle` → `settleTx` | BEGIN; `SELECT billing_status FROM usage_logs WHERE request_id = $1 FOR UPDATE`; `billing.SettleTx`: `settleSQL` (release and charge in one CTE statement; `ensureBalanceSQL`, `releaseSQL`, `applySQL` in a pgx batch until batch 5a, one statement each in batch 5a); `UPDATE usage_logs SET total_cost = …, billing_status = 'billed', … WHERE request_id = $1`; `INSERT INTO events`; COMMIT |
| async task submission, synchronous (added with batch 4b) | `usage.(*Service).BeginTask`, `ReceiveTask`, `RegisterTask` → `recordTask` ← gateway `tasks.go`, `execute.go` | `INSERT INTO task_submission_receipts`; `UPDATE task_submission_receipts SET state = $2, … response_body = COALESCE($4, response_body)`; in the usage transaction: `SELECT … FROM task_submission_receipts WHERE request_id = $1 AND state = 'received' FOR UPDATE`, `INSERT INTO async_tasks`, `UPDATE pending_settlements SET task_public_id`, `UPDATE task_submission_receipts SET state = 'registered'` |
| every async task poll, synchronous | `usage.(*Service).FindTask` → `readTask` ← gateway `tasks.go` | `SELECT public_id, … FROM async_tasks WHERE plugin_key = $1 AND kind = $2 AND user_id = $3 AND group_id = $4 AND (public_id = $5 OR …) ORDER BY public_id LIMIT 2` |

These are cached and reload only on expiry: gateway settings and the failure policy (10 s),
sticky rules (30 s), billing settings and price snapshot
(`model_prices`, 1 min), account group snapshots (30 s), authz permission
sets. The batched writers (`last_used_at`, account statistics, observations)
flush on a ticker and are not per request.

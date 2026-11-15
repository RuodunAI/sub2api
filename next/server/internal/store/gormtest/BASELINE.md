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
UPDATE, and `markExecutionCommitted`). The benchmark measures each pgx
statement next to the GORM form of the same statement (`Create` of
`model.PluginExecution`, `Updates` with a map, and the locked `Row().Scan`
plus `Updates` in one transaction):

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench HotExecution -benchmem -benchtime 1000x -count 5 ./internal/usage/
```

Recorded 2026-11-15 on gorm-4a, same machine and server as above (shared
with other test runs; ns/op is noise). Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotExecution/begin/pgx | 526 111 | 5 522 | 19 |
| BenchmarkHotExecution/begin/gorm | 2 734 771 | 14 868 | 121 |
| BenchmarkHotExecution/observe/pgx | 1 261 612 | 5 529 | 20 |
| BenchmarkHotExecution/observe/gorm | 511 548 | 11 869 | 86 |
| BenchmarkHotExecution/commit/pgx | 652 199 | 1 114 | 21 |
| BenchmarkHotExecution/commit/gorm | 687 708 | 13 194 | 161 |

GORM takes 6.4, 4.3 and 7.7 times the allocations (the limit is 1.5), so
the four statements stay on pgx under `//gormguard:allow-pgx`. Execution
recovery and retention, the monitor receipts, the helper usage receipts and
the console (usage lists, details, export, summary, overview, realtime,
manual reconcile and refund) are on GORM.

## Batch 4b: the settler's statements

Per billable request the settler runs, besides the `usage_logs` insert
(a pgx.Batch, unchanged) and the ledger statements (package billing):
`settleTx`'s row lock (`SELECT billing_status FROM usage_logs WHERE
request_id = $1 FOR UPDATE`) and its write of the billing fields
(`billUsageRow`). A request a plugin reserves runs `reserveTx` instead:
the write of the estimate and the `pending_settlements` insert (`ON
CONFLICT (plugin_key, ref_id) DO NOTHING RETURNING id`). The benchmark
measures each pgx statement next to its GORM form, against one pending
row. For the two writes the GORM form is the production constructor
(`billUsageRow`, `reserveUsageRow`) and both forms encode the jsonb detail
in the loop; the lock and the entry stay on pgx, so their GORM form
(`Raw(...).Row().Scan`) is written out in the benchmark:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench HotStmt -benchmem -benchtime 1000x -count 5 ./internal/usage/
```

Recorded 2026-11-15 on gorm-4fix-a (first on gorm-4b, where the write
benchmarks bound a pre-encoded detail: 99/139 and 70/104 allocs/op, 1.40
and 1.49 times), same machine and server as above. Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotStmtSettleLock/pgx | 237 292 | 461 | 9 |
| BenchmarkHotStmtSettleLock/gorm | 286 528 | 3 493 | 45 |
| BenchmarkHotStmtSettleUpdate/pgx | 370 680 | 4 599 | 129 |
| BenchmarkHotStmtSettleUpdate/gorm | 364 507 | 8 290 | 169 |
| BenchmarkHotStmtReserveUpdate/pgx | 337 561 | 3 925 | 98 |
| BenchmarkHotStmtReserveUpdate/gorm | 334 492 | 6 862 | 132 |
| BenchmarkHotStmtReserveEntry/pgx | 291 730 | 645 | 14 |
| BenchmarkHotStmtReserveEntry/gorm | 287 017 | 4 412 | 59 |

The row lock (5.0 times the allocations) and the reservation entry (4.2
times) stay on pgx under `//gormguard:allow-pgx`. The two writes (1.31 and
1.35 times; most of their allocations are encoding the decimal and jsonb
arguments, which both paths do) are on GORM. The whole settlement moves
from 863 to 903 allocs/op (BenchmarkHotSettle, 1.05 times; the free and
pending inserts are unchanged at 233 and 102):

| Benchmark | before | after |
|---|---:|---:|
| BenchmarkHotSettlerInsertFree | 233 | 233 |
| BenchmarkHotSettlerInsertPending | 102 | 102 |
| BenchmarkHotSettle | 863 | 903 |

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
(`readTask`), the most frequent of them. The benchmark measures each
production statement (pgx) next to the GORM form it had on gorm-4b
(`readTask`'s `Raw(...).Rows()`, the receipt's `Model(...).Create(map)`,
`Exec` for the rest), `recordTask` as its four statements in a
transaction of their own:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench HotTask -benchmem -benchtime 1000x -count 5 ./internal/usage/
```

Recorded 2026-11-15 on gorm-4fix-a, same machine. Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotTaskFind/pgx | 75 298 | 1 258 | 19 |
| BenchmarkHotTaskFind/gorm | 92 941 | 6 453 | 92 |
| BenchmarkHotTaskBegin/pgx | 233 794 | 394 | 10 |
| BenchmarkHotTaskBegin/gorm | 338 304 | 12 157 | 129 |
| BenchmarkHotTaskReceive/pgx | 225 962 | 268 | 6 |
| BenchmarkHotTaskReceive/gorm | 307 978 | 3 413 | 39 |
| BenchmarkHotTaskRegister/pgx | 699 972 | 1 699 | 47 |
| BenchmarkHotTaskRegister/gorm | 786 958 | 14 670 | 201 |

GORM takes 4.8 (`readTask`), 12.9 (the receipt insert), 6.5
(`receiveTask`) and 4.3 (`recordTask`) times the allocations, so all of
them stay on pgx under `//gormguard:allow-pgx`, with the SQL they had
before the migration. The lazy import of a legacy task (`importLegacyTask`,
once per legacy upstream ID that a poll names) and the receipt cleanup are
on GORM.

The rest of the settler (`markFailed`, `RetryPending`,
`closeUnreconcilable`, the reconcile loop, the task reconciler and the
hourly rollup) runs off the request path or on failures only and is on
GORM without a benchmark. The rollup's `INSERT INTO usage_hourly ...
SELECT` keeps its `$n` placeholders and runs through `execNumbered`,
because its measures hold the jsonb `?` operator (`anomalousRow`), which
GORM takes for a placeholder.

## Batch 4c: billing

The ledger writes of the request path are pgx batches and stay so (a
`pgx.Batch` is outside the migration): `prechargeTx` (← `Precharge`, every
billable request), `SettleTx` (settler, every billable request) and
`ApplyTx`. Their SQL (`prechargeSQL`, `releaseSQL`, `applySQL`,
`ensureBalanceSQL`), transaction boundaries, `FOR UPDATE` locks, lock
order and idempotency keys with `ON CONFLICT` are unchanged; amounts stay
`numeric` / `decimal.Decimal` throughout. The one direct statement of the
request path is `ReleasePrechargeTx` (`releaseSQL` on its own), which the
settler runs for every free request it records (usage `insertAtomic`; most
hold no reservation) and the expiry sweeper for every reservation it
returns. Its benchmark measures the pgx statement next to the GORM form of
the same statement (`Raw` with `?` placeholders, the arguments repeated),
for a request without a reservation (miss) and with one (hit):

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/billing/
```

Recorded 2026-11-15 on gorm-4c, same machine; the server was shared with
other test runs, so ns/op is noisy. Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotReleasePrecharge/miss/pgx | 102 700 | 1 213 | 30 |
| BenchmarkHotReleasePrecharge/miss/gorm | 115 185 | 8 437 | 76 |
| BenchmarkHotReleasePrecharge/hit/pgx | 551 849 | 1 342 | 37 |
| BenchmarkHotReleasePrecharge/hit/gorm | 497 944 | 8 535 | 81 |
| BenchmarkHotPrecharge (cdcc117c, before) | 728 823 | 8 755 | 277 |
| BenchmarkHotPrecharge (gorm-4c, after) | 966 492 | 8 833 | 277 |

GORM takes 2.5 and 2.2 times the allocations (the limit is 1.5), so
`ReleasePrechargeTx` stays on pgx under `//gormguard:allow-pgx`. Precharge
allocates the same before and after: its batch did not change.

On GORM, because they do not run for every request:

- `findLedger` (the ledger row of an idempotency key): only after an
  insert lost a race on the key, or for a duplicate settlement refund;
- `Balance` (← `CheckBalance`): behind the Redis balance cache, a miss only;
- `loadSettings` (← `Settings`) and the price snapshot (`snapshot`:
  `model_prices` and `loadRules`, ← `Resolve`): cached for `cacheTTL`
  (1 min) and dropped on change, a miss only;
- the expiry sweep's select, the console (prices, group and segment price
  rules, price sources and their sync, ledger lists, `/me/balance`,
  `/me/prices`, `/key/prices`, quotes).

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

## What every request runs

Read from the code at 06c2f18f. The gateway package itself runs no
statement per request; it reaches the database through the services below.

| When | Function | SQL |
|---|---|---|
| every request, synchronous | `apikey.(*Service).lookup` ← `Authenticate` ← gateway `call.run` | `lookupSQL`: `SELECT k.id, … FROM api_keys k JOIN users u … JOIN segments s … LEFT JOIN groups g … LEFT JOIN segment_groups sg … WHERE k.key_hash = $1 AND k.deleted_at IS NULL` (not cached, on purpose) |
| billable request, synchronous | `billing.(*Service).Precharge` → `prechargeTx` ← gateway `call.precharge` | BEGIN; batch: `SET LOCAL lock_timeout`, `INSERT INTO user_balances (user_id) … ON CONFLICT DO NOTHING`, `SELECT user_id FROM user_balances WHERE user_id = $1 FOR UPDATE`, `prechargeSQL` (CTE: insert `balance_ledger` ON CONFLICT DO NOTHING RETURNING, insert `request_precharges`, update `user_balances`); `INSERT INTO events`; COMMIT |
| billable request, Redis miss only | `billing.(*Service).Balance` ← `CheckBalance` | `SELECT COALESCE((SELECT balance FROM user_balances WHERE user_id = $1), 0), COALESCE((SELECT max(id) FROM balance_ledger WHERE user_id = $1), 0)` |
| every request, settler (async) | `usage.(*Service).insertAtomic` ← `insert` ← `process` / `Submit` | BEGIN; batch `INSERT INTO usage_logs (47 columns) … ON CONFLICT (request_id) DO NOTHING RETURNING id`; free rows: `releaseSQL` (`ReleasePrechargeTx`) and `INSERT INTO events`; COMMIT |
| billable request, settler (async) | `usage.(*Service).settle` → `settleTx` | BEGIN; `SELECT billing_status FROM usage_logs WHERE request_id = $1 FOR UPDATE`; `billing.SettleTx` batch: `ensureBalanceSQL`, `releaseSQL`, `applySQL`; `UPDATE usage_logs SET total_cost = …, billing_status = 'billed', … WHERE request_id = $1`; `INSERT INTO events`; COMMIT |
| async task submission, synchronous (added with batch 4b) | `usage.(*Service).BeginTask`, `ReceiveTask`, `RegisterTask` → `recordTask` ← gateway `tasks.go`, `execute.go` | `INSERT INTO task_submission_receipts`; `UPDATE task_submission_receipts SET state = $2, … response_body = COALESCE($4, response_body)`; in the usage transaction: `SELECT … FROM task_submission_receipts WHERE request_id = $1 AND state = 'received' FOR UPDATE`, `INSERT INTO async_tasks`, `UPDATE pending_settlements SET task_public_id`, `UPDATE task_submission_receipts SET state = 'registered'` |
| every async task poll, synchronous | `usage.(*Service).FindTask` → `readTask` ← gateway `tasks.go` | `SELECT public_id, … FROM async_tasks WHERE plugin_key = $1 AND kind = $2 AND user_id = $3 AND group_id = $4 AND (public_id = $5 OR …) ORDER BY public_id LIMIT 2` |

These are cached and reload only on expiry: gateway settings and the failure policy (10 s),
sticky rules (30 s), billing settings and price snapshot
(`model_prices`, 1 min), account group snapshots (30 s), authz permission
sets. The batched writers (`last_used_at`, account statistics, observations)
flush on a ticker and are not per request.

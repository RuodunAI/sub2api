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
answers Unknown. Its benchmark measures the pgx statement next to the GORM
form of the same statement (`Raw("SELECT EXISTS(?)", withPrefixes(...))`),
on a miss for an owner that holds one chain:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/helperhistory/
```

Recorded 2026-11-15 on gorm-1-fix (after 2579303e), same machine and
server as above. Medians of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotHelperPrefix/pgx | 54 952 | 549 | 12 |
| BenchmarkHotHelperPrefix/gorm | 75 165 | 12 102 | 130 |

GORM takes 10.8 times the allocations (the limit is 1.5), so anyPrefix
stays on pgx under `//gormguard:allow-pgx`. The locked reads of the same
records inside Lookup's transaction (`discoverPrefixes`, only for a
request that hits) are on GORM.

## Batch 2a: the account reads of the request path

The gateway reaches two account reads, both behind a 30 s cache
(`snapshotTTL`), so a request runs them on a miss only: `groupSnapshot`
(← `Candidates`, the schedulable accounts of the group) and the row of
`Load` (`loadLive`). Their benchmark measures each pgx statement next to
the GORM form of the same statement, on a group of ten accounts:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/account/
```

Recorded 2026-11-15 on gorm-2a, same machine and server as above. Medians

## Batch 2b: the CCGateway shared-runtime account read

`ccgateway.(*Service).sharedAccountType` runs for every model request
forwarded to the shared runtime (a configuration saved without per-account
runtimes; `attempt.resolveShared`), with no cache in front. With per-account
runtimes the request path reads the configuration, the account state, its
feature policy and request log level through the request cache
(`requestCacheTTL`, dropped on change), so those statements only run on a
miss and are on GORM. The benchmark measures the pgx statement next to the
GORM form (`Model(&model.Account{}).Select("type").Where(...).Take`):

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/ccgateway/
```

Recorded 2026-11-15 on gorm-2b, same machine and server as above. Medians

## Batch 2c: the plugin grant check

`grants.Holds` runs for every HostService call of a plugin instance that
needs a host permission (`hostServer.require`), so for every plugin-backed
gateway request that calls the host (account credentials, KV, locks,
prices ...). Its benchmark measures the pgx statement next to the GORM form
of the same statement (`Raw` over the named-parameter `versionRows`), on a
granted permission with a scope:

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/plugin/grants/
```

Recorded 2026-11-15 on gorm-2c, same machine and server as above. Medians
of 5.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| BenchmarkHotGroupSnapshot/pgx | 161 496 | 22 250 | 250 |
| BenchmarkHotGroupSnapshot/gorm | 248 970 | 37 865 | 610 |
| BenchmarkHotLoadRow/pgx | 100 952 | 6 432 | 63 |
| BenchmarkHotLoadRow/gorm | 136 736 | 16 629 | 206 |

GORM takes 2.4 and 3.3 times the allocations (the limit is 1.5), so both
stay on pgx under `//gormguard:allow-pgx`. `Load` reads its row with a
statement of its own (`loadLive`: any live account, no scope condition);
`loadRow`, which the console and the background jobs use, is on GORM. The
other account calls of the gateway (`GroupHasType` and `Unschedulable`
after a failed dispatch, the name read of `SetCooldown`, the automatic
disables) run on GORM: none of them runs for every request.

| BenchmarkHotSharedAccountType/pgx | 52 824 | 445 | 8 |
| BenchmarkHotSharedAccountType/gorm | 64 007 | 5 805 | 65 |

GORM takes 8.1 times the allocations, so sharedAccountType stays on pgx
under `//gormguard:allow-pgx`.

| BenchmarkHotGrantHolds/pgx | 79 491 | 549 | 11 |
| BenchmarkHotGrantHolds/gorm | 109 006 | 6 765 | 60 |

GORM takes 5.5 times the allocations (the limit is 1.5), so Holds stays on
pgx under `//gormguard:allow-pgx`. The other grant statements (instance
start, consent, rollout commit, revocation) are on GORM.

The provider resource statements (`internal/providerresources`) run only
for requests that carry or return provider files, containers or skills,
each inside a transaction that already takes advisory locks; they are on
GORM without a benchmark.

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

These are cached and reload only on expiry: gateway settings and the failure policy (10 s),
sticky rules (30 s), billing settings and price snapshot
(`model_prices`, 1 min), account group snapshots (30 s), authz permission
sets. The batched writers (`last_used_at`, account statistics, observations)
flush on a ticker and are not per request.

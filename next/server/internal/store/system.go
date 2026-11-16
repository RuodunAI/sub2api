package store

import (
	"context"
	"strconv"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
)

// System statements: the server's own state rather than rows of a table
// (the clock, snapshots, the database's name, transaction settings and
// advisory locks), one helper per kind, so the text of each lives here and
// the callers build none (CONTRACTS.md, raw-SQL policy). DDL is ddl.go's;
// the combinators every module shares (EXISTS, INSERT ... SELECT, nextval,
// to_regclass) are runtime-contract/gormpg's.

// ClockTimestamp reads the database clock (clock_timestamp(): the time of
// the call, not of the transaction's start).
func ClockTimestamp(db *gorm.DB) (time.Time, error) {
	var at time.Time
	err := db.Raw("SELECT clock_timestamp()").Row().Scan(&at)
	return at, err
}

// CurrentSnapshot returns the xmin and xmax of the current snapshot of db
// (pg_current_snapshot): every transaction id below xmin has finished, and
// none at or past xmax had started.
func CurrentSnapshot(db *gorm.DB) (xmin, xmax uint64, err error) {
	var a, b string
	if err = db.Raw(`SELECT pg_snapshot_xmin(s)::text, pg_snapshot_xmax(s)::text FROM pg_current_snapshot() s`).Row().Scan(&a, &b); err != nil {
		return 0, 0, err
	}
	if xmin, err = strconv.ParseUint(a, 10, 64); err != nil {
		return 0, 0, err
	}
	xmax, err = strconv.ParseUint(b, 10, 64)
	return xmin, xmax, err
}

// CurrentDatabase returns the name of the database db is connected to.
func CurrentDatabase(db *gorm.DB) (string, error) {
	var name string
	err := db.Raw(`SELECT current_database()`).Row().Scan(&name)
	return name, err
}

// SetLocal sets the setting name - a run-time parameter such as
// statement_timeout, or a custom one such as sup2api.x that triggers read
// with current_setting - to value for the rest of the transaction
// (set_config(name, value, true), SET LOCAL with parameters).
func SetLocal(ctx context.Context, tx *gorm.DB, name, value string) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return G(ctx, tx).Exec("SELECT set_config(?, ?, true)", name, value).Error
}

// XactLock takes the transaction-scoped advisory lock named key, which
// holds until tx ends: the transactions that take it on one key run their
// work one after another. Its id is the key's hashtextextended. It needs a
// transaction (RequireTx), since on the pool the lock would end with its
// own statement.
func XactLock(ctx context.Context, tx *gorm.DB, key string) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return G(ctx, tx).Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, key).Error
}

// TryXactLock is XactLock without the wait (gormpg.TryXactLock): it takes
// the lock when it is free and reports whether it did; false when another
// transaction holds it.
func TryXactLock(ctx context.Context, tx *gorm.DB, key string) (bool, error) {
	if err := RequireTx(tx); err != nil {
		return false, err
	}
	return gormpg.TryXactLock(G(ctx, tx), key)
}

// XactLockID is XactLock for a lock named by its id rather than a key.
func XactLockID(ctx context.Context, tx *gorm.DB, id int64) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return G(ctx, tx).Exec(`SELECT pg_advisory_xact_lock(?)`, id).Error
}

// XactLockHashtext is XactLock with the lock's id the key's 32-bit
// hashtext, the id the locks taken before XactLock existed are named by:
// they keep it, so that nodes of the previous release, during a rolling
// upgrade, still take the same lock. A new lock takes XactLock.
func XactLockHashtext(ctx context.Context, tx *gorm.DB, key string) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return G(ctx, tx).Exec(`SELECT pg_advisory_xact_lock(hashtext(?))`, key).Error
}

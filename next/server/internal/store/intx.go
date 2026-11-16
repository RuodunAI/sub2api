package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// ErrNoTx is RequireTx's error for a handle that runs on no transaction.
var ErrNoTx = errors.New("store: this statement needs a transaction (store.DB.Tx or TxOptions)")

// InTx reports whether q runs its statements in a transaction: a handle of
// DB.Tx or DB.TxOptions (or of GORM's Transaction on a WithConn connection),
// and not the pool or a connection outside one.
func InTx(q *gorm.DB) bool {
	if q == nil || q.Statement == nil {
		return false
	}
	_, ok := q.Statement.ConnPool.(gorm.TxCommitter)
	return ok
}

// RequireTx is nil when q runs in a transaction (InTx), else ErrNoTx. A
// function that takes a *gorm.DB but means a transaction - a row lock
// (FOR UPDATE), a transaction-scoped advisory lock, several writes that
// commit together - calls it first: the type no longer says so, and on the
// pool such a lock would end with its own statement. gormguard [tx-lock]
// holds every function that locks to it.
func RequireTx(q *gorm.DB) error {
	if InTx(q) {
		return nil
	}
	return ErrNoTx
}

// XactLock takes the transaction-scoped advisory lock named key, which
// holds until tx ends: the transactions that take it on one key run their
// work one after another. It needs a transaction (RequireTx), since on the
// pool the lock would end with its own statement.
func XactLock(ctx context.Context, tx *gorm.DB, key string) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return xactLock(G(ctx, tx), key).Error
}

// TryXactLock is XactLock without the wait: it takes the lock when it is
// free and reports whether it did; false when another transaction holds it.
func TryXactLock(ctx context.Context, tx *gorm.DB, key string) (bool, error) {
	if err := RequireTx(tx); err != nil {
		return false, err
	}
	var locked bool
	err := G(ctx, tx).Raw(`SELECT pg_try_advisory_xact_lock(hashtextextended(?, 0))`, key).Row().Scan(&locked)
	return locked, err
}

// xactLock is XactLock's statement: the lock's id is the key's
// hashtextextended.
func xactLock(db *gorm.DB, key string) *gorm.DB {
	return db.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, key)
}

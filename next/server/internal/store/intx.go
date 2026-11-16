package store

import (
	"context"
	"errors"
	"strings"

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

// xactLock is XactLock's statement: the lock's id is the key's
// hashtextextended.
func xactLock(db *gorm.DB, key string) *gorm.DB {
	return db.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, key)
}

// XactLock32 is XactLock with the lock's id the key's 32-bit hashtext, the
// id of the locks taken before XactLock existed: a key some running node
// (or a migration) still locks that way keeps it, so both exclude each
// other across a rolling upgrade.
func XactLock32(ctx context.Context, tx *gorm.DB, key string) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return G(ctx, tx).Exec(`SELECT pg_advisory_xact_lock(hashtext(?))`, key).Error
}

// SetLocal sets the run-time parameter name (a constant of the caller's,
// such as statement_timeout) to value for the rest of the transaction.
// SET takes no parameters, so the value goes in as a quoted literal.
func SetLocal(ctx context.Context, tx *gorm.DB, name, value string) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return G(ctx, tx).Exec("SET LOCAL " + QuoteIdent(name) + " TO " + quoteLiteral(value)).Error
}

// quoteLiteral quotes s as an SQL string literal.
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

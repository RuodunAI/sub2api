package store

import (
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

// TxKey identifies a transaction: the handles of one transaction have
// equal keys, those of another a different one. It reaches nothing; the
// zero key stands for no transaction. It holds a reference to the
// transaction object itself, not its address or a hash of it: while a key
// is kept, that object cannot be freed and its address taken by a later
// transaction, so a kept key never equals a later transaction's.
type TxKey struct{ c gorm.ConnPool }

// TxKeyOf is the TxKey of the transaction q runs in, the zero key outside
// one (InTx).
func TxKeyOf(q *gorm.DB) TxKey {
	if !InTx(q) {
		return TxKey{}
	}
	return TxKey{q.Statement.ConnPool}
}

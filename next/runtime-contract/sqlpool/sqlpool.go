// Package sqlpool is a *sql.DB as GORM's connection pool (gorm.ConnPool)
// whose statements do not wait on one another's row locks.
//
// database/sql hands the connection of DB.ExecContext back to the pool
// before the caller reads the result, and the result's RowsAffected then
// takes that connection's mutex (execDC, driverResult). GORM reads it right
// after every Exec, Update and Delete. In between, another goroutine can
// take the connection and run a statement that waits in PostgreSQL for a
// row lock, holding the mutex for as long: the first caller is stuck until
// that lock is released, and if its own transaction holds the lock, nothing
// ever releases it and PostgreSQL sees no deadlock to break. Pool's
// ExecContext reads the result on a connection of its own before giving it
// back. A transaction (*sql.Tx) or a *sql.Conn keeps its connection until
// it ends, so their statements need no wrapper.
//
// Standard library only, so that the core and the gateway share it; the
// SDK, which depends on no other module of the repository, has a copy
// (sdk/pgdb).
package sqlpool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// ErrNoLastInsertID is the LastInsertId of Pool's results: PostgreSQL has
// none (pgx's driver reports an error too); read keys with RETURNING.
var ErrNoLastInsertID = errors.New("sqlpool: no LastInsertId on PostgreSQL, use RETURNING")

// Pool runs statements on DB. It is a gorm.ConnPool, a gorm.TxBeginner and
// a gorm.GetDBConnector, so GORM opens transactions on DB and gorm.DB.DB()
// returns DB.
type Pool struct{ db *sql.DB }

// New wraps db.
func New(db *sql.DB) Pool { return Pool{db: db} }

// badConnAttempts is database/sql's: DB.ExecContext tries a statement that
// failed with driver.ErrBadConn (sent nowhere) up to three times.
const badConnAttempts = 3

// ExecContext runs query on a connection it holds until the result is read.
func (p Pool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	for attempt := 1; ; attempt++ {
		res, err := p.exec(ctx, query, args)
		if attempt == badConnAttempts || !errors.Is(err, driver.ErrBadConn) {
			return res, err
		}
	}
}

func (p Pool) exec(ctx context.Context, query string, args []any) (sql.Result, error) {
	c, err := p.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := c.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return failed{err}, nil
	}
	return rowsAffected(n), nil
}

// QueryContext is DB's: the rows hold their connection until closed.
func (p Pool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return p.db.QueryContext(ctx, query, args...)
}

// QueryRowContext is DB's, for the same reason.
func (p Pool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return p.db.QueryRowContext(ctx, query, args...)
}

// PrepareContext is DB's. A *sql.Stmt runs on the pool like DB does, with
// the same race on its results: GORM prepares only with PrepareStmt, which
// the users of Pool leave off.
func (p Pool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return p.db.PrepareContext(ctx, query)
}

// BeginTx is DB's: a transaction keeps its connection.
func (p Pool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return p.db.BeginTx(ctx, opts)
}

// GetDBConn returns DB, for gorm.DB.DB.
func (p Pool) GetDBConn() (*sql.DB, error) { return p.db, nil }

// rowsAffected is a result read in full. An integer type, so that a small
// count boxes into sql.Result without an allocation.
type rowsAffected int64

func (n rowsAffected) RowsAffected() (int64, error) { return int64(n), nil }
func (rowsAffected) LastInsertId() (int64, error)   { return 0, ErrNoLastInsertID }

// failed is a result whose RowsAffected failed.
type failed struct{ err error }

func (f failed) RowsAffected() (int64, error) { return 0, f.err }
func (failed) LastInsertId() (int64, error)   { return 0, ErrNoLastInsertID }

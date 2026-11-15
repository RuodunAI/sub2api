package pgdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// pool is a *sql.DB as GORM's connection pool whose Exec reads its result
// before the connection goes back.
//
// database/sql hands the connection of DB.ExecContext back to the pool
// before the caller reads the result, and the result's RowsAffected then
// takes that connection's mutex; GORM reads it right after every Exec,
// Update and Delete. In between, another goroutine can take the connection
// and run a statement that waits for a row lock, holding the mutex as long:
// if the first caller's own transaction holds that lock, neither moves and
// PostgreSQL sees no deadlock to break. A transaction keeps its connection
// and needs no wrapper.
//
// The core and the gateway share this wrapper as
// runtime-contract/sqlpool; the SDK depends on no other module of the
// repository, so that a plugin builds alone, and keeps this copy.
type pool struct{ db *sql.DB }

// errNoLastInsertID is the LastInsertId of pool's results: PostgreSQL has
// none (pgx's driver reports an error too); read keys with RETURNING.
var errNoLastInsertID = errors.New("pgdb: no LastInsertId on PostgreSQL, use RETURNING")

// ExecContext runs query on a connection it holds until the result is read,
// and like DB.ExecContext tries a statement that failed with
// driver.ErrBadConn (sent nowhere) up to three times.
func (p pool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	for attempt := 1; ; attempt++ {
		res, err := p.exec(ctx, query, args)
		if attempt == 3 || !errors.Is(err, driver.ErrBadConn) {
			return res, err
		}
	}
}

func (p pool) exec(ctx context.Context, query string, args []any) (sql.Result, error) {
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
	return result{n, err}, nil
}

// QueryContext, QueryRowContext, PrepareContext and BeginTx are DB's: rows
// and transactions hold their connection until they end. GORM prepares only
// with PrepareStmt, which Open leaves off.
func (p pool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return p.db.QueryContext(ctx, query, args...)
}

func (p pool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return p.db.QueryRowContext(ctx, query, args...)
}

func (p pool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return p.db.PrepareContext(ctx, query)
}

func (p pool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return p.db.BeginTx(ctx, opts)
}

// GetDBConn returns DB, for gorm.DB.DB (Close).
func (p pool) GetDBConn() (*sql.DB, error) { return p.db, nil }

// result is a result read in full.
type result struct {
	n   int64
	err error
}

func (r result) RowsAffected() (int64, error) { return r.n, r.err }
func (result) LastInsertId() (int64, error)   { return 0, errNoLastInsertID }

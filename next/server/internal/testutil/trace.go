package testutil

import (
	"context"
	"database/sql"
	"sync"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Statements is the log of the statements a Traced database ran, in order.
type Statements struct {
	mu  sync.Mutex
	sql []string
}

func (s *Statements) add(q string) {
	s.mu.Lock()
	s.sql = append(s.sql, q)
	s.mu.Unlock()
}

// Take returns the statements logged since the last Take and empties the
// log.
func (s *Statements) Take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sql
	s.sql = nil
	return out
}

// Traced returns db with every statement its Gorm handle runs logged, for a
// test that bounds the round trips of a path: each statement once, and a
// transaction's BEGIN, COMMIT and ROLLBACK as "begin", "commit" and
// "rollback". It shares db's pool; what runs on db.SQL directly is not
// logged, nor is a transaction there a store.SQLTx (it is wrapped).
func Traced(db *store.DB) (*store.DB, *Statements) {
	log := &Statements{}
	g := db.Gorm.Session(&gorm.Session{NewDB: true})
	g.Statement.ConnPool = tracedPool{pool: db.Gorm.Statement.ConnPool, log: log}
	return &store.DB{Gorm: g, SQL: db.SQL}, log
}

// tracedPool logs the statements run on pool.
type tracedPool struct {
	pool gorm.ConnPool
	log  *Statements
}

func (p tracedPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	p.log.add(query)
	return p.pool.PrepareContext(ctx, query)
}

func (p tracedPool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	p.log.add(query)
	return p.pool.ExecContext(ctx, query, args...)
}

func (p tracedPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	p.log.add(query)
	return p.pool.QueryContext(ctx, query, args...)
}

func (p tracedPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	p.log.add(query)
	return p.pool.QueryRowContext(ctx, query, args...)
}

// BeginTx makes tracedPool a gorm.ConnPoolBeginner: GORM's Transaction and
// Begin open the transaction here and run its statements on the traced
// transaction it returns.
func (p tracedPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	b, ok := p.pool.(gorm.TxBeginner)
	if !ok {
		return nil, gorm.ErrInvalidTransaction
	}
	p.log.add("begin")
	tx, err := b.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &tracedTx{tracedPool: tracedPool{pool: tx, log: p.log}, tx: tx}, nil
}

// tracedTx is a traced transaction, a gorm.TxCommitter (a pointer: GORM
// checks it with reflect.Value.IsNil).
type tracedTx struct {
	tracedPool
	tx *sql.Tx
}

func (t *tracedTx) Commit() error {
	t.log.add("commit")
	return t.tx.Commit()
}

func (t *tracedTx) Rollback() error {
	t.log.add("rollback")
	return t.tx.Rollback()
}

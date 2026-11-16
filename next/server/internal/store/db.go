// Package store owns the PostgreSQL pool, transactions and the migration
// runner shared by the core schema and plugin schemas. Statements are built
// and run by GORM over that pool (see gorm.go).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
)

// DB is the core's connection pool.
type DB struct {
	// Gorm runs every statement: on the pool, or through Tx, TxOptions and
	// WithConn on one transaction or connection of it.
	Gorm *gorm.DB
	// SQL is the same pool as a database/sql handle, for what needs one
	// rather than GORM: River, the tests. Production code goes through Gorm
	// (gormguard [sql-handle]).
	SQL *sql.DB
}

// Open connects to dsn. The pool is sized by SUB2API_PG_MAX_CONNS, else the
// DSN's pool_* parameters (see pool.go), else the defaults.
func Open(ctx context.Context, dsn string) (*DB, error) {
	db, s, err := open(ctx, dsn, func(p *gormpg.Pool) { p.OverrideMaxConns(os.Getenv("SUB2API_PG_MAX_CONNS")) })
	if err != nil {
		return nil, err
	}
	// No min_conns: database/sql keeps no minimum, and Apply logs a
	// pool_min_conns of the DSN as ignored.
	slog.Info("database pool opened", "max_conns", s.MaxConns,
		"max_conn_lifetime", s.MaxConnLifetime, "max_conn_idle_time", s.MaxConnIdleTime)
	return db, nil
}

// OpenSized connects to dsn with a pool of at most maxConns connections,
// whatever the DSN or the environment say: a pool for one job, such as
// running a plugin's migrations as its own role.
func OpenSized(ctx context.Context, dsn string, maxConns int) (*DB, error) {
	db, _, err := open(ctx, dsn, func(p *gormpg.Pool) { p.MaxConns = &maxConns })
	return db, err
}

// open connects to dsn and pings it. The pool is sized from the DSN's
// pool_* parameters, as size changes them, over poolDefaults; s is what it
// got. gormpg.ParseConfig pins DateStyle ISO, the format Times parses
// array text in (pgtext.go).
func open(ctx context.Context, dsn string, size func(*gormpg.Pool)) (_ *DB, s gormpg.Settings, _ error) {
	cfg, pool, err := gormpg.ParseConfig(dsn)
	if err != nil {
		return nil, s, err
	}
	size(&pool)
	g, sqlDB, err := gormpg.Open(cfg, gormpg.Options{Pool: pool, Defaults: poolDefaults, Gorm: gormConfig(), Setup: registerTimestamps})
	if err != nil {
		return nil, s, err
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, s, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Gorm: g, SQL: sqlDB}, pool.Over(poolDefaults), nil
}

// Close closes the pool.
func (db *DB) Close() { _ = db.SQL.Close() }

// MaxConns is the size of the pool, 0 for a DB without one.
func (db *DB) MaxConns() int {
	if db == nil || db.SQL == nil {
		return 0
	}
	return db.SQL.Stats().MaxOpenConnections
}

// Ping checks that the database answers on a pooled connection.
func (db *DB) Ping(ctx context.Context) error { return db.SQL.PingContext(ctx) }

// Tx runs fn in a transaction, committing on nil error and rolling back on
// an error or a panic, which it then re-raises.
func (db *DB) Tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return db.Gorm.WithContext(ctx).Transaction(fn)
}

// TxOptions is Tx with an isolation level or read-only access, such as
// ReadOnly() or Snapshot().
func (db *DB) TxOptions(ctx context.Context, opts sql.TxOptions, fn func(tx *gorm.DB) error) error {
	return db.Gorm.WithContext(ctx).Transaction(fn, &opts)
}

// ReadOnly is a read-only transaction at the default isolation level. A
// function, not a variable: no package can change what every other one
// gets.
func ReadOnly() sql.TxOptions { return sql.TxOptions{ReadOnly: true} }

// Snapshot is a read-only repeatable-read transaction: all its reads see one
// snapshot of the database.
func Snapshot() sql.TxOptions {
	return sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}

// WithConn runs fn on one connection taken from the pool, for state that
// lives on a session rather than a transaction (a session advisory lock).
// conn's statements all run on that connection. GORM's Transaction on conn
// is package store's own (the migrator's): gormguard [gorm-tx] refuses it
// elsewhere, so code outside store that needs a transaction takes Tx or
// TxOptions instead. The connection goes back to the pool when fn returns.
func (db *DB) WithConn(ctx context.Context, fn func(conn *gorm.DB) error) error {
	c, err := db.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// A session of its own: the Statement is a copy, so pointing it at the
	// connection leaves the pool's handle as it is.
	s := db.Gorm.Session(&gorm.Session{NewDB: true, Context: ctx})
	s.Statement.ConnPool = c
	return fn(s)
}

// SQLTx is the database/sql transaction a handle of Tx, TxOptions or a
// WithConn transaction runs on, for a library that takes one (River's
// InsertTx); nil when tx is no transaction.
func SQLTx(tx *gorm.DB) *sql.Tx {
	if tx == nil {
		return nil
	}
	t, _ := tx.Statement.ConnPool.(*sql.Tx)
	return t
}

// IsNoRows reports a read that found no row: GORM's
// gorm.ErrRecordNotFound (First, Take, Last) and database/sql's
// sql.ErrNoRows (Row().Scan).
func IsNoRows(err error) bool { return gormpg.IsNoRows(err) }

// QuoteIdent quotes a (qualified) identifier for SQL text, for DDL, which
// takes no parameters: QuoteIdent("s", "t") is "s"."t".
func QuoteIdent(parts ...string) string {
	q := make([]string, len(parts))
	for i, p := range parts {
		p = strings.ReplaceAll(p, "\x00", "")
		q[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(q, ".")
}

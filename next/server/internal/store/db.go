// Package store owns the PostgreSQL pool, transactions and the migration
// runner shared by the core schema and plugin schemas. Statements are built
// and run by GORM over that pool (see gorm.go).
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/pgdsn"
	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/sqlpool"
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
	dsn, pool, err := pgdsn.Split(dsn)
	if err != nil {
		return nil, err
	}
	maxConns := poolMaxConns(os.Getenv("SUB2API_PG_MAX_CONNS"), pool.MaxConns)
	pool.MaxConns = &maxConns
	db, err := open(ctx, dsn, pool)
	if err != nil {
		return nil, err
	}
	// No min_conns: database/sql keeps no minimum, and Apply logs a
	// pool_min_conns of the DSN as ignored.
	slog.Info("database pool opened", "max_conns", maxConns,
		"max_conn_lifetime", valueOr(pool.MaxConnLifetime, poolDefaults.MaxConnLifetime),
		"max_conn_idle_time", valueOr(pool.MaxConnIdleTime, poolDefaults.MaxConnIdleTime))
	return db, nil
}

// OpenSized connects to dsn with a pool of at most maxConns connections,
// whatever the DSN or the environment say: a pool for one job, such as
// running a plugin's migrations as its own role.
func OpenSized(ctx context.Context, dsn string, maxConns int) (*DB, error) {
	dsn, pool, err := pgdsn.Split(dsn)
	if err != nil {
		return nil, err
	}
	pool.MaxConns = &maxConns
	return open(ctx, dsn, pool)
}

// open connects to dsn, which holds no pool_* parameter, and sizes the pool
// from pool over poolDefaults.
func open(ctx context.Context, dsn string, pool pgdsn.Pool) (*DB, error) {
	// database/sql hands array columns over as their text, which Times
	// parses in DateStyle ISO's format (pgtext.go). The session's DateStyle
	// is pinned to it, over the DSN's (a datestyle parameter, or -c in
	// options) and the role's, database's or server's default, which could
	// print 'SQL, DMY' or German dates.
	dsn, err := pgdsn.Set(dsn, "datestyle", "ISO")
	if err != nil {
		return nil, err
	}
	g, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), gormConfig())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := registerTimestamps(g); err != nil {
		return nil, err
	}
	sqlDB, err := g.DB()
	if err != nil {
		return nil, err
	}
	pool.Apply(sqlDB, poolDefaults, nil)
	// GORM reads an Exec's RowsAffected after database/sql has given the
	// connection back, under a mutex that the connection's next statement
	// holds: sqlpool reads it first (see there). Transactions and WithConn
	// keep their connection and need no wrapper.
	g.ConnPool = sqlpool.New(sqlDB)
	g.Statement.ConnPool = g.ConnPool
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Gorm: g, SQL: sqlDB}, nil
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
func IsNoRows(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, sql.ErrNoRows)
}

// DryRun is a DryRun session of a GORM instance configured as Open's
// (callbacks, clause builders), on a pool that never connects: it builds
// statements without a database, and one that escaped DryRun would fail
// instead of reaching a server. Package gormtest snapshots its SQL.
func DryRun() *gorm.DB { return dryRun().Session(&gorm.Session{DryRun: true, NewDB: true}) }

// ErrOffline is what a statement of DryRun's instance that is no DryRun
// gets: there is no database behind it.
var ErrOffline = errors.New("store: the DryRun instance has no database")

var dryRun = sync.OnceValue(func() *gorm.DB {
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: sql.OpenDB(offline{})}), gormConfig())
	if err == nil {
		err = registerTimestamps(g)
	}
	if err != nil {
		panic("store: open the DryRun instance: " + err.Error()) // no I/O: only a programming error gets here
	}
	return g
})

// offline is a database/sql connector and driver whose connections all
// fail to open.
type offline struct{}

func (offline) Connect(context.Context) (driver.Conn, error) { return nil, ErrOffline }
func (offline) Driver() driver.Driver                        { return offline{} }
func (offline) Open(string) (driver.Conn, error)             { return nil, ErrOffline }

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

// GORM bridge.
//
// GORM builds SQL and maps rows to structs; pgx executes it. G binds a GORM
// handle to the caller's Querier (the pool or the pgx.Tx of DB.Tx), so code
// already on GORM and code still on raw pgx share one transaction:
//
//	err := db.Tx(ctx, func(tx pgx.Tx) error {
//		if err := store.G(ctx, tx).Create(&row).Error; err != nil {
//			return err
//		}
//		_, err := tx.Exec(ctx, `UPDATE ...`) // sees row
//		return err
//	})
//
// Rules:
//   - Build every handle with G and keep its context: a statement whose
//     context carries no Querier fails (ErrNoQuerier) instead of running on
//     some other connection. Do not replace that context (WithContext,
//     Session{Context: ...}).
//   - Transactions start only with DB.Tx; GORM's Begin and Transaction fail
//     (ErrGORMTx).
//   - Array parameters and columns are Int64s / Strings; a plain slice is
//     expanded by GORM into one placeholder per element.
//   - The schema belongs to Migrate and the .sql files: AutoMigrate and
//     CreateTable fail (ErrGORMMigrate).
//   - Errors stay pgx's: IsUniqueViolation and friends work as with pgx;
//     test "not found" with IsNoRows (GORM reports it as
//     gorm.ErrRecordNotFound).

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

var (
	// ErrNoQuerier is a GORM statement run on a context that G did not
	// build.
	ErrNoQuerier = errors.New("store: GORM statement without a Querier; build the handle with store.G(ctx, q)")
	// ErrGORMTx is a transaction GORM tried to open itself.
	ErrGORMTx = errors.New("store: GORM cannot begin transactions; call store.G with the pgx.Tx of store.DB.Tx")
	// ErrGORMMigrate is a schema change GORM tried to make itself.
	ErrGORMMigrate = errors.New("store: GORM does not change the schema; add a migration under internal/migrations")
	errGORMPrepare = errors.New("store: the GORM bridge does not prepare statements")
)

type querierKey struct{}

// G is a GORM handle whose statements run on q, with ctx as their context.
func G(ctx context.Context, q Querier) *gorm.DB {
	return gormDB().WithContext(context.WithValue(ctx, querierKey{}, q))
}

// gormDB is the one process-wide GORM instance. Opening it does no I/O: its
// *sql.DB only forwards, so there is no pool to size or close.
var gormDB = sync.OnceValue(func() *gorm.DB {
	sqlDB := sql.OpenDB(bridgeConnector{})
	// Connections are stateless values; keeping a few idle only spares
	// database/sql from re-wrapping one per statement under concurrency.
	sqlDB.SetMaxIdleConns(64)
	g, err := gorm.Open(bridgeDialector{postgres.Dialector{Config: &postgres.Config{Conn: sqlDB}}}, GormConfig())
	if err != nil {
		panic("store: open GORM: " + err.Error()) // no I/O: only a programming error gets here
	}
	return g
})

// GormConfig is the configuration of G's GORM instance, a new value on
// each call (gorm.Open keeps state in it). Tools that build SQL with GORM
// outside G, such as DryRun snapshots, use it so their SQL is G's.
func GormConfig() *gorm.Config {
	return &gorm.Config{
		NamingStrategy:         schema.NamingStrategy{}, // snake_case, plural table names
		SkipDefaultTransaction: true,                    // the caller owns the transaction
		PrepareStmt:            false,
		DisableAutomaticPing:   true,
		// Select the model's columns, not *: the row decodes only what the
		// model maps, and a column added later costs nothing until mapped.
		QueryFields: true,
		// Statements are not logged, as on the raw pgx path: errors go back
		// to the caller, and the SQL text could carry values.
		Logger: logger.Discard,
		// TranslateError stays off so that errors remain *pgconn.PgError.
		TranslateError: false,
	}
}

// bridgeDialector is GORM's postgres dialect with the schema changes
// switched off.
type bridgeDialector struct{ postgres.Dialector }

func (d bridgeDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return bridgeMigrator{d.Dialector.Migrator(db)}
}

type bridgeMigrator struct{ gorm.Migrator }

func (bridgeMigrator) AutoMigrate(...any) error { return ErrGORMMigrate }
func (bridgeMigrator) CreateTable(...any) error { return ErrGORMMigrate }

type bridgeConnector struct{}

func (bridgeConnector) Connect(context.Context) (driver.Conn, error) { return bridgeConn{}, nil }
func (bridgeConnector) Driver() driver.Driver                        { return bridgeDriver{} }

type bridgeDriver struct{}

func (bridgeDriver) Open(string) (driver.Conn, error) { return bridgeConn{}, nil }

// bridgeConn holds no state: each statement runs on the Querier its context
// carries, so the same value serves every goroutine.
type bridgeConn struct{}

func querierOf(ctx context.Context) (Querier, error) {
	if q, ok := ctx.Value(querierKey{}).(Querier); ok {
		return q, nil
	}
	return nil, ErrNoQuerier
}

func (bridgeConn) Prepare(string) (driver.Stmt, error) { return nil, errGORMPrepare }
func (bridgeConn) Close() error                        { return nil }
func (bridgeConn) Begin() (driver.Tx, error)           { return nil, ErrGORMTx }
func (bridgeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return nil, ErrGORMTx
}

// CheckNamedValue hands every argument to pgx as is: pgx encodes Go values
// (arrays included) itself, better than database/sql's conversion would.
func (bridgeConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (bridgeConn) ExecContext(ctx context.Context, query string, nv []driver.NamedValue) (driver.Result, error) {
	q, err := querierOf(ctx)
	if err != nil {
		return nil, err
	}
	tag, err := q.Exec(ctx, query, plainArgs(nil, nv)...)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(tag.RowsAffected()), nil
}

func (bridgeConn) QueryContext(ctx context.Context, query string, nv []driver.NamedValue) (driver.Rows, error) {
	q, err := querierOf(ctx)
	if err != nil {
		return nil, err
	}
	// The first argument is a pgx query option, not a parameter.
	r, err := q.Query(ctx, query, plainArgs([]any{resultFormats}, nv)...)
	if err != nil {
		return nil, err
	}
	return newBridgeRows(r)
}

// plainArgs appends the values of nv to head.
func plainArgs(head []any, nv []driver.NamedValue) []any {
	out := append(make([]any, 0, len(head)+len(nv)), head...)
	for _, v := range nv {
		out = append(out, v.Value)
	}
	return out
}

var _ interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.NamedValueChecker
} = bridgeConn{}

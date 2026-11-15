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
//   - Array parameters and columns are Int64s / Strings / Times; GORM
//     expands a plain slice into one placeholder per element.
//   - A jsonb column read into a Go value (a []string, a struct) is a
//     JSON[T], e.g. JSON[[]string]: GORM hands the raw document over where
//     pgx used to decode it. json.RawMessage keeps the document as is.
//   - Read results into a row struct of their own (a model, or a local
//     struct tagged gorm:"column:...") and convert it to the API type, so
//     the API shape and the columns change apart. Scanning into an API or
//     core type directly is fine where its fields are the columns read,
//     one to one.
//   - Give model and result fields concrete types: GORM refuses a struct
//     with a field of type any or another interface ("unsupported data
//     type"); a document is a JSON[T] or json.RawMessage.
//   - The schema belongs to Migrate and the .sql files: every Migrator
//     operation fails (ErrGORMMigrate), AutoMigrate included.
//   - Errors stay pgx's: IsUniqueViolation and friends work as with pgx;
//     test "not found" with IsNoRows (GORM reports it as
//     gorm.ErrRecordNotFound).
//   - An UPDATE writes exactly the columns the raw SQL wrote: updated_at
//     only where that SQL set it, with the same expression (now(),
//     clock_timestamp()), e.g. "updated_at": gorm.Expr("now()") in the
//     Updates map. Nothing adds it on its own; some updated_at columns are
//     versions that must not move. An INSERT fills a zero created_at or
//     updated_at that no database default fills with now()
//     (gorm_timestamps.go). Save writes every field as the struct holds it,
//     updated_at included.
//   - Call a function that returns void (pg_advisory_xact_lock and the
//     other advisory locks) with Exec, not Raw(...).Scan: there is no value
//     to scan, and the bridge does not read the void type.
//   - numeric NaN and ±Infinity do not fit decimal.Decimal, so scanning one
//     fails; filter or cast them in SQL where a column can hold them.
//   - Raw(...).Scan reads rows by its destination: a slice gets every row, a
//     struct the first, a scalar each row in turn (the last one stays). Give
//     a scalar a query of one row. Int64s, Strings and Times are slices
//     too: read one array value with Row().Scan or into a struct field.

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	errNilRows     = errors.New("store: the Querier's Query returned neither rows nor an error")
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
	// No idle connections: database/sql reads RowsAffected of an Exec under
	// the mutex of the driverConn it ran on, after handing that connection
	// back to the pool. A pooled connection taken meanwhile by a statement
	// that waits on a row lock of the first caller's transaction holds that
	// mutex, and both goroutines wait forever (PostgreSQL sees no deadlock;
	// TestGORMExecDoesNotDeadlockOnRowLocks). A closed connection is never
	// shared, and opening one costs nothing: it is a stateless value.
	// The other limits stay at their defaults, which do not block: no cap
	// on open connections (a cap would make statements queue for a
	// driverConn while their transactions hold row locks, the same wait in
	// another form; pgxpool already bounds the real connections), and no
	// lifetimes, which only apply to idle or reused connections.
	sqlDB.SetMaxIdleConns(0)
	g, err := gorm.Open(bridgeDialector{postgres.Dialector{Config: &postgres.Config{Conn: sqlDB}}}, gormConfig())
	if err == nil {
		err = registerTimestamps(g)
	}
	if err != nil {
		panic("store: open GORM: " + err.Error()) // no I/O: only a programming error gets here
	}
	return g
})

// gormConfig is the configuration of G's GORM instance. DryRun snapshots
// (package gormtest) take a session of G, so they build the same SQL.
func gormConfig() *gorm.Config {
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

// bridgeDialector is GORM's postgres dialect without its migrator.
type bridgeDialector struct{ postgres.Dialector }

func (bridgeDialector) Migrator(*gorm.DB) gorm.Migrator { return bridgeMigrator{} }

// bridgeMigrator refuses every operation, the reads as well: the schema is
// the migrations', and no code should plan against GORM's view of it. The
// methods without an error result panic with ErrGORMMigrate instead; only
// a Migrator() call, which gormguard rejects, reaches them.
type bridgeMigrator struct{}

var _ gorm.Migrator = bridgeMigrator{}

func (bridgeMigrator) AutoMigrate(...any) error                   { return ErrGORMMigrate }
func (bridgeMigrator) CurrentDatabase() string                    { panic(ErrGORMMigrate) }
func (bridgeMigrator) FullDataTypeOf(*schema.Field) clause.Expr   { panic(ErrGORMMigrate) }
func (bridgeMigrator) GetTypeAliases(string) []string             { panic(ErrGORMMigrate) }
func (bridgeMigrator) CreateTable(...any) error                   { return ErrGORMMigrate }
func (bridgeMigrator) DropTable(...any) error                     { return ErrGORMMigrate }
func (bridgeMigrator) HasTable(any) bool                          { panic(ErrGORMMigrate) }
func (bridgeMigrator) RenameTable(any, any) error                 { return ErrGORMMigrate }
func (bridgeMigrator) GetTables() ([]string, error)               { return nil, ErrGORMMigrate }
func (bridgeMigrator) TableType(any) (gorm.TableType, error)      { return nil, ErrGORMMigrate }
func (bridgeMigrator) AddColumn(any, string) error                { return ErrGORMMigrate }
func (bridgeMigrator) DropColumn(any, string) error               { return ErrGORMMigrate }
func (bridgeMigrator) AlterColumn(any, string) error              { return ErrGORMMigrate }
func (bridgeMigrator) HasColumn(any, string) bool                 { panic(ErrGORMMigrate) }
func (bridgeMigrator) RenameColumn(any, string, string) error     { return ErrGORMMigrate }
func (bridgeMigrator) ColumnTypes(any) ([]gorm.ColumnType, error) { return nil, ErrGORMMigrate }
func (bridgeMigrator) CreateView(string, gorm.ViewOption) error   { return ErrGORMMigrate }
func (bridgeMigrator) DropView(string) error                      { return ErrGORMMigrate }
func (bridgeMigrator) CreateConstraint(any, string) error         { return ErrGORMMigrate }
func (bridgeMigrator) DropConstraint(any, string) error           { return ErrGORMMigrate }
func (bridgeMigrator) HasConstraint(any, string) bool             { panic(ErrGORMMigrate) }
func (bridgeMigrator) CreateIndex(any, string) error              { return ErrGORMMigrate }
func (bridgeMigrator) DropIndex(any, string) error                { return ErrGORMMigrate }
func (bridgeMigrator) HasIndex(any, string) bool                  { panic(ErrGORMMigrate) }
func (bridgeMigrator) RenameIndex(any, string, string) error      { return ErrGORMMigrate }
func (bridgeMigrator) GetIndexes(any) ([]gorm.Index, error)       { return nil, ErrGORMMigrate }
func (bridgeMigrator) MigrateColumn(any, *schema.Field, gorm.ColumnType) error {
	return ErrGORMMigrate
}
func (bridgeMigrator) MigrateColumnUnique(any, *schema.Field, gorm.ColumnType) error {
	return ErrGORMMigrate
}

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
	switch {
	case err != nil:
		return nil, err
	case r == nil:
		return nil, errNilRows // a broken (fake) Querier; pgx never does this
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

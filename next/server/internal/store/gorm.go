// GORM.
//
// Every statement runs on GORM over the pool of DB: db.Gorm outside a
// transaction, the handle DB.Tx (TxOptions, WithConn) passes inside one. G
// binds such a handle to a context, so a function takes the handle as q
// and does not care which it is:
//
//	err := db.Tx(ctx, func(tx *gorm.DB) error {
//		if err := store.G(ctx, tx).Create(&row).Error; err != nil {
//			return err
//		}
//		return store.G(ctx, tx).Exec(`UPDATE ...`).Error // sees row
//	})
//
// Rules:
//   - Transactions start only with DB.Tx and DB.TxOptions (inside package
//     store, the migrator also opens them on a WithConn connection); GORM's
//     Begin, Transaction, Commit and the other transaction methods, and the
//     Migrator, are refused by gormguard elsewhere, and so is a handle of
//     another instance (gorm.Open). A function that needs the transaction
//     it is handed checks it with RequireTx; gormguard [tx-lock] holds a
//     function that locks to it.
//   - Row locks are RowLock values: ForUpdate(), Lock(strength) and their
//     Of and SkipLocked, never a shared clause.Locking variable.
//   - Array parameters and columns are Int64s / Strings / Times; GORM
//     expands a plain slice into one placeholder per element. A bytea or
//     document parameter ([]byte, json.RawMessage) right after "(" is a
//     Bytes, for the same reason.
//   - A jsonb column read into a Go value (a []string, a struct) is a
//     JSON[T], e.g. JSON[[]string]: database/sql hands over the raw
//     document. json.RawMessage keeps the document as is.
//   - Read results into a row struct of their own (a model, or a local
//     struct tagged gorm:"column:...") and convert it to the API type, so
//     the API shape and the columns change apart. Scanning into an API or
//     core type directly is fine where its fields are the columns read,
//     one to one.
//   - Give model and result fields concrete types: GORM refuses a struct
//     with a field of type any or another interface ("unsupported data
//     type"); a document is a JSON[T] or json.RawMessage.
//   - The schema belongs to Migrate and the .sql files: never AutoMigrate
//     or use the Migrator.
//   - Errors are the driver's: test codes with IsUniqueViolation and the
//     other error readers of db.go, and "not found" with IsNoRows (GORM
//     reports it as gorm.ErrRecordNotFound, Row().Scan as sql.ErrNoRows).
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
//     to scan, and database/sql does not read the void type.
//   - numeric NaN and ±Infinity do not fit decimal.Decimal, so scanning one
//     fails; filter or cast them in SQL where a column can hold them.
//   - Raw(...).Scan reads rows by its destination: a slice gets every row, a
//     struct the first, a scalar each row in turn (the last one stays). Give
//     a scalar a query of one row. Int64s, Strings and Times are slices
//     too: read one array value with Row().Scan or into a struct field.

package store

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// G is q, the pool's handle or a transaction's, with ctx as the context of
// its statements.
func G(ctx context.Context, q *gorm.DB) *gorm.DB {
	return q.WithContext(ctx)
}

// InsertChunk is the row count of each full INSERT of a writer whose
// batches vary in size: it creates them with CreateInBatches(rows,
// InsertChunk), so every full chunk has one text, and pgx prepares and
// caches at most InsertChunk texts of it on a connection (the full chunk's
// and one per remainder size) instead of one per batch size.
const InsertChunk = 100

// gormConfig is the configuration of Open's GORM instance. DryRun snapshots
// (package gormtest) take an instance of the same configuration, so they
// build the same SQL.
func gormConfig() *gorm.Config {
	return &gorm.Config{
		NamingStrategy:         schema.NamingStrategy{}, // snake_case, plural table names
		SkipDefaultTransaction: true,                    // the caller owns the transaction
		PrepareStmt:            false,                   // pgx caches the statements of each connection itself
		DisableAutomaticPing:   true,                    // Open pings, with the caller's context
		// Select the model's columns, not *: the row decodes only what the
		// model maps, and a column added later costs nothing until mapped.
		QueryFields: true,
		// Statements are not logged: errors go back to the caller, and the
		// SQL text could carry values.
		Logger: logger.Discard,
		// TranslateError stays off so that errors remain the driver's,
		// which the error readers (db.go) take apart.
		TranslateError: false,
	}
}

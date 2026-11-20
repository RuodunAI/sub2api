package pgdb

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
)

// SQL is a fragment of SQL text that a helper hands on to GORM (a column
// list, an ORDER BY, a condition): a constant of the code, never input.
// An untyped string constant converts to it by itself; a string variable
// takes an explicit conversion, which TestNoRawSQL rejects unless the value
// is built from constants. Values go in as ? parameters, and identifiers a
// request chooses through a fixed whitelist into clause.Column or
// clause.OrderByColumn.
type SQL string

// The statements GORM's builder has no form for are runtime-contract/
// gormpg's, which the core and the gateway run as well; these are the
// plugins' names for them.

// Exists reports whether sub, a query built on db, finds a row, in one
// SELECT EXISTS round trip; sub's select list becomes 1.
func Exists(db, sub *gorm.DB) (bool, error) { return gormpg.Exists(db, sub) }

// InsertSelect inserts the rows sub selects into table, its values in the
// order of columns, with clauses after it (a clause.OnConflict, or a
// pointer to one): INSERT ... SELECT, which Create, writing VALUES, cannot.
// The statement runs; its result has the error and the rows written.
func InsertSelect(db *gorm.DB, table string, columns []string, sub *gorm.DB, clauses ...clause.Expression) *gorm.DB {
	return gormpg.InsertSelect(db, table, columns, sub, clauses...)
}

// NextIDs reserves n ids from the id sequence of table, ascending, for a
// batch insert that must know each row's id before it writes the rows: the
// caller sets the ids on its models and creates them with one INSERT.
func NextIDs(db *gorm.DB, table string, n int) ([]int64, error) { return gormpg.NextIDs(db, table, n) }

// LockTable takes the SHARE ROW EXCLUSIVE lock on table until tx ends: it
// conflicts with itself and with every row write, not with plain reads, so
// the transactions that take it replace the table's rows one after another
// while readers go on. Unlike an advisory lock, its key is the table, not
// a number shared with the core and every other plugin. PostgreSQL refuses
// it outside a transaction.
func LockTable(tx *gorm.DB, table string) error {
	return tx.Exec("LOCK TABLE ? IN SHARE ROW EXCLUSIVE MODE", clause.Table{Name: table}).Error
}

// TryXactLock tries the transaction-level advisory lock of name for the
// connected user, the plugin's database identity, which names the plugin
// and the database (so plugins, and installs sharing a server, keep their
// own keys), and reports whether tx got it; it is held until tx ends. The
// key is hashtextextended(name || ':' || current_user, 0)
// (gormpg.TryXactLock).
func TryXactLock(tx *gorm.DB, name string) (bool, error) {
	return gormpg.TryXactLock(tx, gorm.Expr("? || ':' || current_user", name))
}

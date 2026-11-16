package pgdb

import (
	"fmt"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SQL is a fragment of SQL text that a helper hands on to GORM (a column
// list, an ORDER BY, a condition): a constant of the code, never input.
// An untyped string constant converts to it by itself; a string variable
// takes an explicit conversion, which TestNoRawSQL rejects unless the value
// is a constant. Values go in as ? parameters, and identifiers a request
// chooses through a fixed whitelist into clause.Column or
// clause.OrderByColumn.
type SQL string

// Exists reports whether sub, a query built on db, finds a row, in one
// SELECT EXISTS round trip; sub's select list becomes 1.
func Exists(db, sub *gorm.DB) (bool, error) {
	var found bool
	err := db.Raw("SELECT EXISTS (?)", sub.Select("1")).Scan(&found).Error
	return found, err
}

// InsertSelect inserts the rows sub selects into table, its values in the
// order of columns, with conflict (nil = none) as the ON CONFLICT clause:
// INSERT ... SELECT, which Create, writing VALUES, cannot. The statement
// runs; its result has the error and the rows written.
func InsertSelect(db *gorm.DB, table string, columns []string, sub *gorm.DB, conflict *clause.OnConflict) *gorm.DB {
	cols := make([]clause.Column, len(columns))
	for i, c := range columns {
		cols[i] = clause.Column{Name: c}
	}
	if conflict == nil {
		return db.Exec("INSERT INTO ? ? ?", clause.Table{Name: table}, cols, sub)
	}
	return db.Exec("INSERT INTO ? ? ? ?", clause.Table{Name: table}, cols, sub, *conflict)
}

// NextIDs reserves n ids from the id sequence of table, ascending, for a
// batch insert that must know each row's id before it writes the rows: the
// caller sets the ids on its models and creates them with one INSERT.
func NextIDs(db *gorm.DB, table string, n int) ([]int64, error) {
	ids := make([]int64, 0, n)
	if n <= 0 {
		return ids, nil
	}
	err := db.Raw(`SELECT nextval(pg_get_serial_sequence(?, 'id')) FROM generate_series(1, ?)`, table, n).Scan(&ids).Error
	if err != nil {
		return nil, err
	}
	if len(ids) != n {
		return nil, fmt.Errorf("pgdb: drew %d ids from %s, want %d", len(ids), table, n)
	}
	slices.Sort(ids)
	return ids, nil
}

// LockTable takes the SHARE ROW EXCLUSIVE lock on table until tx ends: it
// conflicts with itself and with every row write, not with plain reads, so
// the transactions that take it replace the table's rows one after another
// while readers go on. Unlike an advisory lock, its key is the table, not
// a number shared with the core and every other plugin. PostgreSQL refuses
// it outside a transaction.
func LockTable(tx *gorm.DB, table string) error {
	return tx.Exec("LOCK TABLE ? IN SHARE ROW EXCLUSIVE MODE", clause.Table{Name: table}).Error
}

package gormpg

import (
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Statements GORM's builder has no form for, written once for the core, the
// gateway and the SDK: thin combinators around builder queries (EXISTS,
// INSERT ... SELECT) and the server functions they need (nextval,
// to_regclass, a transaction-scoped advisory lock). The modules' SQL checks
// (package sqlcheck) allow these declarations here and nowhere else.

// ExistsQuery selects, in one round trip, whether each of subs (queries
// built on db) finds a row: SELECT EXISTS (sub1), EXISTS (sub2) ... Each
// sub's select list becomes 1. Scan or Row reads the flags.
func ExistsQuery(db *gorm.DB, subs ...*gorm.DB) *gorm.DB {
	args := make([]any, len(subs))
	for i, sub := range subs {
		args[i] = sub.Select("1")
	}
	return db.Raw("SELECT "+strings.TrimSuffix(strings.Repeat("EXISTS (?),", len(subs)), ","), args...)
}

// Exists reports whether sub, a query built on db, finds a row
// (ExistsQuery).
func Exists(db, sub *gorm.DB) (bool, error) {
	var found bool
	err := ExistsQuery(db, sub).Scan(&found).Error
	return found, err
}

// InsertSelect runs INSERT INTO table (columns) sub, sub a query built on db
// whose select list gives the columns' values in order, with clauses after
// it (a clause.OnConflict): GORM's Create only inserts values.
// RowsAffected counts the rows written.
func InsertSelect(db *gorm.DB, table string, columns []string, sub *gorm.DB, clauses ...clause.Expression) *gorm.DB {
	return db.Exec("?", insertSelect{table: table, columns: columns, sub: sub, clauses: clauses})
}

// InsertSelectReturning is InsertSelect not yet run, for a RETURNING among
// clauses (clause.Returning, or gorm.Expr("RETURNING ...") of expressions):
// Scan or Row reads the rows written.
func InsertSelectReturning(db *gorm.DB, table string, columns []string, sub *gorm.DB, clauses ...clause.Expression) *gorm.DB {
	return db.Raw("?", insertSelect{table: table, columns: columns, sub: sub, clauses: clauses})
}

type insertSelect struct {
	table   string
	columns []string
	sub     *gorm.DB
	clauses []clause.Expression
}

func (s insertSelect) Build(b clause.Builder) {
	b.WriteString("INSERT INTO ")
	b.WriteQuoted(clause.Table{Name: s.table})
	b.WriteString(" (")
	for i, c := range s.columns {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteQuoted(clause.Column{Name: c})
	}
	b.WriteString(") ")
	b.AddVar(b, s.sub)
	for _, c := range s.clauses {
		b.WriteByte(' ')
		b.AddVar(b, c)
	}
}

// NextIDs reserves n ids from the id sequence of table, ascending, for a
// batch insert that must know each row's id before it writes the rows: the
// caller sets the ids on its models and creates them with one INSERT, so no
// row depends on the order RETURNING happens to give.
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
		return nil, fmt.Errorf("gormpg: drew %d ids from %s, want %d", len(ids), table, n)
	}
	slices.Sort(ids)
	return ids, nil
}

// RelationExists reports whether name (schema-qualified, or on the
// search_path) is a table, view or other relation (to_regclass): a table of
// another program's schema, or of a version that may not be installed.
func RelationExists(db *gorm.DB, name string) (bool, error) {
	var ok bool
	err := db.Raw(`SELECT to_regclass(?) IS NOT NULL`, name).Scan(&ok).Error
	return ok, err
}

// TryXactLock takes the transaction-scoped advisory lock whose id is the
// hashtextextended of key when no other transaction holds it, and reports
// whether it did; the lock holds until tx ends. key is bound as a
// parameter: a string, or an expression that gives one (gorm.Expr). On the
// pool, outside a transaction, the lock would end with the statement: the
// caller checks tx.
func TryXactLock(tx *gorm.DB, key any) (bool, error) {
	var locked bool
	err := tx.Raw(`SELECT pg_try_advisory_xact_lock(hashtextextended(?, 0))`, key).Scan(&locked).Error
	return locked, err
}

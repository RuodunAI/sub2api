package store

import (
	"database/sql"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Query building helpers for the GORM path.

// Conds are WHERE conditions gathered one at a time and applied together.
// The zero value holds none.
type Conds []func(*gorm.DB) *gorm.DB

// And adds cond, whose "?" placeholders take args in order; the conditions
// are ANDed. An array argument is an Int64s, Strings or Times, as for Where.
func (c *Conds) And(cond string, args ...any) {
	*c = append(*c, func(db *gorm.DB) *gorm.DB { return db.Where(cond, args...) })
}

// Scope applies the conditions, for db.Scopes.
func (c Conds) Scope() func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		for _, cond := range c {
			db = cond(db)
		}
		return db
	}
}

// SetIf puts *v into set under column unless v is nil: set is the column map
// of a partial UPDATE, in which a nil field is one the request left out and
// the column keeps its value.
func SetIf[T any](set map[string]any, column string, v *T) {
	if v != nil {
		set[column] = *v
	}
}

// The row lock strengths that gorm's clause package has no constant for
// (it has clause.LockingStrengthUpdate and clause.LockingStrengthShare).
const (
	LockingStrengthNoKeyUpdate = "NO KEY UPDATE"
	LockingStrengthKeyShare    = "KEY SHARE"
)

// RowLock locks the rows a query reads until the transaction ends, for
// db.Clauses: Lock or ForUpdate builds one, Of and SkipLocked refine it.
// Each returns a new value, so no package can change the lock another one
// takes (a shared clause.Locking variable could be rewritten for the whole
// process with one assignment).
type RowLock struct{ clause.Locking }

// Lock is a row lock of strength: clause.LockingStrengthUpdate or Share,
// LockingStrengthNoKeyUpdate or KeyShare.
func Lock(strength string) RowLock { return RowLock{clause.Locking{Strength: strength}} }

// ForUpdate is Lock(clause.LockingStrengthUpdate): FOR UPDATE.
func ForUpdate() RowLock { return Lock(clause.LockingStrengthUpdate) }

// Of limits l to the rows of one table of a join, by its name or alias:
// FOR UPDATE OF a.
func (l RowLock) Of(table string) RowLock {
	l.Table = clause.Table{Name: table}
	return l
}

// SkipLocked makes l skip the rows another transaction holds instead of
// waiting for them: FOR UPDATE SKIP LOCKED.
func (l RowLock) SkipLocked() RowLock {
	l.Options = clause.LockingOptionsSkipLocked
	return l
}

// ExistsQuery selects whether sub, a query built on db, finds a row, in one
// SELECT EXISTS round trip; sub's select list becomes 1.
func ExistsQuery(db, sub *gorm.DB) *gorm.DB {
	return db.Raw("SELECT EXISTS (?)", sub.Select("1"))
}

// Exists reports whether sub, a query built on db, finds a row (ExistsQuery).
func Exists(db, sub *gorm.DB) (bool, error) {
	var found bool
	err := ExistsQuery(db, sub).Scan(&found).Error
	return found, err
}

// Reading rows by position, for statements whose columns have no names of
// their own (expressions) or the same name twice, or whose rows are folded
// as they come rather than mapped onto a struct.

// RowScanner is a *sql.Row or *sql.Rows: a scan function written against it
// reads the one row of Row() and each row of Rows() (EachRow, CollectRows).
type RowScanner interface{ Scan(dest ...any) error }

// EachRow runs db, a query, and calls fn on each row it returns, in order.
// The first error of fn stops the read and is returned; the rows are closed
// either way.
func EachRow(db *gorm.DB, fn func(*sql.Rows) error) error {
	rows, err := db.Rows()
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CollectRows runs db, a query, and returns what scan reads of each row, in
// order: never nil, so that no rows is an empty list (pgx.CollectRows).
func CollectRows[T any](db *gorm.DB, scan func(*sql.Rows) (T, error)) ([]T, error) {
	out := []T{}
	err := EachRow(db, func(rows *sql.Rows) error {
		v, err := scan(rows)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
	slices.Sort(ids)
	return ids, err
}

// Statements the query builder has no form for, as thin combinators of
// builder parts (gormguard [raw-sql] lists each one with its reason).

// InsertSelect runs INSERT INTO table (columns) sub, sub a query built on db,
// with clauses after it (a clause.OnConflict): GORM's Create only inserts
// values. RowsAffected counts the rows written.
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

// UpdateFrom is the FROM list of an UPDATE, for db.Clauses: UPDATE t SET ...
// FROM sql WHERE ..., sql taking vars at its ? placeholders (a set of rows
// to join, such as unnest(?::bigint[], ?::timestamptz[]) AS v(id, at)).
func UpdateFrom(sql string, vars ...any) clause.Interface {
	return updateFrom{clause.Expr{SQL: sql, Vars: vars}}
}

type updateFrom struct{ clause.Expr }

func (updateFrom) Name() string                   { return "FROM" }
func (f updateFrom) MergeClause(c *clause.Clause) { c.Expression = f.Expr }

// ClockTimestamp reads the database clock (clock_timestamp(): the time of
// the call, not of the transaction's start).
func ClockTimestamp(db *gorm.DB) (time.Time, error) {
	var at time.Time
	err := db.Raw("SELECT clock_timestamp()").Row().Scan(&at)
	return at, err
}

// SetConfig sets the custom setting name (such as sup2api.x) to value for
// the rest of the transaction (set_config(name, value, true)): triggers read
// it with current_setting.
func SetConfig(tx *gorm.DB, name, value string) error {
	if err := RequireTx(tx); err != nil {
		return err
	}
	return tx.Exec("SELECT set_config(?, ?, true)", name, value).Error
}

// OneRow is a query of one row and no columns, for a Select of expressions
// that read no table of their own (scalar subqueries, COALESCE((?), 0)).
func OneRow(db *gorm.DB) *gorm.DB { return db.Table("(SELECT) AS one") }

package store

import (
	"reflect"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Timestamps on the GORM path.
//
// Most created_at and updated_at columns default to now(), and no trigger
// maintains updated_at: the raw SQL writes updated_at = now() in each
// UPDATE that should, and leaves it alone in the others (proxies.updated_at,
// for one, versions the cached HTTP client, so an UPDATE that bumped it
// would rebuild the client). GORM's own stamping (autoCreateTime,
// autoUpdateTime) reads the Go clock, so the generated models switch it
// off, and G's instance follows the raw SQL instead:
//
//   - an UPDATE sets updated_at only where the statement assigns it, as the
//     raw SQL it replaces did: Update("updated_at", gorm.Expr("now()")) or
//     an "updated_at" entry in an Updates map;
//   - an INSERT writes now() for a zero created_at or updated_at that is
//     NOT NULL without a database default (a time.Time field without a
//     default tag) and reads the value back with RETURNING, as GORM reads
//     the defaulted columns. Inside a transaction now() is the
//     transaction's start time, on both paths.
//
// The INSERT rewrites the VALUES clause as the statement is built (gorm's
// ClauseBuilders): GORM fills VALUES from the struct inside its create
// callback, with no hook between filling and building, and only an
// expression in the clause yields the database clock.

const (
	createdAtColumn = "created_at"
	updatedAtColumn = "updated_at"
)

var (
	dbNow    = clause.Expr{SQL: "now()"}
	timeType = reflect.TypeFor[time.Time]()
)

func registerTimestamps(g *gorm.DB) error {
	g.ClauseBuilders["VALUES"] = buildValues
	return g.Callback().Create().Before("gorm:create").Register("store:return_stamped", returnStamped)
}

// stampedOnInsert reports a column that the INSERT stamps.
func stampedOnInsert(f *schema.Field) bool {
	return f != nil && (f.DBName == createdAtColumn || f.DBName == updatedAtColumn) &&
		f.FieldType == timeType && !f.HasDefaultValue && f.Creatable
}

func statementSchema(b clause.Builder) (*gorm.Statement, *schema.Schema) {
	stmt, ok := b.(*gorm.Statement)
	if !ok {
		return nil, nil
	}
	return stmt, stmt.Schema
}

// buildValues writes now() into the zero cells of stamped columns.
func buildValues(c clause.Clause, b clause.Builder) {
	values, ok := c.Expression.(clause.Values)
	if _, s := statementSchema(b); ok && s != nil {
		for i, col := range values.Columns {
			if stampedOnInsert(s.LookUpField(col.Name)) {
				stampZero(values.Values, i)
			}
		}
	}
	c.Build(b)
}

// stampZero replaces the zero times in column i of rows. The rows are the
// statement's own, built for this INSERT alone.
func stampZero(rows [][]any, i int) {
	for _, row := range rows {
		if t, ok := row[i].(time.Time); ok && t.IsZero() {
			row[i] = dbNow
		}
	}
}

// returnStamped adds the stamped columns to the INSERT's RETURNING. GORM's
// create callback returns only the columns with a database default, and
// adds that clause only when there is none yet, so this one carries both.
func returnStamped(db *gorm.DB) {
	s := db.Statement.Schema
	if db.Error != nil || s == nil {
		return
	}
	if _, ok := db.Statement.Clauses["RETURNING"]; ok {
		return
	}
	var stamped []clause.Column
	for _, name := range [...]string{createdAtColumn, updatedAtColumn} {
		if stampedOnInsert(s.LookUpField(name)) {
			stamped = append(stamped, clause.Column{Name: name})
		}
	}
	if len(stamped) == 0 {
		return
	}
	cols := make([]clause.Column, 0, len(s.FieldsWithDefaultDBValue)+len(stamped))
	for _, f := range s.FieldsWithDefaultDBValue {
		if f.Readable {
			cols = append(cols, clause.Column{Name: f.DBName})
		}
	}
	db.Statement.AddClause(clause.Returning{Columns: append(cols, stamped...)})
}

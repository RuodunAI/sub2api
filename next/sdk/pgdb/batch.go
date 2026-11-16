package pgdb

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaxBatchRows is the most rows one INSERT of Insert carries.
const MaxBatchRows = 1000

// maxParams is PostgreSQL's limit of bind parameters per statement.
const maxParams = 65535

// Insert inserts rows on db in the order given, MaxBatchRows at a time (fewer
// for a table so wide that a batch would pass PostgreSQL's 65535 parameters),
// each INSERT with clauses: an ON CONFLICT, a RETURNING. The order is the
// caller's: writers that sort their rows lock them in the same order and do
// not deadlock. Insert opens no transaction (handles skip GORM's default
// one), so several batches are atomic only on a transaction; InsertAll opens
// one.
func Insert[T any](db *gorm.DB, rows []T, clauses ...clause.Expression) error {
	if len(rows) == 0 {
		return nil
	}
	return db.Clauses(clauses...).CreateInBatches(&rows, batchRows(db, new(T))).Error
}

// InsertAll is Insert of all rows or none: the batches share a transaction
// (a savepoint when db is in one already). A single batch is one statement,
// atomic by itself, and needs none.
func InsertAll[T any](db *gorm.DB, rows []T, clauses ...clause.Expression) error {
	if len(rows) <= batchRows(db, new(T)) {
		return Insert(db, rows, clauses...)
	}
	return db.Transaction(func(tx *gorm.DB) error { return Insert(tx, rows, clauses...) })
}

// batchRows is how many rows of model one INSERT carries.
func batchRows(db *gorm.DB, model any) int {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(model); err != nil || len(stmt.Schema.DBNames) == 0 {
		return MaxBatchRows // the INSERT reports the model's error
	}
	return max(1, min(MaxBatchRows, maxParams/len(stmt.Schema.DBNames)))
}

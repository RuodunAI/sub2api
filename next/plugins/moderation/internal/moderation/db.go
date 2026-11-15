package moderation

import (
	"encoding/json"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The statements of this package are SQL written out (with ? placeholders):
// the reads scan their columns by position into the plugin's own types,
// through the helpers below. They give database/sql scanning the shape of
// the pgx calls they replaced, so every query keeps its text, its locks and
// its result: queryRow is QueryRow, eachRow ForEachRow, collect
// CollectRows, affected the row count of Exec. Arrays are bound and scanned
// as pgdb.Array (GORM would expand a plain slice into a list of
// parameters), JSON values the driver hands over as text are decoded with
// jsonOf. The record and statistics batches are model rows (internal/model)
// inserted with insertRows.
//
// The *gorm.DB a function gets carries the context its statements run
// under (Host.DB binds it): helpers take no context of their own.

// row is one result row read by position: the first row of queryRow, or the
// current row of eachRow and collect.
type row interface{ Scan(dest ...any) error }

// firstRow is a query run when its first row is scanned.
type firstRow struct{ q *gorm.DB }

// queryRow is query's first row. Scan runs the query and reports
// gorm.ErrRecordNotFound when it returns no row.
func queryRow(db *gorm.DB, query string, args ...any) row {
	return firstRow{db.Raw(query, args...)}
}

func (r firstRow) Scan(dest ...any) error {
	rows, err := r.q.Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return gorm.ErrRecordNotFound
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	// The statement runs to its end (an INSERT ... RETURNING of several
	// rows, a later row's error) before it counts as done.
	return rows.Close()
}

// affected is a statement's row count and error, the pair Exec returned.
func affected(res *gorm.DB) (int64, error) { return res.RowsAffected, res.Error }

// eachRow runs q and calls fn after scanning each row into dest.
func eachRow(q *gorm.DB, dest []any, fn func() error) error {
	rows, err := q.Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		if err := fn(); err != nil {
			return err
		}
	}
	return rows.Err()
}

// collect runs q and maps each row with scan. No rows is an empty slice,
// not nil: lists go out as JSON arrays.
func collect[T any](q *gorm.DB, scan func(row) (T, error)) ([]T, error) {
	rows, err := q.Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// column reads the one column of q's rows.
func column[T any](q *gorm.DB) ([]T, error) {
	return collect(q, func(r row) (v T, err error) { return v, r.Scan(&v) })
}

// insertBatch is the most rows one INSERT carries (PostgreSQL takes at most
// 65535 parameters per statement).
const insertBatch = 1000

// insertRows inserts rows insertBatch at a time, in the order given:
// callers sort them, so that concurrent writers lock rows in the same order
// and do not deadlock. Callers that need all of them or none (the batch
// the statements replace) run it in a transaction.
func insertRows[T any](tx *gorm.DB, rows []T, clauses ...clause.Expression) error {
	return tx.Clauses(clauses...).CreateInBatches(&rows, insertBatch).Error
}

// columns names the conflict target of an upsert.
func columns(names ...string) []clause.Column {
	out := make([]clause.Column, len(names))
	for i, n := range names {
		out[i] = clause.Column{Name: n}
	}
	return out
}

// assign sets column to the SQL expression expr in an upsert.
func assign(column, expr string) clause.Assignment {
	return clause.Assignment{Column: clause.Column{Name: column}, Value: gorm.Expr(expr)}
}

// jsonOf scans a json or jsonb column into v (NULL leaves v alone):
// database/sql hands the value over as text, where pgx decoded it.
func jsonOf(v any) jsonColumn { return jsonColumn{v} }

type jsonColumn struct{ v any }

func (c jsonColumn) Scan(src any) error {
	switch b := src.(type) {
	case nil:
		return nil
	case []byte:
		return json.Unmarshal(b, c.v)
	case string:
		return json.Unmarshal([]byte(b), c.v)
	}
	return fmt.Errorf("moderation: cannot decode %T as JSON", src)
}

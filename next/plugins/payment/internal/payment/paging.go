package payment

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// pagedQuery is one paged list endpoint over the table of a model. The
// filter is written once: listPaged applies it to both the count and the page.
type pagedQuery struct {
	// columns is the page's select list.
	columns string
	// where is the optional filter shared by both statements; args are its
	// arguments.
	where string
	args  []any
	// orderBy is the page statement's ORDER BY ("created_at DESC").
	orderBy string
	// Labels are the failure prefixes, kept per stage because the endpoints
	// report the raw error to administrators ("count codes: ...").
	countLabel string
	queryLabel string
}

// listPaged counts the rows of model M that q selects and reads one page of
// them, converted by conv. It returns the total for the pagination block,
// which callers still need even when the page is empty.
func listPaged[M, T any](db *gorm.DB, q pagedQuery, limit, offset int, conv func(M) T) ([]T, int, error) {
	db = db.Model(new(M))
	if q.where != "" {
		db = db.Where(q.where, q.args...)
	}
	rows, total, err := pgdb.List[M](db, q.columns, q.orderBy, limit, offset)
	if err != nil {
		label := q.queryLabel
		var le *pgdb.ListError
		if errors.As(err, &le) && le.Count {
			label = q.countLabel
		}
		return nil, 0, fmt.Errorf("%s: %w", label, err)
	}
	items := make([]T, len(rows))
	for i, r := range rows {
		items[i] = conv(r)
	}
	return items, int(total), nil
}

// lockedRow describes the row a redemption locks and the wording of its
// failures: the messages are rendered to the console verbatim, so each caller
// keeps its own.
type lockedRow struct {
	// columns are read; where selects exactly one row with one argument.
	columns string
	where   string
	arg     any
	// missing is returned when no row matches.
	missing string
	// queryFail prefixes a genuine query error ("query code").
	queryFail string
}

// withLockedRow runs fn inside a transaction that holds the FOR UPDATE lock on
// the row of model M that l selects; the transaction commits when fn returns
// nil. fn reads and writes through tx.
func withLockedRow[M any](db *gorm.DB, l lockedRow, fn func(tx *gorm.DB, row M) error) error {
	return inTx(db, func(tx *gorm.DB) error {
		var row M
		err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
			Select(l.columns).Where(l.where, l.arg).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New(l.missing)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", l.queryFail, err)
		}
		return fn(tx, row)
	})
}

// inTx runs fn in a transaction that commits when fn returns nil. Unlike
// gorm.DB.Transaction it keeps the wording of begin and commit failures,
// which the endpoints report.
func inTx(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	tx := db.Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin transaction: %w", tx.Error)
	}
	defer tx.Rollback() // a no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// deref is the value p points to, the zero value for nil (a NULL column).
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

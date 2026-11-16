package pgdb

import "gorm.io/gorm"

// List reads one page of a list and the total its pagination shows: it
// counts the rows q selects, then reads limit of them from offset on, the
// given columns (every column when empty) in order, into a []T (a struct of
// the columns, or one column's type). q names the table (Model or Table) and
// the filter, which both statements share. An empty page is an empty slice,
// not nil; the total is there all the same. columns and order are SQL
// text: constants of the caller (SQL).
func List[T any](q *gorm.DB, columns, order SQL, limit, offset int) (rows []T, total int64, err error) {
	// A session: each statement starts from q's conditions, not from the
	// other statement's.
	q = q.Session(&gorm.Session{})
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, &ListError{Count: true, Err: err}
	}
	page := q.Order(string(order)).Limit(limit).Offset(offset)
	if columns != "" {
		page = page.Select(string(columns))
	}
	rows = []T{} // Scan leaves it alone when no row comes back
	if err := page.Scan(&rows).Error; err != nil {
		return nil, 0, &ListError{Err: err}
	}
	return rows, total, nil
}

// ListError is a failure of List, in the count (Count) or in the page. Its
// text is the statement's error's: callers that word the two failures apart
// tell them by errors.As.
type ListError struct {
	Count bool
	Err   error
}

func (e *ListError) Error() string { return e.Err.Error() }

func (e *ListError) Unwrap() error { return e.Err }

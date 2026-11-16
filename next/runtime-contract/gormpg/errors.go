package gormpg

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// The errors of a statement are the driver's: the *pgconn.PgError the
// server sent, wrapped. Its SQLSTATE and constraint name are only on that
// type, so the core, the gateway and the plugin SDK read them through the
// functions below.

// IsNoRows reports a read that found no row: GORM's gorm.ErrRecordNotFound
// (First, Take, Last) and database/sql's sql.ErrNoRows (Row().Scan).
func IsNoRows(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, sql.ErrNoRows)
}

// IsUniqueViolation reports a unique violation (SQLSTATE 23505) in err's
// chain, on the constraint named constraint unless that is "". GORM's
// translated gorm.ErrDuplicatedKey is not enough: Raw(...).Row() and Rows()
// errors are never translated.
func IsUniqueViolation(err error, constraint string) bool {
	return SQLState(err) == "23505" && (constraint == "" || ConstraintName(err) == constraint)
}

// IsForeignKeyViolation reports a write that refers to a row that is not
// there (any more), SQLSTATE 23503.
func IsForeignKeyViolation(err error) bool { return SQLState(err) == "23503" }

// IsDeadlock reports a transaction the server aborted to break a deadlock
// (40P01); nothing of it was applied.
func IsDeadlock(err error) bool { return SQLState(err) == "40P01" }

// SQLState is the SQLSTATE code of a PostgreSQL error in err's chain, ""
// when there is none.
func SQLState(err error) string { return pgError(err).Code }

// ConstraintName is the constraint a PostgreSQL error in err's chain names,
// "" when there is none.
func ConstraintName(err error) string { return pgError(err).ConstraintName }

// Hint is the HINT of a PostgreSQL error in err's chain, "" when there is
// none.
func Hint(err error) string { return pgError(err).Hint }

// pgError is the PostgreSQL error in err's chain, the zero one (never
// written) when there is none.
func pgError(err error) *pgconn.PgError {
	if pg := (*pgconn.PgError)(nil); errors.As(err, &pg) {
		return pg
	}
	return &noPgError
}

var noPgError pgconn.PgError

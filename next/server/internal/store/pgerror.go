// PostgreSQL errors.
//
// This is the one file of the module that imports pgx (gormguard
// [pgx-import]), which go.mod therefore lists as a direct dependency: GORM
// runs on pgx's database/sql driver and hands back the *pgconn.PgError the
// server sent, and the SQLSTATE and constraint name are only on that type.
// Everything else reaches them through the functions below.

package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLState is the SQLSTATE code of a PostgreSQL error in err's chain, ""
// when there is none.
func SQLState(err error) string {
	if pg := pgError(err); pg != nil {
		return pg.Code
	}
	return ""
}

// ConstraintName is the constraint a PostgreSQL error in err's chain names,
// "" when there is none.
func ConstraintName(err error) string {
	if pg := pgError(err); pg != nil {
		return pg.ConstraintName
	}
	return ""
}

// IsUniqueViolation reports a 23505 error (optionally on a named constraint).
func IsUniqueViolation(err error, constraint string) bool {
	pg := pgError(err)
	return pg != nil && pg.Code == "23505" && (constraint == "" || pg.ConstraintName == constraint)
}

// IsForeignKeyViolation reports a write that refers to a row that is not
// there (any more).
func IsForeignKeyViolation(err error) bool { return SQLState(err) == "23503" }

// IsDeadlock reports a transaction the server aborted to break a deadlock
// (40P01); nothing of it was applied.
func IsDeadlock(err error) bool { return SQLState(err) == "40P01" }

func pgError(err error) *pgconn.PgError {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg
	}
	return nil
}

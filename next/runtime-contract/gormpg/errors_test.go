package gormpg

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func TestErrors(t *testing.T) {
	for _, err := range []error{gorm.ErrRecordNotFound, fmt.Errorf("read: %w", sql.ErrNoRows)} {
		if !IsNoRows(err) {
			t.Errorf("IsNoRows(%v) = false", err)
		}
	}
	unique := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", ConstraintName: "t_key"})
	if !IsUniqueViolation(unique, "") || !IsUniqueViolation(unique, "t_key") || IsUniqueViolation(unique, "other") {
		t.Error("IsUniqueViolation misreads a 23505 error")
	}
	if IsUniqueViolation(&pgconn.PgError{Code: "23503"}, "") || IsNoRows(unique) {
		t.Error("other errors match")
	}
	fk := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23503", Hint: "h"})
	if !IsForeignKeyViolation(fk) || IsDeadlock(fk) || Hint(fk) != "h" || ConstraintName(unique) != "t_key" {
		t.Error("the error fields are misread")
	}
	if SQLState(nil) != "" || ConstraintName(sql.ErrNoRows) != "" || IsDeadlock(nil) {
		t.Error("an error without a PostgreSQL error reads as one")
	}
}

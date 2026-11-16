package store

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// DDL statements (CREATE, ALTER, GRANT, COMMENT, DROP of schemas, roles and
// databases) take no parameters: their names and literals are part of the
// text. DDL writes that text from a constant format, whose %s verbs take
// DDLArg values, each quoted as what it is, so no input reaches the text
// unquoted and no caller joins SQL text with a variable.

// DDLArg is a value written into a DDL statement: an Ident or a Literal.
type DDLArg interface{ ddl() string }

// Ident is a name in a DDL statement, written as a quoted identifier
// (QuoteIdent).
type Ident string

func (i Ident) ddl() string { return QuoteIdent(string(i)) }

// Literal is a string constant in a DDL statement, written quoted, its
// quotes doubled; one with a backslash is an escape string (E'...'), its
// backslashes doubled too, so it reads the same whatever
// standard_conforming_strings is.
type Literal string

func (l Literal) ddl() string {
	s := strings.ReplaceAll(string(l), "'", "''")
	if !strings.Contains(s, `\`) {
		return "'" + s + "'"
	}
	return " E'" + strings.ReplaceAll(s, `\`, `\\`) + "'"
}

// DDL runs the statement of format (a constant) with its %s verbs replaced
// by args, quoted. It runs without arguments, so GORM sends the text as it
// is: a "?" or "@name" inside a literal stays text. Its error does not
// repeat the statement, which may hold a password.
func DDL(db *gorm.DB, format string, args ...DDLArg) error {
	parts := make([]any, len(args))
	for i, a := range args {
		parts[i] = a.ddl()
	}
	return db.Exec(fmt.Sprintf(format, parts...)).Error
}

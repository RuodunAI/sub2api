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

// DDLArg is a value written into a DDL statement: an Ident, a Literal,
// Privileges or a Keyword.
type DDLArg interface{ ddl() (string, error) }

// Keyword is a role keyword written where a role name (an Ident) may also
// stand: only Public and CurrentUser exist.
type Keyword struct{ word string }

var (
	Public      = Keyword{"PUBLIC"}
	CurrentUser = Keyword{"CURRENT_USER"}
)

func (k Keyword) ddl() (string, error) {
	if k.word == "" {
		return "", fmt.Errorf("empty keyword")
	}
	return k.word, nil
}

// Ident is a name in a DDL statement, written as a quoted identifier
// (QuoteIdent).
type Ident string

func (i Ident) ddl() (string, error) { return QuoteIdent(string(i)), nil }

// Literal is a string constant in a DDL statement, written quoted, its
// quotes doubled; one with a backslash is an escape string (E'...'), its
// backslashes doubled too, so it reads the same whatever
// standard_conforming_strings is.
type Literal string

func (l Literal) ddl() (string, error) {
	s := strings.ReplaceAll(string(l), "'", "''")
	if !strings.Contains(s, `\`) {
		return "'" + s + "'", nil
	}
	return " E'" + strings.ReplaceAll(s, `\`, `\\`) + "'", nil
}

// Privileges is the privilege list of a GRANT on a table: each of Names
// (select, insert, update, delete or references, in lower case), on
// Columns when there are any, written as the keywords and quoted column
// names. DELETE has no column form and always covers the whole table.
// Any other name is an error.
type Privileges struct{ Names, Columns []string }

// tablePrivileges are the keywords of Privileges.Names, and whether each
// takes a column list.
var tablePrivileges = map[string]struct {
	keyword string
	columns bool
}{
	"select": {"SELECT", true}, "insert": {"INSERT", true}, "update": {"UPDATE", true},
	"delete": {"DELETE", false}, "references": {"REFERENCES", true},
}

func (p Privileges) ddl() (string, error) {
	cols := make([]string, len(p.Columns))
	for i, c := range p.Columns {
		cols[i] = QuoteIdent(c)
	}
	list := make([]string, len(p.Names))
	for i, n := range p.Names {
		tp, ok := tablePrivileges[n]
		if !ok {
			return "", fmt.Errorf("unknown table privilege %q", n)
		}
		list[i] = tp.keyword
		if tp.columns && len(cols) > 0 {
			list[i] += " (" + strings.Join(cols, ", ") + ")"
		}
	}
	return strings.Join(list, ", "), nil
}

// DDL runs the statement of format (a constant) with its %s verbs replaced
// by args, quoted. It runs without arguments, so GORM sends the text as it
// is: a "?" or "@name" inside a literal stays text. Its error does not
// repeat the statement, which may hold a password.
func DDL(db *gorm.DB, format string, args ...DDLArg) error {
	parts := make([]any, len(args))
	for i, a := range args {
		text, err := a.ddl()
		if err != nil {
			return err
		}
		parts[i] = text
	}
	return db.Exec(fmt.Sprintf(format, parts...)).Error
}

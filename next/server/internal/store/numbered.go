package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Numbered is SQL written with pgx's numbered placeholders ($1, $2 ...),
// as Filter numbers its conditions and as a CTE statement names each
// parameter once however often it uses it, rewritten for GORM: each $n is
// a ? bound to the n-th argument, again wherever $n repeats; the text is
// sent otherwise unchanged. A $ inside a quoted literal ('...', where a
// doubled quote is a quote, and E'...', where a backslash also escapes one)
// is left alone, and a ? of the text (jsonb's key operator) is bound to
// itself, since GORM would read it as a placeholder. A []byte or
// json.RawMessage argument is bound as a Bytes, so that it stays one
// parameter wherever it stands (see Bytes).
//
// NewNumbered rewrites a statement once (a package-level statement, at
// start-up); RawNumbered and ExecNumbered rewrite SQL assembled per call.
// The SQL is the caller's own: a malformed placeholder, an argument a
// placeholder names but the call does not pass, or an @ (which makes GORM
// parse named parameters) is a programming error and panics.
type Numbered struct {
	sql   string
	binds []int // for each ? of sql, in order, the index of the argument it binds; literalBind binds the ? itself
}

// literalBind is the bind of a ? that the SQL itself holds.
const literalBind = -1

// literalQuestionMark is the binding of a ? that the SQL holds: an
// expression GORM writes out as is.
var literalQuestionMark = clause.Expr{SQL: "?"}

// NewNumbered rewrites query for GORM (see Numbered). With names, a
// placeholder may also name its argument: $name binds the argument at
// name's position in names, and each name must be used. That lets SQL
// assembled from shared fragments name a parameter which the statements
// built from them pass in different positions.
func NewNumbered(query string, names ...string) Numbered {
	if strings.Contains(query, "@") {
		panic("store: numbered SQL holds an @, which GORM reads as a named parameter")
	}
	var b strings.Builder
	b.Grow(len(query))
	var n Numbered
	used := make([]bool, len(names))
	// quoted is inside a string literal; escapes is that literal an E'...'
	// one, where a backslash escapes the next byte. '' inside a literal
	// closes and reopens it, which leaves both as they were.
	quoted, escapes := false, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '?':
			b.WriteByte('?')
			n.binds = append(n.binds, literalBind)
			continue
		case c == '\\' && quoted && escapes:
			b.WriteByte(c)
			// The escaped byte cannot end the literal; a ? is still bound.
			if i+1 < len(query) && query[i+1] != '?' {
				i++
				b.WriteByte(query[i])
			}
			continue
		case c == '\'':
			if !quoted && (i == 0 || query[i-1] != '\'') {
				escapes = escapeLiteral(query, i)
			}
			quoted = !quoted
		}
		if c != '$' || quoted {
			b.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(query) && isWordByte(query[j]) {
			j++
		}
		arg := placeholderArg(query[i+1:j], names)
		if arg < 0 {
			panic(fmt.Sprintf("store: malformed placeholder %q in numbered SQL", query[i:j]))
		}
		b.WriteByte('?')
		n.binds = append(n.binds, arg)
		if arg < len(used) {
			used[arg] = true
		}
		i = j - 1
	}
	for i, name := range names {
		if !used[i] {
			panic(fmt.Sprintf("store: numbered SQL does not use $%s", name))
		}
	}
	n.sql = b.String()
	return n
}

// placeholderArg is the index of the argument that the placeholder $p binds
// (p without its $), or -1 for none: $n is argument n-1, and with names
// $name is the argument at name's position.
func placeholderArg(p string, names []string) int {
	if num, err := strconv.Atoi(p); err == nil {
		if num < 1 {
			return -1
		}
		return num - 1
	}
	return slices.Index(names, p)
}

// SQL is the rewritten text, with a ? for each binding.
func (n Numbered) SQL() string { return n.sql }

// Vars are the bindings of the text's ? for args ($1 is args[0]).
func (n Numbered) Vars(args ...any) []any {
	vars := make([]any, len(n.binds))
	for i, a := range n.binds {
		switch {
		case a == literalBind:
			vars[i] = literalQuestionMark
		case a >= len(args):
			panic(fmt.Sprintf("store: placeholder $%d of numbered SQL has no argument (%d given)", a+1, len(args)))
		default:
			vars[i] = bindable(args[a])
		}
	}
	return vars
}

// Raw is db.Raw of the statement with args, not yet run.
func (n Numbered) Raw(db *gorm.DB, args ...any) *gorm.DB { return db.Raw(n.sql, n.Vars(args...)...) }

// Exec runs the statement with args on db.
func (n Numbered) Exec(db *gorm.DB, args ...any) *gorm.DB { return db.Exec(n.sql, n.Vars(args...)...) }

// RawNumbered is db.Raw for numbered SQL assembled per call (see Numbered).
func RawNumbered(db *gorm.DB, query string, args ...any) *gorm.DB {
	return NewNumbered(query).Raw(db, args...)
}

// ExecNumbered is db.Exec for numbered SQL assembled per call.
func ExecNumbered(db *gorm.DB, query string, args ...any) *gorm.DB {
	return NewNumbered(query).Exec(db, args...)
}

// escapeLiteral reports whether the quote at query[i] opens an E'...'
// literal: an E or e right before it that does not end a longer word.
func escapeLiteral(query string, i int) bool {
	if i == 0 || (query[i-1] != 'E' && query[i-1] != 'e') {
		return false
	}
	return i == 1 || !isWordByte(query[i-2])
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// bindable is v as GORM binds it as one parameter: GORM spreads a []byte
// or json.RawMessage after "(" and turns an empty json.RawMessage into NULL.
func bindable(v any) any {
	switch b := v.(type) {
	case []byte:
		return Bytes(b)
	case json.RawMessage:
		return Bytes(b)
	}
	return v
}

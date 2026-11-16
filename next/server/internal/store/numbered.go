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

// Numbered is SQL written with PostgreSQL's numbered placeholders ($1,
// $2 ...), as a CTE statement names each parameter once however often it
// uses it, rewritten for GORM: each $n is a ? bound to the n-th argument,
// again wherever $n repeats; the text is sent otherwise unchanged. A $ is
// left alone inside what is no SQL code - a string literal ('...', where a
// doubled quote is a quote, and E'...', where a backslash also escapes
// one), a quoted identifier ("..."), a dollar-quoted string ($$...$$,
// $tag$...$tag$), a comment (-- to the end of the line, /* ... */, which
// nest) - and a ? anywhere (jsonb's key operator, or one of those) is bound
// to itself, since GORM would read it as a placeholder. A []byte or
// json.RawMessage argument is bound as a Bytes, so that it stays one
// parameter wherever it stands (see Bytes).
//
// NewNumbered rewrites a statement once (a package-level statement, at
// start-up). The SQL is the caller's own: a malformed placeholder, an
// argument a placeholder names but the call does not pass, a literal,
// identifier or comment left open, or an @ (which makes GORM parse named
// parameters) is a programming error and panics.
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
	for i := 0; i < len(query); {
		if end := skipEnd(query, i); end > i {
			n.writeText(&b, query[i:end])
			i = end
			continue
		}
		if query[i] != '$' {
			n.writeText(&b, query[i:i+1])
			i++
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
		i = j
	}
	for i, name := range names {
		if !used[i] {
			panic(fmt.Sprintf("store: numbered SQL does not use $%s", name))
		}
	}
	n.sql = b.String()
	return n
}

// writeText writes text, which holds no placeholder, binding each ? in it
// to itself.
func (n *Numbered) writeText(b *strings.Builder, text string) {
	b.WriteString(text)
	for range strings.Count(text, "?") {
		n.binds = append(n.binds, literalBind)
	}
}

// skipEnd is the end of the literal, quoted identifier, dollar-quoted
// string or comment that starts at query[i], or i when none does; one left
// open panics.
func skipEnd(query string, i int) int {
	rest := query[i:]
	switch {
	case rest[0] == '\'':
		return i + quotedLen(rest, '\'', escapeLiteral(query, i))
	case rest[0] == '"':
		return i + quotedLen(rest, '"', false)
	case strings.HasPrefix(rest, "--"):
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			return i + nl + 1
		}
		return len(query)
	case strings.HasPrefix(rest, "/*"):
		return i + blockCommentLen(rest)
	case rest[0] == '$':
		tag, ok := dollarTag(rest)
		if !ok {
			return i
		}
		end := strings.Index(rest[len(tag):], tag)
		if end < 0 {
			panic("store: unterminated dollar-quoted string in numbered SQL")
		}
		return i + len(tag) + end + len(tag)
	}
	return i
}

// quotedLen is the length of the text quoted by q at the start of s,
// quotes included: a doubled q is one, and with escapes a backslash
// escapes the next byte.
func quotedLen(s string, q byte, escapes bool) int {
	for j := 1; j < len(s); j++ {
		switch {
		case escapes && s[j] == '\\':
			j++
		case s[j] != q:
		case j+1 < len(s) && s[j+1] == q:
			j++
		default:
			return j + 1
		}
	}
	panic(fmt.Sprintf("store: unterminated %c in numbered SQL", q))
}

// blockCommentLen is the length of the /* comment */ at the start of s;
// PostgreSQL nests them.
func blockCommentLen(s string) int {
	depth := 0
	for j := 0; j+1 < len(s); j++ {
		switch s[j : j+2] {
		case "/*":
			depth++
			j++
		case "*/":
			if depth--; depth == 0 {
				return j + 2
			}
			j++
		}
	}
	panic("store: unterminated /* comment in numbered SQL")
}

// dollarTag is the opening $tag$ (or $$) of a dollar-quoted string at the
// start of s: a tag starts like an identifier, so $1 is no tag.
func dollarTag(s string) (string, bool) {
	j := 1
	for j < len(s) && isWordByte(s[j]) {
		j++
	}
	if j == len(s) || s[j] != '$' || j > 1 && s[1] >= '0' && s[1] <= '9' {
		return "", false
	}
	return s[:j+1], true
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

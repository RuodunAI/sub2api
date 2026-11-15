package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// rawNumbered is db.Raw for SQL whose placeholders are numbered for pgx ($1,
// $2 ...), as store.Filter and statRows number them. Each $n becomes a ?
// bound to args[n-1], so a number used more than once (statRows repeats the
// filter in every part of its UNION) binds its value again; the text is sent
// otherwise unchanged. A $ inside a quoted literal is left alone, and a ? of
// the text (jsonb's key operator, in the rollup's measures) is bound to
// itself, since GORM would read it as a placeholder. The SQL is the
// package's own: a malformed placeholder, or an @ (which makes GORM parse
// named parameters), is a programming error and panics.
func rawNumbered(db *gorm.DB, query string, args []any) *gorm.DB {
	sql, vars := numbered(query, args)
	return db.Raw(sql, vars...)
}

// execNumbered is db.Exec for SQL numbered as rawNumbered's is.
func execNumbered(db *gorm.DB, query string, args []any) *gorm.DB {
	sql, vars := numbered(query, args)
	return db.Exec(sql, vars...)
}

// numbered rewrites query for GORM as rawNumbered describes: the SQL and
// the bindings of its placeholders.
func numbered(query string, args []any) (string, []any) {
	if strings.Contains(query, "@") {
		panic("usage: rawNumbered SQL holds an @, which GORM reads as a named parameter")
	}
	var b strings.Builder
	b.Grow(len(query))
	vars := make([]any, 0, len(args))
	quoted := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '\'' {
			quoted = !quoted
		}
		if c == '?' {
			b.WriteByte('?')
			vars = append(vars, literalQuestionMark)
			continue
		}
		if c != '$' || quoted {
			b.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(query) && query[j] >= '0' && query[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(query[i+1 : j])
		if err != nil || n < 1 || n > len(args) {
			panic(fmt.Sprintf("usage: placeholder %q of rawNumbered has no argument (%d given)", query[i:j], len(args)))
		}
		b.WriteByte('?')
		vars = append(vars, args[n-1])
		i = j - 1
	}
	return b.String(), vars
}

// literalQuestionMark is the binding of a ? that rawNumbered's SQL holds: an
// expression GORM writes out as is.
var literalQuestionMark = clause.Expr{SQL: "?"}

// nullRaw scans a json, jsonb or text column into a json.RawMessage through
// database/sql, which refuses NULL for a byte slice type other than []byte:
// NULL reads as nil, as it did with pgx.
type nullRaw struct{ dst *json.RawMessage }

// Scan implements sql.Scanner.
func (n nullRaw) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n.dst = nil
	case []byte:
		*n.dst = bytes.Clone(v)
	case string:
		*n.dst = json.RawMessage(v)
	default:
		return fmt.Errorf("usage: cannot scan %T into json.RawMessage", src)
	}
	return nil
}

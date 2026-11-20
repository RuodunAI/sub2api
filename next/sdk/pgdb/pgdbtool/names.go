package pgdbtool

import (
	"fmt"
	"strings"
)

// maxIdentifier is how many bytes of a name PostgreSQL keeps (NAMEDATALEN
// - 1); it cuts a longer one without an error.
const maxIdentifier = 63

// longNames lists the names in a migration's text longer than
// maxIdentifier, for DriftReport.Refused: PostgreSQL would give the object
// another name than the one written, and the catalog shows only the cut
// one.
func longNames(sql string) []string {
	var out []string
	for _, name := range identifiers(sql) {
		if len(name) > maxIdentifier {
			out = append(out, fmt.Sprintf("identifier %s: %d bytes, PostgreSQL keeps %d", name, len(name), maxIdentifier))
		}
	}
	return out
}

// identifiers returns the words and quoted identifiers of SQL text, outside
// comments and string literals ('...', and E'...' with its backslash
// escapes). A dollar-quoted body is read as code, not skipped as a string:
// with functions refused, it is a DO block (db diff writes one around each
// new constraint), whose statements name objects as the file does. A name
// a DO block builds in a string for EXECUTE is not seen. Keywords are among
// the words: none is near maxIdentifier.
func identifiers(sql string) []string {
	var out []string
	for i := 0; i < len(sql); {
		switch c := sql[i]; {
		case strings.HasPrefix(sql[i:], "--"):
			i = after(sql, i+2, "\n")
		case strings.HasPrefix(sql[i:], "/*"):
			i = after(sql, i+2, "*/")
		case c == '\'':
			i = afterQuoted(sql, i, '\'')
		case c == '"':
			end := afterQuoted(sql, i, '"')
			out = append(out, strings.ReplaceAll(strings.TrimSuffix(sql[i+1:end], `"`), `""`, `"`))
			i = end
		case (c == 'E' || c == 'e') && strings.HasPrefix(sql[i+1:], "'") && (i == 0 || !identPart(sql[i-1])):
			i = afterEscaped(sql, i+1)
		case identStart(c):
			j := i + 1
			for j < len(sql) && identPart(sql[j]) {
				j++
			}
			out = append(out, sql[i:j])
			i = j
		default:
			i++
		}
	}
	return out
}

func identStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}

func identPart(c byte) bool { return identStart(c) || c >= '0' && c <= '9' || c == '$' }

// after is the index after the first end at or after i, or len(s).
func after(s string, i int, end string) int {
	if n := strings.Index(s[i:], end); n >= 0 {
		return i + n + len(end)
	}
	return len(s)
}

// afterEscaped is the index after the quote that closes the escape string
// literal whose quote is at i (a backslash escapes the next byte, a doubled
// quote is one), or len(s).
func afterEscaped(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\\':
			j++
		case s[j] == '\'' && j+1 < len(s) && s[j+1] == '\'':
			j++
		case s[j] == '\'':
			return j + 1
		}
	}
	return len(s)
}

// afterQuoted is the index after the quote q that closes the one at i (a
// doubled q is one character of the text), or len(s).
func afterQuoted(s string, i int, q byte) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1
	}
	return len(s)
}

package pgdbtool

import (
	"fmt"
	"os"
	"strings"
)

// maxIdentifier is how many bytes of a name PostgreSQL keeps (NAMEDATALEN
// - 1); it cuts a longer one without an error.
const maxIdentifier = 63

// longNames lists the names in migrations longer than maxIdentifier, as
// DriftReport.Misnamed: PostgreSQL would give the object another name than
// the one written, and the catalog shows only the cut one.
func longNames(migrations []Migration) ([]string, error) {
	var out []string
	for _, m := range migrations {
		text, err := os.ReadFile(m.Path)
		if err != nil {
			return nil, fmt.Errorf("pgdbtool: read %s: %w", m.Name, err)
		}
		for _, name := range identifiers(string(text)) {
			if len(name) > maxIdentifier {
				out = append(out, fmt.Sprintf("identifier %s in %s: %d bytes, PostgreSQL keeps %d", name, m.Name, len(name), maxIdentifier))
			}
		}
	}
	return out, nil
}

// identifiers returns the words and quoted identifiers of SQL text, outside
// comments and string literals. Dollar-quoted bodies are code (DO blocks,
// functions) and are read as such. Keywords are among the words: none is
// near maxIdentifier.
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
		case identStart(c):
			j := i + 1
			for j < len(sql) && (identStart(sql[j]) || sql[j] >= '0' && sql[j] <= '9' || sql[j] == '$') {
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

// after is the index after the first end at or after i, or len(s).
func after(s string, i int, end string) int {
	if n := strings.Index(s[i:], end); n >= 0 {
		return i + n + len(end)
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

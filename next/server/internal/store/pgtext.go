package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The text forms of PostgreSQL arrays and timestamps. database/sql hands
// over every array column as its text ("{1,2}", {"a b","c"}), and pgx's
// driver a timestamptz it cannot turn into a time.Time (±infinity) as text,
// so the store types parse them here; they write arrays in the same form.

// parseArray calls elem with each element of src, the text form of an
// array, in order; null marks an unquoted NULL. A multi-dimensional array
// is read flat, row by row, and a dimension prefix ("[0:1]={...}") is
// skipped, as pgx's flat arrays read them.
func parseArray(src string, elem func(s string, null bool) error) error {
	p := arrayParser{s: src}
	if strings.HasPrefix(p.s, "[") {
		eq := strings.IndexByte(p.s, '=')
		if eq < 0 {
			return p.fail()
		}
		p.i = eq + 1
	}
	if err := p.array(elem); err != nil {
		return err
	}
	if p.skipBlanks(); p.i != len(p.s) {
		return p.fail()
	}
	return nil
}

type arrayParser struct {
	s string
	i int
}

func (p *arrayParser) fail() error { return fmt.Errorf("store: invalid array text %q", p.s) }

func (p *arrayParser) skipBlanks() {
	for p.i < len(p.s) && isBlank(p.s[p.i]) {
		p.i++
	}
}

// eat consumes c, after blanks, if it comes next.
func (p *arrayParser) eat(c byte) bool {
	p.skipBlanks()
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *arrayParser) array(elem func(string, bool) error) error {
	if !p.eat('{') {
		return p.fail()
	}
	if p.eat('}') {
		return nil
	}
	for {
		var err error
		if p.skipBlanks(); p.i < len(p.s) && p.s[p.i] == '{' {
			err = p.array(elem)
		} else {
			err = p.element(elem)
		}
		switch {
		case err != nil:
			return err
		case p.eat('}'):
			return nil
		case !p.eat(','):
			return p.fail()
		}
	}
}

// element reads one element: double-quoted, or running to the next "," or
// "}" with blanks around it dropped. A backslash takes the next character
// literally in both.
func (p *arrayParser) element(elem func(string, bool) error) error {
	if p.i == len(p.s) {
		return p.fail()
	}
	quoted := p.s[p.i] == '"'
	if quoted {
		p.i++
	}
	start, escaped := p.i, false
	var b []byte
	for ; p.i < len(p.s); p.i++ {
		c := p.s[p.i]
		switch {
		case c == '\\' && p.i+1 < len(p.s):
			if !escaped {
				b, escaped = append(b, p.s[start:p.i]...), true
			}
			p.i++
			b = append(b, p.s[p.i])
			continue
		case quoted && c == '"':
			p.i++
			return elem(p.text(b, escaped, start, p.i-1), false)
		case !quoted && (c == ',' || c == '}'):
			s := p.text(b, escaped, start, p.i)
			if !escaped {
				s = strings.TrimRight(s, " \t\n\r\v\f")
			}
			if s == "" {
				return p.fail()
			}
			return elem(s, !escaped && strings.EqualFold(s, "NULL"))
		}
		if escaped {
			b = append(b, c)
		}
	}
	return p.fail()
}

// text is the element read from start to end: the source text itself
// unless an escape made a copy (b).
func (p *arrayParser) text(b []byte, escaped bool, start, end int) string {
	if escaped {
		return string(b)
	}
	return p.s[start:end]
}

// errNullElement is an array with a NULL element read into a Go slice of
// values, which has no NULL to give it.
var errNullElement = errors.New("store: array has a NULL element")

// scanArray reads src, an array column as database/sql gives it (text, as
// string or []byte, or NULL), into a slice of the elements parse returns.
// NULL reads as a nil slice and the empty array as an empty one.
func scanArray[E any](src any, parse func(string) (E, error)) ([]E, error) {
	var text string
	switch v := src.(type) {
	case nil:
		return nil, nil
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return nil, fmt.Errorf("store: cannot scan %T into an array", src)
	}
	out := []E{}
	err := parseArray(text, func(s string, null bool) error {
		if null {
			return errNullElement
		}
		e, err := parse(s)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// appendArray writes a as an array's text form, each element written by
// add.
func appendArray[E any](a []E, add func([]byte, E) []byte) string {
	b := make([]byte, 0, 2+len(a)*8)
	b = append(b, '{')
	for i, e := range a {
		if i > 0 {
			b = append(b, ',')
		}
		b = add(b, e)
	}
	return string(append(b, '}'))
}

// appendQuoted writes s as a double-quoted array element.
func appendQuoted(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return append(b, '"')
}

// timestampLayout writes a time PostgreSQL reads as the same instant
// (Z is UTC), to the microsecond it stores.
const timestampLayout = "2006-01-02 15:04:05.999999Z07:00"

// appendTimestamp writes t as a quoted array element. The sub-microsecond
// part is dropped, as pgx's binary encoding drops it, rather than rounded
// by the server.
func appendTimestamp(b []byte, t time.Time) []byte {
	b = append(b, '"')
	b = t.UTC().Truncate(time.Microsecond).AppendFormat(b, timestampLayout)
	return append(b, '"')
}

// parseTimestamptz reads a finite timestamptz as PostgreSQL prints it with
// DateStyle ISO: "2024-05-01 10:00:00.123456+08", the offset with optional
// minutes and seconds, a year of four digits or more and an optional " BC".
// The time is in time.Local, as pgx returns timestamptz values.
func parseTimestamptz(s string) (time.Time, error) {
	bad := func() (time.Time, error) { return time.Time{}, fmt.Errorf("store: invalid timestamptz %q", s) }
	src := s
	bc := strings.HasSuffix(s, " BC")
	s = strings.TrimSuffix(s, " BC")
	date, clock, ok := strings.Cut(s, " ")
	if !ok {
		return bad()
	}
	ymd := strings.SplitN(date, "-", 3)
	if len(ymd) != 3 || len(ymd[0]) < 4 {
		return bad()
	}
	year, e1 := strconv.Atoi(ymd[0])
	month, e2 := strconv.Atoi(ymd[1])
	day, e3 := strconv.Atoi(ymd[2])
	off := strings.IndexAny(clock, "+-")
	if e1 != nil || e2 != nil || e3 != nil || off < 0 {
		return bad()
	}
	hms, frac, _ := strings.Cut(clock[:off], ".")
	parts := strings.Split(hms, ":")
	if len(parts) != 3 {
		return bad()
	}
	var tod [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || len(part) != 2 || n > []int{23, 59, 59}[i] {
			return bad()
		}
		tod[i] = n
	}
	nsec := 0
	if frac != "" {
		if len(frac) > 9 {
			frac = frac[:9]
		}
		n, err := strconv.Atoi(frac + strings.Repeat("0", 9-len(frac)))
		if err != nil {
			return bad()
		}
		nsec = n
	}
	zone, err := parseOffset(clock[off:])
	if err != nil {
		return time.Time{}, fmt.Errorf("store: invalid timestamptz %q", src)
	}
	if bc {
		year = 1 - year
	}
	t := time.Date(year, time.Month(month), day, tod[0], tod[1], tod[2], nsec, time.FixedZone("", zone))
	if t.Month() != time.Month(month) || t.Day() != day {
		return bad() // time.Date normalized an out-of-range field
	}
	return t.In(time.Local), nil
}

// parseOffset reads a UTC offset, "+08", "-03:30" or "+00:53:28", in
// seconds east of UTC.
func parseOffset(s string) (int, error) {
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	secs := 0
	for i, part := range strings.Split(s[1:], ":") {
		n, err := strconv.Atoi(part)
		if err != nil || len(part) != 2 || i > 2 {
			return 0, errors.New("invalid offset")
		}
		secs += n * []int{3600, 60, 1}[i]
	}
	return sign * secs, nil
}

// isBlank reports the blanks PostgreSQL skips around array elements.
func isBlank(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

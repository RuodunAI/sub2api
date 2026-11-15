// Package pgdsn separates pgxpool's pool_* parameters from a PostgreSQL DSN.
//
// One DATABASE_URL is shared by processes on different drivers: the core's
// pgxpool reads pool_max_conns and its siblings, while database/sql (GORM)
// and a plain pgx connection would send them to PostgreSQL as settings,
// which it rejects (FATAL: unrecognized configuration parameter). Split
// takes them out of either DSN form and Pool.Apply maps what database/sql
// can express onto a *sql.DB. Standard library only, so any module can
// depend on it.
package pgdsn

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Pool is the pool_* parameters of a DSN; a nil field was not set.
type Pool struct {
	MaxConns              *int
	MinConns              *int
	MinIdleConns          *int
	MaxConnLifetime       *time.Duration
	MaxConnIdleTime       *time.Duration
	HealthCheckPeriod     *time.Duration
	MaxConnLifetimeJitter *time.Duration
}

// params are pgxpool's DSN parameters (pgxpool.ParseConfig), each with the
// parser that stores it. Other keys, pool_* or not, stay in the DSN.
var params = map[string]func(p *Pool, v string) error{
	"pool_max_conns":                count(func(p *Pool) **int { return &p.MaxConns }, 1),
	"pool_min_conns":                count(func(p *Pool) **int { return &p.MinConns }, 0),
	"pool_min_idle_conns":           count(func(p *Pool) **int { return &p.MinIdleConns }, 0),
	"pool_max_conn_lifetime":        duration(func(p *Pool) **time.Duration { return &p.MaxConnLifetime }),
	"pool_max_conn_idle_time":       duration(func(p *Pool) **time.Duration { return &p.MaxConnIdleTime }),
	"pool_health_check_period":      duration(func(p *Pool) **time.Duration { return &p.HealthCheckPeriod }),
	"pool_max_conn_lifetime_jitter": duration(func(p *Pool) **time.Duration { return &p.MaxConnLifetimeJitter }),
}

func count(field func(*Pool) **int, least int) func(*Pool, string) error {
	return func(p *Pool, v string) error {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || int(n) < least {
			return fmt.Errorf("must be an integer of at least %d", least)
		}
		i := int(n)
		*field(p) = &i
		return nil
	}
}

func duration(field func(*Pool) **time.Duration) func(*Pool, string) error {
	return func(p *Pool, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return errors.New("must be a non-negative duration such as 30m")
		}
		*field(p) = &d
		return nil
	}
}

// set parses one parameter into p; known reports whether key is a pool one.
// Errors name the key and value, never the DSN: it carries the password.
func (p *Pool) set(key, value string) (known bool, err error) {
	parse, known := params[key]
	if !known {
		return false, nil
	}
	if err := parse(p, value); err != nil {
		return true, fmt.Errorf("pgdsn: %s=%q %w", key, value, err)
	}
	return true, nil
}

// Split returns dsn without its pool_* parameters, and those parameters.
// dsn is a URL (postgres:// or postgresql://) or keyword=value pairs, the two
// forms libpq and pgx accept. What is not a pool parameter is kept as written.
func Split(dsn string) (string, Pool, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return splitURL(dsn)
	}
	return splitKeywordValue(dsn)
}

func splitURL(dsn string) (string, Pool, error) {
	var p Pool
	base, rest, hasQuery := strings.Cut(dsn, "?")
	if !hasQuery {
		return dsn, p, nil
	}
	query, fragment, hasFragment := strings.Cut(rest, "#")
	var kept []string
	for _, pair := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(pair, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			return "", Pool{}, errors.New("pgdsn: invalid query in the database URL")
		}
		known, err := p.set(key, value)
		if err != nil {
			return "", Pool{}, err
		}
		if !known && pair != "" {
			kept = append(kept, pair)
		}
	}
	out := base
	if len(kept) > 0 {
		out += "?" + strings.Join(kept, "&")
	}
	if hasFragment {
		out += "#" + fragment
	}
	return out, p, nil
}

// splitKeywordValue follows libpq's (and pgconn's) keyword=value syntax:
// blanks around "=", a value either single quoted or running to the next
// blank, and a backslash escaping the next character in both. The pairs
// that stay are copied byte for byte.
func splitKeywordValue(dsn string) (string, Pool, error) {
	var p Pool
	var kept []string
	s := strings.TrimLeft(dsn, blanks)
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return "", Pool{}, errors.New("pgdsn: invalid keyword/value database DSN")
		}
		key := strings.Trim(s[:eq], blanks)
		rest := strings.TrimLeft(s[eq+1:], blanks)
		n, err := valueLen(rest)
		if err != nil {
			return "", Pool{}, err
		}
		known, err := p.set(key, unescape(rest[:n]))
		if err != nil {
			return "", Pool{}, err
		}
		if !known {
			kept = append(kept, key+"="+rest[:n])
		}
		s = strings.TrimLeft(rest[n:], blanks)
	}
	return strings.Join(kept, " "), p, nil
}

// blanks are the separators of the keyword=value form (C isspace).
const blanks = " \t\n\r\v\f"

// valueLen is the length of the value at the start of s, quotes included.
func valueLen(s string) (int, error) {
	quoted := s != "" && s[0] == '\''
	i := 0
	if quoted {
		i = 1
	}
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			if i++; i == len(s) {
				return 0, errors.New("pgdsn: invalid backslash in the database DSN")
			}
		case quoted && c == '\'':
			return i + 1, nil
		case !quoted && strings.IndexByte(blanks, c) >= 0:
			return i, nil
		}
	}
	if quoted {
		return 0, errors.New("pgdsn: unterminated quoted string in the database DSN")
	}
	return i, nil
}

// unescape drops the quotes and backslashes of a value valueLen measured.
func unescape(raw string) string {
	if len(raw) >= 2 && raw[0] == '\'' {
		raw = raw[1 : len(raw)-1]
	}
	if !strings.Contains(raw, `\`) {
		return raw
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}

// Defaults are the pool settings of a handle whose DSN sets none.
type Defaults struct {
	MaxConns        int
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// Apply sizes db from p, falling back to def. Like pgxpool, every connection
// may stay idle (database/sql would keep two and redial for every burst).
// The parameters database/sql has no counterpart for - a floor of open or
// idle connections, the health check period, the lifetime jitter - are
// logged to log (nil: slog.Default) as ignored, once.
func (p Pool) Apply(db *sql.DB, def Defaults, log *slog.Logger) {
	maxConns := orDefault(p.MaxConns, def.MaxConns)
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(orDefault(p.MaxConnLifetime, def.MaxConnLifetime))
	db.SetConnMaxIdleTime(orDefault(p.MaxConnIdleTime, def.MaxConnIdleTime))

	var ignored []string
	for key, set := range map[string]bool{
		"pool_min_conns":                p.MinConns != nil,
		"pool_min_idle_conns":           p.MinIdleConns != nil,
		"pool_health_check_period":      p.HealthCheckPeriod != nil,
		"pool_max_conn_lifetime_jitter": p.MaxConnLifetimeJitter != nil,
	} {
		if set {
			ignored = append(ignored, key)
		}
	}
	if len(ignored) == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	slices.Sort(ignored)
	log.Warn("database DSN pool parameters ignored: database/sql has no equivalent", "params", ignored)
}

func orDefault[T any](v *T, def T) T {
	if v != nil {
		return *v
	}
	return def
}

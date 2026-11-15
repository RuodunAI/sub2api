package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The pool settings a DSN may carry, as pgxpool read them before the pool
// moved to database/sql. pgx's connection parser would send them to
// PostgreSQL as session settings, which it rejects, so takePoolParams
// removes every pool_* parameter and keeps the ones database/sql has a
// counterpart for.
const (
	// defaultPoolMaxConns is the pool size when neither
	// SUB2API_PG_MAX_CONNS nor pool_max_conns sets one.
	defaultPoolMaxConns = 20
	// pgxpool's defaults, kept so that a DSN without the parameters behaves
	// as before.
	defaultMaxConnLifetime = time.Hour
	defaultMaxConnIdleTime = 30 * time.Minute
)

// poolConfig is the sizing of one pool. maxConns 0 means unset.
type poolConfig struct {
	maxConns    int
	minConns    int // logged only: database/sql neither opens connections ahead nor keeps a floor
	maxLifetime time.Duration
	maxIdleTime time.Duration
}

// apply sizes sqlDB. Idle connections are kept up to the pool size, as
// pgxpool kept them, rather than database/sql's default of two, which would
// close and reopen connections under every burst.
func (pc poolConfig) apply(sqlDB *sql.DB) {
	sqlDB.SetMaxOpenConns(pc.maxConns)
	sqlDB.SetMaxIdleConns(pc.maxConns)
	sqlDB.SetConnMaxLifetime(pc.maxLifetime)
	sqlDB.SetConnMaxIdleTime(pc.maxIdleTime)
}

// poolMaxConns picks the pool size: SUB2API_PG_MAX_CONNS (AR-P1-2) first,
// then pool_max_conns of the DSN (fromDSN, 0 when absent), else
// defaultPoolMaxConns. 4 connections shared by the gateway and the console
// made every list request queue for a connection per statement; a
// plugin-heavy or multi-node setup may need more.
func poolMaxConns(env string, fromDSN int) int {
	if n, err := strconv.Atoi(env); err == nil && n > 0 {
		return n
	}
	if fromDSN > 0 {
		return fromDSN
	}
	return defaultPoolMaxConns
}

// takePoolParams removes the pool_* parameters from dsn, a URL or a
// keyword/value DSN, and returns them as a poolConfig over the defaults.
func takePoolParams(dsn string) (string, poolConfig, error) {
	pc := poolConfig{maxLifetime: defaultMaxConnLifetime, maxIdleTime: defaultMaxConnIdleTime}
	if !strings.Contains(dsn, "pool_") {
		return dsn, pc, nil
	}
	var params map[string]string
	var err error
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		dsn, params, err = takeURLParams(dsn)
	} else {
		dsn, params, err = takeKeywordParams(dsn)
	}
	if err != nil {
		return "", pc, err
	}
	for k, v := range params {
		if err := pc.set(k, v); err != nil {
			return "", pc, err
		}
	}
	return dsn, pc, nil
}

// set reads one pool_* parameter; the ones database/sql has no counterpart
// for (pool_health_check_period, pool_min_idle_conns,
// pool_max_conn_lifetime_jitter) are dropped.
func (pc *poolConfig) set(key, val string) error {
	var err error
	switch key {
	case "pool_max_conns":
		pc.maxConns, err = positive(val)
	case "pool_min_conns":
		pc.minConns, err = strconv.Atoi(val)
	case "pool_max_conn_lifetime":
		pc.maxLifetime, err = time.ParseDuration(val)
	case "pool_max_conn_idle_time":
		pc.maxIdleTime, err = time.ParseDuration(val)
	}
	if err != nil {
		return fmt.Errorf("parse database url: invalid %s %q", key, val)
	}
	return nil
}

func positive(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err == nil && n < 1 {
		err = errors.New("not positive")
	}
	return n, err
}

// takeURLParams removes the pool_* query parameters of a URL DSN.
func takeURLParams(dsn string) (string, map[string]string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", nil, errors.New("parse database url: invalid URL") // the URL carries the password
	}
	q := u.Query()
	params := map[string]string{}
	for k := range q {
		if strings.HasPrefix(k, "pool_") {
			params[k] = q.Get(k)
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), params, nil
}

// takeKeywordParams removes the pool_* settings of a keyword/value DSN
// (host=h pool_max_conns=5 password='a b'), keeping the text of the others
// as written.
func takeKeywordParams(dsn string) (string, map[string]string, error) {
	params := map[string]string{}
	var kept []string
	for s := strings.TrimLeft(dsn, " \t\n\r\v\f"); s != ""; s = strings.TrimLeft(s, " \t\n\r\v\f") {
		key, val, n, err := nextKeyword(s)
		if err != nil {
			return "", nil, err
		}
		if strings.HasPrefix(key, "pool_") {
			params[key] = val
		} else {
			kept = append(kept, s[:n])
		}
		s = s[n:]
	}
	return strings.Join(kept, " "), params, nil
}

// nextKeyword reads the setting s starts with, as libpq does: a key, "="
// with optional blanks around it, and a value that is either single-quoted
// (\' and \\ escaped) or runs to the next blank (backslash escapes the next
// character). n is the length of the setting's text.
func nextKeyword(s string) (key, val string, n int, err error) {
	eq := strings.IndexByte(s, '=')
	if eq < 1 {
		return "", "", 0, errors.New("parse database url: invalid keyword/value DSN")
	}
	key = strings.TrimSpace(s[:eq])
	i := eq + 1
	for i < len(s) && isBlank(s[i]) {
		i++
	}
	var b strings.Builder
	quoted := i < len(s) && s[i] == '\''
	if quoted {
		i++
	}
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case quoted && c == '\'':
			return key, b.String(), i + 1, nil
		case !quoted && isBlank(c):
			return key, b.String(), i, nil
		default:
			b.WriteByte(c)
		}
	}
	if quoted {
		return "", "", 0, errors.New("parse database url: unterminated quoted value")
	}
	return key, b.String(), i, nil
}

func isBlank(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

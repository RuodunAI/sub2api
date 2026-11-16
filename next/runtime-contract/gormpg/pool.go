package gormpg

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ParseConfig parses dsn, a URL or keyword=value pairs, with pgx.
//
// The session's DateStyle and IntervalStyle are pinned to ISO and postgres,
// the formats database/sql's text of an array column is parsed in (Array,
// and the core's pgtext): over the DSN's (in any letter case: PostgreSQL's
// names have none, and two spellings would both be sent), its options (-c,
// which PostgreSQL applies before the startup parameters) and the role's,
// database's or server's default.
//
// The pool is sized by the caller (Settings), never by the DSN: pgx would
// send a pool_* parameter to PostgreSQL as a setting, which fails only on
// the first statement of a pool that connects lazily, so it is refused here
// (naming the parameter, never the DSN).
func ParseConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	var pool []string
	for k := range cfg.RuntimeParams {
		switch {
		case strings.HasPrefix(strings.ToLower(k), "pool_"):
			pool = append(pool, k)
		case strings.EqualFold(k, "datestyle") || strings.EqualFold(k, "intervalstyle"):
			delete(cfg.RuntimeParams, k)
		}
	}
	if len(pool) > 0 {
		slices.Sort(pool)
		return nil, fmt.Errorf("database DSN: %s: the pool is sized by environment variables, not the DSN", strings.Join(pool, ", "))
	}
	cfg.RuntimeParams["datestyle"] = "ISO"
	cfg.RuntimeParams["intervalstyle"] = "postgres"
	return cfg, nil
}

// Settings are what database/sql sizes a pool with.
type Settings struct {
	MaxConns        int
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// WithMaxConns is s with MaxConns from value, an environment variable that
// sizes the pool, when it is a positive integer; another value is ignored.
func (s Settings) WithMaxConns(value string) Settings {
	if n, err := strconv.Atoi(value); err == nil && n > 0 {
		s.MaxConns = n
	}
	return s
}

// apply sizes db. Every connection may stay idle (database/sql would keep
// two and redial for every burst).
func (s Settings) apply(db *sql.DB) {
	db.SetMaxOpenConns(s.MaxConns)
	db.SetMaxIdleConns(s.MaxConns)
	db.SetConnMaxLifetime(s.MaxConnLifetime)
	db.SetConnMaxIdleTime(s.MaxConnIdleTime)
}

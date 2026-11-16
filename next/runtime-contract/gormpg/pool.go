package gormpg

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ParseConfig parses dsn, a URL or keyword=value pairs, with pgx, and takes
// pgxpool's pool_* parameters out of it.
//
// One DATABASE_URL is shared by processes on different drivers: pgxpool
// reads pool_max_conns and its siblings, while database/sql and a plain pgx
// connection would send them to PostgreSQL as settings, which it rejects
// (FATAL: unrecognized configuration parameter). No PostgreSQL setting
// starts with pool_, so they all leave the configuration; Pool holds the
// ones pgxpool knows, and Apply logs the others as ignored.
//
// The session's DateStyle and IntervalStyle are pinned to ISO and postgres,
// the formats database/sql's text of an array column is parsed in (Array,
// and the core's pgtext): over the DSN's (in any letter case: PostgreSQL's
// names have none, and two spellings would both be sent), its options (-c,
// which PostgreSQL applies before the startup parameters) and the role's,
// database's or server's default.
//
// Errors name a pool key and its value, never the DSN, which carries the
// password (pgx's own errors leave it out as well).
func ParseConfig(dsn string) (*pgx.ConnConfig, Pool, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, Pool{}, err
	}
	var p Pool
	for k, v := range cfg.RuntimeParams {
		switch {
		case strings.HasPrefix(k, "pool_"):
			if err := p.set(k, v); err != nil {
				return nil, Pool{}, err
			}
			delete(cfg.RuntimeParams, k)
		case strings.EqualFold(k, "datestyle"), strings.EqualFold(k, "intervalstyle"):
			delete(cfg.RuntimeParams, k)
		}
	}
	cfg.RuntimeParams["datestyle"] = "ISO"
	cfg.RuntimeParams["intervalstyle"] = "postgres"
	return cfg, p, nil
}

// Pool is the pool_* parameters of a DSN; a nil field was not set.
type Pool struct {
	MaxConns              *int
	MinConns              *int
	MinIdleConns          *int
	MaxConnLifetime       *time.Duration
	MaxConnIdleTime       *time.Duration
	HealthCheckPeriod     *time.Duration
	MaxConnLifetimeJitter *time.Duration
	// Unknown are the pool_* keys pgxpool does not know, in order.
	Unknown []string
}

// params are pgxpool's DSN parameters (pgxpool.ParseConfig), each with the
// parser that stores it.
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

// set parses one pool_* parameter into p.
func (p *Pool) set(key, value string) error {
	parse, known := params[key]
	if !known {
		p.Unknown = append(p.Unknown, key)
		slices.Sort(p.Unknown)
		return nil
	}
	if err := parse(p, value); err != nil {
		return fmt.Errorf("gormpg: %s=%q %w", key, value, err)
	}
	return nil
}

// OverrideMaxConns sets MaxConns from value, an environment variable that
// takes precedence over the DSN, when it is a positive integer; another
// value is ignored.
func (p *Pool) OverrideMaxConns(value string) {
	if n, err := strconv.Atoi(value); err == nil && n > 0 {
		p.MaxConns = &n
	}
}

// Settings are what database/sql sizes a pool with.
type Settings struct {
	MaxConns        int
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// Over is p's settings, def's where p sets none.
func (p Pool) Over(def Settings) Settings {
	return Settings{
		MaxConns:        orDefault(p.MaxConns, def.MaxConns),
		MaxConnLifetime: orDefault(p.MaxConnLifetime, def.MaxConnLifetime),
		MaxConnIdleTime: orDefault(p.MaxConnIdleTime, def.MaxConnIdleTime),
	}
}

// Apply sizes db from p over def. Like pgxpool, every connection may stay
// idle (database/sql would keep two and redial for every burst). The
// parameters database/sql has no counterpart for - a floor of open or idle
// connections, the health check period, the lifetime jitter - and the
// unknown ones are logged to log (nil: slog.Default) as ignored, once.
func (p Pool) Apply(db *sql.DB, def Settings, log *slog.Logger) {
	s := p.Over(def)
	db.SetMaxOpenConns(s.MaxConns)
	db.SetMaxIdleConns(s.MaxConns)
	db.SetConnMaxLifetime(s.MaxConnLifetime)
	db.SetConnMaxIdleTime(s.MaxConnIdleTime)

	ignored := slices.Clone(p.Unknown)
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

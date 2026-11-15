// Package pgdb opens GORM handles on PostgreSQL for plugins, their tests
// and the sub2api-plugin db commands.
//
// This file is the only one in the SDK that imports pgx: a dial hook (the
// egress tunnel of strict network mode) can only be installed on the driver
// configuration, and PostgreSQL arrays are only parsed by the driver's type
// map. Everything else, the plugins included, speaks GORM.
package pgdb

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DefaultMaxConns is the connection limit of a handle opened without one,
// the pgxpool default the plugins used before.
const DefaultMaxConns = 4

// Options tune Open. The zero value dials directly with the defaults.
type Options struct {
	// SearchPath is set as search_path unless the DSN already sets one.
	SearchPath string
	// MaxConns caps the open connections (0 = DefaultMaxConns).
	MaxConns int
	// Dial opens the TCP connections (nil = a net.Dialer).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// DialByName hands host names to Dial unresolved: the egress tunnel
	// resolves them on the host, where the plugin's DNS cannot.
	DialByName bool
}

// Open returns a GORM handle on dsn. It does no I/O: connections are opened
// on first use, like the pgx pool it replaces, so a database that is down
// at Init does not fail the plugin's start.
func Open(dsn string, o Options) (*gorm.DB, error) { return open(dsn, o, nil) }

// open is Open with the dialector wrapped (drift.go swaps the migrator).
func open(dsn string, o Options, wrap func(gorm.Dialector) gorm.Dialector) (*gorm.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgdb: parse dsn: %w", err)
	}
	// pgx takes a key it does not know for a server setting. The host strips
	// its pgxpool parameters (pool_max_conns, ...) from the DSN it hands a
	// plugin, but a DSN from elsewhere (a test's, a host of an older release)
	// may still carry them, and PostgreSQL refuses the connection over one.
	// No PostgreSQL setting starts with pool_, so they all go; the pool is
	// sized by Options alone. Parsing them (runtime-contract/pgdsn) would
	// add a module dependency to every plugin for settings it does not use.
	for k := range cfg.RuntimeParams {
		if strings.HasPrefix(k, "pool_") {
			delete(cfg.RuntimeParams, k)
		}
	}
	if o.Dial != nil {
		cfg.DialFunc = o.Dial
	} else {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		cfg.DialFunc = d.DialContext
	}
	if o.DialByName {
		cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	}
	if _, set := cfg.RuntimeParams["search_path"]; !set && o.SearchPath != "" {
		cfg.RuntimeParams["search_path"] = o.SearchPath
	}
	sqlDB := stdlib.OpenDB(*cfg)
	conns := o.MaxConns
	if conns <= 0 {
		conns = DefaultMaxConns
	}
	// Keep the pool's connections like pgxpool did (database/sql would keep
	// only two idle ones and reconnect for every burst).
	sqlDB.SetMaxOpenConns(conns)
	sqlDB.SetMaxIdleConns(conns)
	sqlDB.SetConnMaxLifetime(time.Hour)
	sqlDB.SetConnMaxIdleTime(30 * time.Minute)

	var dialector gorm.Dialector = postgres.New(postgres.Config{Conn: sqlDB})
	if wrap != nil {
		dialector = wrap(dialector)
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		// The caller owns transactions; a single statement needs none.
		SkipDefaultTransaction: true,
		DisableAutomaticPing:   true,
		// Errors go back to the caller; the SQL text could carry values.
		Logger: logger.Discard,
		// gorm.ErrDuplicatedKey and friends, wrapping the driver error.
		TranslateError: true,
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("pgdb: open: %w", err)
	}
	return db, nil
}

// Close closes the connections of a handle Open returned.
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// typeMaps parse array values: database/sql hands them over as text. A
// pgtype.Map caches scan plans without a lock, so concurrent scans take one
// each; a pool is cheaper than a new map per scan.
var typeMaps = sync.Pool{New: func() any { return pgtype.NewMap() }}

// IsUniqueViolation reports whether err is or wraps a unique violation
// (SQLSTATE 23505). Unlike errors.Is(err, gorm.ErrDuplicatedKey), it also
// holds for the errors of Raw(...).Row() and Rows(), which GORM does not
// translate.
func IsUniqueViolation(err error) bool {
	var e interface{ SQLState() string }
	return errors.As(err, &e) && e.SQLState() == "23505"
}

// Array is a PostgreSQL array (text[], bigint[], ...) as a model field or a
// query parameter. GORM expands a plain slice parameter into a list, one
// placeholder per element, and cannot scan an array column into one; an
// Array goes to the driver as one array value and scans back from one.
type Array[T any] []T

// Value hands the slice to the driver, which encodes it as an array; nil is
// NULL. Being a driver.Valuer is what keeps GORM from expanding it.
func (a Array[T]) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	return []T(a), nil
}

// GormDataType makes an Array field without a type tag acceptable to GORM,
// e.g. in a query result. A model column still names its type
// (`gorm:"type:text[]"`): that is the DDL sub2api-plugin db writes.
func (Array[T]) GormDataType() string { return "array" }

// Scan parses an array column; NULL is nil.
func (a *Array[T]) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	m := typeMaps.Get().(*pgtype.Map)
	defer typeMaps.Put(m)
	return m.SQLScanner((*[]T)(a)).Scan(src)
}

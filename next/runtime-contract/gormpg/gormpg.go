// Package gormpg opens GORM on PostgreSQL the one way the core, the gateway
// and the plugin SDK share: pgx parses the DSN (ParseConfig), database/sql
// pools pgx's connections, sized by the caller (Settings), and GORM runs its
// statements on a sqlpool.Pool, which reads every Exec's result before the
// connection goes back (Open). It also holds the small helpers the three
// need alike: the array parameter Array, the PostgreSQL error readers
// (errors.go), and the statements GORM's builder has no form for
// (query.go); package sqlcheck is the SQL source check all of them run.
// pgx is the driver, and this package is the only one of the repository's
// modules that imports it.
//
// The protocol packages of this module (the root, locklease, sqlpool) stay
// standard library only; this one depends on GORM and pgx, which every
// user of it already has.
package gormpg

import (
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/sqlpool"
)

// Options tune Open.
type Options struct {
	// Pool sizes the pool.
	Pool Settings
	// Gorm is the instance's configuration. It must set
	// DisableAutomaticPing (Open does no I/O) and leave PrepareStmt off: a
	// prepared statement runs on the pool like database/sql's DB, past the
	// result reading of sqlpool.
	Gorm *gorm.Config
	// Dialector wraps the postgres dialector (nil: none).
	Dialector func(gorm.Dialector) gorm.Dialector
	// Setup runs on the instance before Open returns it, e.g. to register
	// callbacks (nil: none).
	Setup func(*gorm.DB) error
}

// Open returns a GORM instance on cfg (see the package) and its pool as a
// database/sql handle, which closes it. It does no I/O: the first statement
// dials. On an error nothing stays open.
func Open(cfg *pgx.ConnConfig, o Options) (*gorm.DB, *sql.DB, error) {
	sqlDB := stdlib.OpenDB(*cfg)
	o.Pool.apply(sqlDB)
	var dialector gorm.Dialector = postgres.New(postgres.Config{Conn: sqlDB})
	if o.Dialector != nil {
		dialector = o.Dialector(dialector)
	}
	g, err := gorm.Open(dialector, o.Gorm)
	if err == nil && o.Setup != nil {
		err = o.Setup(g)
	}
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	// After gorm.Open, which took sqlDB as the instance's pool. GORM reads
	// an Exec's RowsAffected after database/sql has given the connection
	// back, under a mutex that the connection's next statement holds:
	// sqlpool reads it first (see there). Transactions keep their
	// connection and need no wrapper.
	g.ConnPool = sqlpool.New(sqlDB)
	g.Statement.ConnPool = g.ConnPool
	return g, sqlDB, nil
}

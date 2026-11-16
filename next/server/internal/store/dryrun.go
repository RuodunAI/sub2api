package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// DryRun is a DryRun session of a GORM instance configured as Open's
// (callbacks, clause builders), on a pool that never connects: it builds
// statements without a database, and one that escaped DryRun would fail
// instead of reaching a server. Package gormtest snapshots its SQL.
func DryRun() *gorm.DB { return dryRun().Session(&gorm.Session{DryRun: true, NewDB: true}) }

// ErrOffline is what a statement of DryRun's instance that is no DryRun
// gets: there is no database behind it.
var ErrOffline = errors.New("store: the DryRun instance has no database")

var dryRun = sync.OnceValue(func() *gorm.DB {
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: sql.OpenDB(offline{})}), gormConfig())
	if err == nil {
		err = registerTimestamps(g)
	}
	if err != nil {
		panic("store: open the DryRun instance: " + err.Error()) // no I/O: only a programming error gets here
	}
	return g
})

// offline is a database/sql connector and driver whose connections all
// fail to open.
type offline struct{}

func (offline) Connect(context.Context) (driver.Conn, error) { return nil, ErrOffline }
func (offline) Driver() driver.Driver                        { return offline{} }
func (offline) Open(string) (driver.Conn, error)             { return nil, ErrOffline }

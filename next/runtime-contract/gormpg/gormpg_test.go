package gormpg

import (
	"database/sql"
	"errors"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/sqlpool"
)

func config() *gorm.Config {
	return &gorm.Config{SkipDefaultTransaction: true, DisableAutomaticPing: true, Logger: logger.Discard}
}

// Open does no I/O, sizes the pool and runs the statements of the instance
// on sqlpool, the pool that reads an Exec's result before the connection
// goes back.
func TestOpen(t *testing.T) {
	cfg, pool, err := ParseConfig("postgres://u:p@127.0.0.1:1/app?pool_max_conns=3")
	if err != nil {
		t.Fatal(err)
	}
	set := false
	g, sqlDB, err := Open(cfg, Options{Pool: pool, Defaults: Settings{MaxConns: 8}, Gorm: config(),
		Setup: func(*gorm.DB) error { set = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.ConnPool.(sqlpool.Pool); !ok || g.Statement.ConnPool != g.ConnPool || !set {
		t.Fatalf("pool %T, statement pool %T, setup ran %v", g.ConnPool, g.Statement.ConnPool, set)
	}
	defer sqlDB.Close()
	if fromGorm, err := g.DB(); err != nil || fromGorm != sqlDB {
		t.Fatalf("g.DB() = %p, %v; want the pool Open returned", fromGorm, err)
	}
	if n := sqlDB.Stats().MaxOpenConnections; n != 3 {
		t.Fatalf("max open = %d, want the DSN's 3", n)
	}
}

// A Setup that fails leaves nothing open: the pool Open made is closed.
func TestOpenClosesOnError(t *testing.T) {
	cfg, _, err := ParseConfig("postgres://u:p@127.0.0.1:1/app")
	if err != nil {
		t.Fatal(err)
	}
	var opened *sql.DB
	boom := errors.New("boom")
	_, _, err = Open(cfg, Options{Defaults: Settings{MaxConns: 1}, Gorm: config(), Setup: func(g *gorm.DB) error {
		opened, _ = g.DB()
		return boom
	}})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := opened.Ping(); err == nil || err.Error() != "sql: database is closed" {
		t.Fatalf("the pool stayed open: %v", err)
	}
}

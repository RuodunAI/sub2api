package store

import (
	"testing"
	"time"
)

func TestPoolMaxConns(t *testing.T) {
	for _, c := range []struct {
		env     string
		fromDSN int
		want    int
	}{
		{"", 0, defaultPoolMaxConns},    // no pool_max_conns: the default, not database/sql's unlimited
		{"", 7, 7},                      // the DSN decides
		{"30", 7, 30},                   // the environment wins
		{"abc", 0, defaultPoolMaxConns}, // an invalid value is ignored
		{"0", 0, defaultPoolMaxConns},   // so is a non-positive one
		{"-1", 3, 3},
	} {
		if got := poolMaxConns(c.env, c.fromDSN); got != c.want {
			t.Errorf("poolMaxConns(%q, %d) = %d, want %d", c.env, c.fromDSN, got, c.want)
		}
	}
}

func TestTakePoolParams(t *testing.T) {
	defaults := poolConfig{maxLifetime: time.Hour, maxIdleTime: 30 * time.Minute}
	for _, c := range []struct {
		dsn, want string
		pc        poolConfig
	}{
		{"postgres://u@h/db?sslmode=disable", "postgres://u@h/db?sslmode=disable", defaults},
		{
			"postgres://u:p@h:5432/db?pool_max_conns=7&sslmode=disable&pool_min_conns=2&pool_max_conn_lifetime=2h&pool_max_conn_idle_time=5m&pool_health_check_period=1m",
			"postgres://u:p@h:5432/db?sslmode=disable",
			poolConfig{maxConns: 7, minConns: 2, maxLifetime: 2 * time.Hour, maxIdleTime: 5 * time.Minute},
		},
		{"postgresql://h/db?pool_max_conns=3", "postgresql://h/db", poolConfig{maxConns: 3, maxLifetime: time.Hour, maxIdleTime: 30 * time.Minute}},
		{
			"host=h dbname=db pool_max_conns=9 password='a b\\'c' pool_max_conn_idle_time = 1m sslmode=disable",
			"host=h dbname=db password='a b\\'c' sslmode=disable",
			poolConfig{maxConns: 9, maxLifetime: time.Hour, maxIdleTime: time.Minute},
		},
		{"host=h  pool_min_conns=1\tuser=x", "host=h user=x", poolConfig{minConns: 1, maxLifetime: time.Hour, maxIdleTime: 30 * time.Minute}},
	} {
		dsn, pc, err := takePoolParams(c.dsn)
		if err != nil || dsn != c.want || pc != c.pc {
			t.Errorf("takePoolParams(%q) = %q, %+v, %v; want %q, %+v", c.dsn, dsn, pc, err, c.want, c.pc)
		}
	}
	for _, dsn := range []string{
		"postgres://h/db?pool_max_conns=0",
		"postgres://h/db?pool_max_conns=x",
		"postgres://h/db?pool_max_conn_lifetime=forever",
		"host=h pool_max_conns=-2",
		"host=h password='open pool_max_conns=1",
	} {
		if _, _, err := takePoolParams(dsn); err == nil {
			t.Errorf("takePoolParams(%q) accepted it", dsn)
		}
	}
}

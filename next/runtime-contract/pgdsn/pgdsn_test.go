package pgdsn

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name, dsn, want string
		pool            Pool
	}{
		{"url without query", "postgres://u:p@db/app", "postgres://u:p@db/app", Pool{}},
		{"url without pool params", "postgresql://db/app?sslmode=disable&application_name=a%20b",
			"postgresql://db/app?sslmode=disable&application_name=a%20b", Pool{}},
		{"url", "postgres://u:p%40w@db:5432/app?sslmode=require&pool_max_conns=20&pool_min_conns=4&pool_max_conn_lifetime=1h30m",
			"postgres://u:p%40w@db:5432/app?sslmode=require",
			Pool{MaxConns: ptr(20), MinConns: ptr(4), MaxConnLifetime: ptr(90 * time.Minute)}},
		{"url only pool params", "postgres://db/app?pool_max_conn_idle_time=1m&pool_health_check_period=5s&pool_max_conn_lifetime_jitter=10s&pool_min_idle_conns=1#f",
			"postgres://db/app#f",
			Pool{MaxConnIdleTime: ptr(time.Minute), HealthCheckPeriod: ptr(5 * time.Second), MaxConnLifetimeJitter: ptr(10 * time.Second), MinIdleConns: ptr(1)}},
		{"url escaped key", "postgres://db/app?pool%5Fmax_conns=3&x=1", "postgres://db/app?x=1", Pool{MaxConns: ptr(3)}},
		{"url unknown pool_ key kept", "postgres://db/app?pool_mode=x", "postgres://db/app?pool_mode=x", Pool{}},
		{"kv without pool params", "host=db dbname=app", "host=db dbname=app", Pool{}},
		{"kv", "host=db pool_max_conns=7 dbname=app pool_max_conn_idle_time=2m",
			"host=db dbname=app", Pool{MaxConns: ptr(7), MaxConnIdleTime: ptr(2 * time.Minute)}},
		{"kv blanks and quotes", "  host = db\tpool_max_conns = '9'\n password='a b\\'c pool_max_conns=1' ",
			"host=db password='a b\\'c pool_max_conns=1'", Pool{MaxConns: ptr(9)}},
		{"kv backslash in unquoted value", `password=a\ b pool_min_conns=\2`, `password=a\ b`, Pool{MinConns: ptr(2)}},
		{"kv empty value", "password= pool_max_conns=2", "password=pool_max_conns=2", Pool{}},
		{"kv empty", "", "", Pool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, pool, err := Split(tc.dsn)
			if err != nil || got != tc.want || !reflect.DeepEqual(pool, tc.pool) {
				t.Fatalf("Split(%q) = %q, %+v, %v; want %q, %+v", tc.dsn, got, pool, err, tc.want, tc.pool)
			}
		})
	}
}

func TestSplitRejects(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:secret@db/app?pool_max_conns=0",
		"postgres://u:secret@db/app?pool_max_conns=x",
		"postgres://u:secret@db/app?pool_min_conns=-1",
		"postgres://u:secret@db/app?pool_max_conn_lifetime=-1s",
		"postgres://u:secret@db/app?pool_max_conn_idle_time=10",
		"postgres://u:secret@db/app?x=%zz",
		"password=secret pool_max_conns=0",
		"password=secret host",
		"password='secret",
		`password=secret\`,
	} {
		_, _, err := Split(dsn)
		if err == nil {
			t.Errorf("Split(%q) accepted", dsn)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("Split(%q) error leaks the password: %v", dsn, err)
		}
	}
}

// nopDriver lets a *sql.DB exist without a server: Apply never connects.
type nopDriver struct{}

func (nopDriver) Open(string) (driver.Conn, error) { return nil, errors.New("no connections") }

func init() { sql.Register("pgdsn-nop", nopDriver{}) }

func TestApply(t *testing.T) {
	def := Defaults{MaxConns: 8, MaxConnLifetime: time.Hour, MaxConnIdleTime: 30 * time.Minute}
	for _, tc := range []struct {
		name     string
		pool     Pool
		maxConns int
		warn     string
	}{
		{"defaults", Pool{}, 8, ""},
		{"from dsn", Pool{MaxConns: ptr(3), MaxConnLifetime: ptr(time.Minute), MaxConnIdleTime: ptr(time.Second)}, 3, ""},
		{"ignored", Pool{MinConns: ptr(1), MaxConnLifetimeJitter: ptr(time.Second)}, 8,
			"params=\"[pool_max_conn_lifetime_jitter pool_min_conns]\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("pgdsn-nop", "")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var logs bytes.Buffer
			tc.pool.Apply(db, def, slog.New(slog.NewTextHandler(&logs, nil)))
			if got := db.Stats().MaxOpenConnections; got != tc.maxConns {
				t.Errorf("max open = %d, want %d", got, tc.maxConns)
			}
			if out := logs.String(); (tc.warn == "") != (out == "") || !strings.Contains(out, tc.warn) {
				t.Errorf("log = %q, want %q", out, tc.warn)
			}
		})
	}
}

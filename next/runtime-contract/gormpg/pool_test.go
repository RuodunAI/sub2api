package gormpg

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestParseConfig(t *testing.T) {
	pinned := map[string]string{"datestyle": "ISO", "intervalstyle": "postgres"}
	for _, tc := range []struct {
		name, dsn string
		params    map[string]string // RuntimeParams besides the pinned ones
		pool      Pool
	}{
		{"url without query", "postgres://u:p@db/app", nil, Pool{}},
		{"url without pool params", "postgresql://db/app?sslmode=disable&application_name=a%20b",
			map[string]string{"application_name": "a b"}, Pool{}},
		{"url", "postgres://u:p%40w@db:5432/app?sslmode=require&pool_max_conns=20&pool_min_conns=4&pool_max_conn_lifetime=1h30m",
			nil, Pool{MaxConns: ptr(20), MinConns: ptr(4), MaxConnLifetime: ptr(90 * time.Minute)}},
		{"url only pool params", "postgres://db/app?pool_max_conn_idle_time=1m&pool_health_check_period=5s&pool_max_conn_lifetime_jitter=10s&pool_min_idle_conns=1",
			nil, Pool{MaxConnIdleTime: ptr(time.Minute), HealthCheckPeriod: ptr(5 * time.Second), MaxConnLifetimeJitter: ptr(10 * time.Second), MinIdleConns: ptr(1)}},
		{"url escaped key", "postgres://db/app?pool%5Fmax_conns=3&x=1", map[string]string{"x": "1"}, Pool{MaxConns: ptr(3)}},
		{"unknown pool_ keys leave too", "postgres://db/app?pool_mode=x&pool_a=1", nil, Pool{Unknown: []string{"pool_a", "pool_mode"}}},
		{"kv without pool params", "host=db dbname=app", nil, Pool{}},
		{"kv", "host=db pool_max_conns=7 dbname=app pool_max_conn_idle_time=2m",
			nil, Pool{MaxConns: ptr(7), MaxConnIdleTime: ptr(2 * time.Minute)}},
		{"kv blanks and quotes", "  host = db\tpool_max_conns = '9'\n application_name='a b\\'c pool_max_conns=1' ",
			map[string]string{"application_name": "a b'c pool_max_conns=1"}, Pool{MaxConns: ptr(9)}},
		{"date and interval styles pinned in any case", "postgres://db/app?DateStyle=German&datestyle=SQL%2C%20DMY&IntervalStyle=iso_8601&options=-c%20datestyle%3DSQL",
			map[string]string{"options": "-c datestyle=SQL"}, Pool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, pool, err := ParseConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			want := maps.Clone(pinned)
			maps.Copy(want, tc.params)
			if !maps.Equal(cfg.RuntimeParams, want) || !reflect.DeepEqual(pool, tc.pool) {
				t.Fatalf("ParseConfig(%q) = %v, %+v; want %v, %+v", tc.dsn, cfg.RuntimeParams, pool, want, tc.pool)
			}
		})
	}
	// What is no setting stays pgx's: the host, the password, TLS.
	cfg, _, err := ParseConfig("postgres://u:p%40w@db:6432/app?sslmode=disable&pool_max_conns=2")
	if err != nil || cfg.Host != "db" || cfg.Port != 6432 || cfg.Password != "p@w" || cfg.Database != "app" || cfg.TLSConfig != nil {
		t.Fatalf("ParseConfig = %+v, %v", cfg, err)
	}
}

func TestParseConfigRejects(t *testing.T) {
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
	} {
		_, _, err := ParseConfig(dsn)
		if err == nil {
			t.Errorf("ParseConfig(%q) accepted", dsn)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("ParseConfig(%q) error leaks the password: %v", dsn, err)
		}
	}
}

func TestOverrideMaxConns(t *testing.T) {
	for _, c := range []struct {
		env     string
		fromDSN *int
		want    *int
	}{
		{"", nil, nil},
		{"", ptr(7), ptr(7)},    // the DSN decides
		{"30", ptr(7), ptr(30)}, // the environment wins
		{"abc", nil, nil},       // an invalid value is ignored
		{"0", nil, nil},         // so is a non-positive one
		{"-1", ptr(3), ptr(3)},
	} {
		p := Pool{MaxConns: c.fromDSN}
		p.OverrideMaxConns(c.env)
		if !reflect.DeepEqual(p.MaxConns, c.want) {
			t.Errorf("OverrideMaxConns(%q) over %v = %v, want %v", c.env, c.fromDSN, p.MaxConns, c.want)
		}
	}
}

// nopDriver lets a *sql.DB exist without a server: Apply never connects.
type nopDriver struct{}

func (nopDriver) Open(string) (driver.Conn, error) { return nil, errors.New("no connections") }

func init() { sql.Register("gormpg-nop", nopDriver{}) }

func TestApply(t *testing.T) {
	def := Settings{MaxConns: 8, MaxConnLifetime: time.Hour, MaxConnIdleTime: 30 * time.Minute}
	for _, tc := range []struct {
		name     string
		pool     Pool
		maxConns int
		warn     string
	}{
		{"defaults", Pool{}, 8, ""},
		{"from dsn", Pool{MaxConns: ptr(3), MaxConnLifetime: ptr(time.Minute), MaxConnIdleTime: ptr(time.Second)}, 3, ""},
		{"ignored", Pool{MinConns: ptr(1), MaxConnLifetimeJitter: ptr(time.Second), Unknown: []string{"pool_mode"}}, 8,
			"params=\"[pool_max_conn_lifetime_jitter pool_min_conns pool_mode]\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("gormpg-nop", "")
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
	got := Pool{MaxConnIdleTime: ptr(time.Second)}.Over(def)
	if want := (Settings{MaxConns: 8, MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Second}); got != want {
		t.Errorf("Over = %+v, want %+v", got, want)
	}
}

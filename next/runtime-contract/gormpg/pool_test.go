package gormpg

import (
	"maps"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	pinned := map[string]string{"datestyle": "ISO", "intervalstyle": "postgres"}
	for _, tc := range []struct {
		name, dsn string
		params    map[string]string // RuntimeParams besides the pinned ones
	}{
		{"url without query", "postgres://u:p@db/app", nil},
		{"url", "postgresql://db/app?sslmode=disable&application_name=a%20b", map[string]string{"application_name": "a b"}},
		{"kv", "host=db dbname=app", nil},
		{"kv blanks and quotes", "  host = db\tapplication_name='a b\\'c'\n ", map[string]string{"application_name": "a b'c"}},
		{"date and interval styles pinned in any case", "postgres://db/app?DateStyle=German&datestyle=SQL%2C%20DMY&IntervalStyle=iso_8601&options=-c%20datestyle%3DSQL",
			map[string]string{"options": "-c datestyle=SQL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			want := maps.Clone(pinned)
			maps.Copy(want, tc.params)
			if !maps.Equal(cfg.RuntimeParams, want) {
				t.Fatalf("ParseConfig(%q) = %v; want %v", tc.dsn, cfg.RuntimeParams, want)
			}
		})
	}
	// What is no setting stays pgx's: the host, the password, TLS.
	cfg, err := ParseConfig("postgres://u:p%40w@db:6432/app?sslmode=disable")
	if err != nil || cfg.Host != "db" || cfg.Port != 6432 || cfg.Password != "p@w" || cfg.Database != "app" || cfg.TLSConfig != nil {
		t.Fatalf("ParseConfig = %+v, %v", cfg, err)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:secret@db/app?x=%zz",
		"password=secret host",
		"password='secret",
	} {
		_, err := ParseConfig(dsn)
		if err == nil {
			t.Errorf("ParseConfig(%q) accepted", dsn)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("ParseConfig(%q) error leaks the password: %v", dsn, err)
		}
	}
}

func TestWithMaxConns(t *testing.T) {
	def := Settings{MaxConns: 7, MaxConnLifetime: time.Hour}
	for _, c := range []struct {
		env  string
		want int
	}{
		{"", 7},
		{"30", 30}, // the environment wins
		{"abc", 7}, // an invalid value is ignored
		{"0", 7},   // so is a non-positive one
		{"-1", 7},
	} {
		got := def.WithMaxConns(c.env)
		if got != (Settings{MaxConns: c.want, MaxConnLifetime: time.Hour}) {
			t.Errorf("WithMaxConns(%q) = %+v, want MaxConns %d", c.env, got, c.want)
		}
	}
}

package store

import (
	"testing"
)

func TestPoolMaxConns(t *testing.T) {
	for _, c := range []struct {
		env     string
		fromDSN *int
		want    int
	}{
		{"", nil, defaultPoolMaxConns},    // no pool_max_conns: the default, not database/sql's unlimited
		{"", ptr(7), 7},                   // the DSN decides
		{"30", ptr(7), 30},                // the environment wins
		{"abc", nil, defaultPoolMaxConns}, // an invalid value is ignored
		{"0", nil, defaultPoolMaxConns},   // so is a non-positive one
		{"-1", ptr(3), 3},
	} {
		if got := poolMaxConns(c.env, c.fromDSN); got != c.want {
			t.Errorf("poolMaxConns(%q, %v) = %d, want %d", c.env, c.fromDSN, got, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

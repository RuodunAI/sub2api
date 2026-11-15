package store

import (
	"strconv"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/pgdsn"
)

// The pool settings a DSN may carry are pgxpool's pool_* parameters, as the
// core read them before its pool moved to database/sql. pgx's connection
// parser would send them to PostgreSQL as session settings, which it
// rejects, so Open takes them out of the DSN (pgdsn.Split) and sizes the
// database/sql pool from them (pgdsn.Pool.Apply), as the gateway does.
const (
	// defaultPoolMaxConns is the pool size when neither
	// SUB2API_PG_MAX_CONNS nor pool_max_conns sets one.
	defaultPoolMaxConns = 20
	// pgxpool's defaults, kept so that a DSN without the parameters behaves
	// as before.
	defaultMaxConnLifetime = time.Hour
	defaultMaxConnIdleTime = 30 * time.Minute
)

// poolDefaults are the settings a DSN does not set. Every connection may
// stay idle, as in pgxpool, rather than database/sql's default of two,
// which would close and reopen connections under every burst (Apply sets
// MaxIdle to the pool size).
var poolDefaults = pgdsn.Defaults{
	MaxConns:        defaultPoolMaxConns,
	MaxConnLifetime: defaultMaxConnLifetime,
	MaxConnIdleTime: defaultMaxConnIdleTime,
}

// poolMaxConns picks the pool size: SUB2API_PG_MAX_CONNS (AR-P1-2) first,
// then pool_max_conns of the DSN (fromDSN, nil when absent), else
// defaultPoolMaxConns. 4 connections shared by the gateway and the console
// made every list request queue for a connection per statement; a
// plugin-heavy or multi-node setup may need more.
func poolMaxConns(env string, fromDSN *int) int {
	if n, err := strconv.Atoi(env); err == nil && n > 0 {
		return n
	}
	if fromDSN != nil && *fromDSN > 0 {
		return *fromDSN
	}
	return defaultPoolMaxConns
}

// valueOr is *v, or def when v is nil.
func valueOr[T any](v *T, def T) T {
	if v != nil {
		return *v
	}
	return def
}

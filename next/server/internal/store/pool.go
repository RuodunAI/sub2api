package store

import (
	"time"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
)

// poolDefaults are the pool settings when neither SUB2API_PG_MAX_CONNS nor
// the DSN's pool_* parameters (pgxpool's, as the core read them before its
// pool moved to database/sql; gormpg takes them out of the DSN) set one.
// 4 connections shared by the gateway and the console made every list
// request queue for a connection per statement; a plugin-heavy or
// multi-node setup may need more than 20 (AR-P1-2). The lifetimes are
// pgxpool's defaults, kept so that a DSN without the parameters behaves as
// before. Every connection may stay idle, as in pgxpool (Apply sets MaxIdle
// to the pool size).
var poolDefaults = gormpg.Settings{
	MaxConns:        20,
	MaxConnLifetime: time.Hour,
	MaxConnIdleTime: 30 * time.Minute,
}

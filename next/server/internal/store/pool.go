package store

import (
	"time"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
)

// poolDefaults are the pool settings; SUB2API_PG_MAX_CONNS sets another
// size. 4 connections shared by the gateway and the console made every list
// request queue for a connection per statement; a plugin-heavy or
// multi-node setup may need more than 20 (AR-P1-2). Every connection may
// stay idle.
var poolDefaults = gormpg.Settings{
	MaxConns:        20,
	MaxConnLifetime: time.Hour,
	MaxConnIdleTime: 30 * time.Minute,
}

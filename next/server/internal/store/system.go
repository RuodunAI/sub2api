package store

import (
	"strconv"

	"gorm.io/gorm"
)

// System statements: the server's own state rather than rows of a table
// (snapshots, catalog lookups), one helper per kind, so the text of each
// lives here and the callers build none (CONTRACTS.md, raw-SQL policy).

// CurrentSnapshot returns the xmin and xmax of the current snapshot of db
// (pg_current_snapshot): every transaction id below xmin has finished, and
// none at or past xmax had started.
func CurrentSnapshot(db *gorm.DB) (xmin, xmax uint64, err error) {
	var a, b string
	if err = db.Raw(`SELECT pg_snapshot_xmin(s)::text, pg_snapshot_xmax(s)::text FROM pg_current_snapshot() s`).Row().Scan(&a, &b); err != nil {
		return 0, 0, err
	}
	if xmin, err = strconv.ParseUint(a, 10, 64); err != nil {
		return 0, 0, err
	}
	xmax, err = strconv.ParseUint(b, 10, 64)
	return xmin, xmax, err
}

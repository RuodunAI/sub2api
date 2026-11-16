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

// CurrentDatabase returns the name of the database db is connected to.
func CurrentDatabase(db *gorm.DB) (string, error) {
	var name string
	err := db.Raw(`SELECT current_database()`).Row().Scan(&name)
	return name, err
}

// RelationExists reports whether name (schema-qualified or on the
// search_path) is a table, view or other relation (to_regclass): for a
// table outside the core schema, such as another program's.
func RelationExists(db *gorm.DB, name string) (bool, error) {
	var ok bool
	err := db.Raw(`SELECT to_regclass(?) IS NOT NULL`, name).Row().Scan(&ok)
	return ok, err
}

package dbschema

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL snapshots of the dbschema statements (see package gormtest).

// The marker comes first: the arguments follow the placeholders' order.
func TestSQLRoleOwned(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		var owned []bool
		return roleOwnedQuery(tx, "plg_demo", "plg_demo", "sub2api plugin role; database=app; plugin=demo").Find(&owned)
	})
}

func TestSQLPending(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		var n int64
		return pluginMigrations(tx, "demo").Where("migration_id = ANY(?)", store.Strings{"0001_init.sql"}).Count(&n)
	})
}

func TestSQLDropMigrationRecords(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return dropMigrationRecords(tx, "demo") })
}

func TestSQLApplied(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return Tracker{PluginKey: "demo"}.appliedQuery(tx).Scan(&[]migrationRecord{})
	})
}

func TestSQLAppliedAsPluginRole(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return Tracker{PluginKey: "demo", AsPluginRole: true}.appliedQuery(tx).Scan(&[]migrationRecord{})
	})
}

func TestSQLRecord(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return Tracker{PluginKey: "demo"}.recordQuery(tx, "0001_init.sql", "abc")
	})
}

func TestSQLRecordAsPluginRole(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return Tracker{PluginKey: "demo", AsPluginRole: true}.recordQuery(tx, "0001_init.sql", "abc")
	})
}

// A statement without arguments reaches the server as written, a "?" or
// "@name" inside a literal included: DDL is built by quoting, not binding.
func TestSQLDDLVerbatim(t *testing.T) {
	gormtest.AssertRun(t, func(tx *gorm.DB) error {
		return store.DDL(tx, "COMMENT ON ROLE %s IS %s", store.Ident("plg_demo"), store.Literal("database=a?b@c d; it's"))
	})
}

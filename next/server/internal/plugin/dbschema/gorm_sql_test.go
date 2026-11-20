package dbschema

import (
	"testing"

	"gorm.io/gorm"

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

func TestSQLApplied(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return tx.Table(Tracker{Schema: "public", Table: "plg_demo__schema_migrations"}.table()).Select("migration_id, checksum").Scan(&[]migrationRecord{})
	})
}

func TestSQLRecord(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return Tracker{Schema: "public", Table: "plg_demo__schema_migrations"}.recordQuery(tx, "0001_init.sql", "abc")
	})
}

func TestSQLStrayObjects(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		var found []stray
		return strayObjects(tx, "public", "plg_demo__", "plg1_demo").Scan(&found)
	})
}

func TestSQLCodeObjects(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		var found []stray
		return codeObjects(tx, "public", "plg_demo__").Scan(&found)
	})
}

func TestSQLForeignGrants(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		var found []grant
		return foreignGrants(tx, "public", "plg_demo__").Scan(&found)
	})
}

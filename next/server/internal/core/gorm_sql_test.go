package core

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the core statements built with GORM (see package gormtest).

var snapshotPatterns = []string{`(^|/)account/3$`, `(^|/)account/4$`}

func TestSQLLockOpenDrafts(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return lockOpenDrafts(db, 7) })
}

func TestSQLScopeGrantsExist(t *testing.T) {
	var found bool
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return store.ExistsQuery(db, scopeGrants(db, snapshotPatterns)).Scan(&found)
	})
}

// The patterns and ids are one array parameter each.
func TestSQLClearScopeGrants(t *testing.T) {
	actor := int64(9)
	var version int64
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return clearScopeGrants(db, snapshotPatterns, &actor, ScopeRefAccount, []int64{3, 4}).Scan(&version)
	})
}

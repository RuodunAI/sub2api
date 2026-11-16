package group

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the group list, whose filter is assembled per request (see
// package gormtest); rerun with -update after an intended change.

// GET /groups: the filters only appear when given, the id list is one array
// parameter and the plugin keys fill both placeholders of account_count.
func TestSQLGroupList(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []groupRow
		return selectGroups(db, []string{"anthropic"}).Scopes(listFilter("vip", "active", []int64{3, 1})).
			Order("g.id").Limit(20).Offset(20).Scan(&rows)
	})
}

// Without a registry the plugin keys are NULL (every account counts), not
// the empty array (none would).
func TestSQLGroupListWithoutRegistry(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []groupRow
		return selectGroups(db, nil).Scopes(listFilter("", "", nil)).Order("g.id").Limit(20).Scan(&rows)
	})
}

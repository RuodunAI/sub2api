package group

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The snapshots pin the SQL of the statements this package builds with
// GORM (see package gormtest); rerun with -update after an intended change.

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

func TestSQLGroupAccess(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []struct{ GroupID int64 }
		return groupAccessQuery(db, []int64{1, 2}).Scan(&rows)
	})
}

// DELETE /groups/:id locks the group and the move target in id order.
func TestSQLLockGroups(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var locked []int64
		return lockGroups(db, []int64{4, 9}).Pluck("id", &locked)
	})
}

func TestSQLMoveKeys(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var moved []model.APIKey
		to := int64(9)
		return moveKeys(db, &moved, 4, &to)
	})
}

func TestSQLGroupTypes(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []struct{ GroupID int64 }
		return liveAccounts(db, []int64{1, 2}).Distinct("ag.group_id", "a.plugin_key", "a.type").Scan(&rows)
	})
}

package cluster

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL snapshots of the cluster statements (testdata/*.golden): that of
// CoreUpdateBusy, the only one of the package.

func TestSQLUpgradeActive(t *testing.T) {
	var busy bool
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return gormpg.ExistsQuery(db, upgradeActive(db)).Scan(&busy) })
}

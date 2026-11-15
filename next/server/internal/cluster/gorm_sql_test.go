package cluster

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL snapshots of the cluster statements (testdata/*.golden): those of
// CoreUpdateBusy, the only ones of the package.

func TestSQLUpdaterInstalled(t *testing.T) {
	var installed bool
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return updaterInstalled(db, &installed) })
}

func TestSQLUpgradeActive(t *testing.T) {
	var busy bool
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return upgradeActive(db, &busy) })
}

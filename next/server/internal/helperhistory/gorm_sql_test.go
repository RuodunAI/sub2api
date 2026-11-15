package helperhistory

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL snapshots of the helper history statements (testdata/*.golden).

var (
	sqlOwner = core.ResourceOwner{UserID: 3, GroupID: 5}
	sqlNow   = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
)

func TestSQLHoldings(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return holdingsQuery(db, sqlOwner, sqlNow, &holdings{}) })
}

func TestSQLPrefixRows(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return prefixRows(db, sqlOwner, "ns", "prefix", &[]prefixRow{}) })
}

func TestSQLPendingOutbox(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return pendingOutbox(db, sqlNow, 50, &[]model.ProviderHelperUsageOutbox{})
	})
}

func TestSQLDeferOutbox(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return deferOutbox(db, "req", "digest", "integrity", sqlNow) })
}

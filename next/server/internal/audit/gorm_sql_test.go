package audit

import (
	"encoding/json"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL snapshots of the audit statements (testdata/*.golden).

var sqlFilter = func() store.Conds {
	var f store.Conds
	f.And("a.target_type = ?", "plugin")
	f.And("a.user_id = ?", int64(7))
	f.And("a.created_at >= ?", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	return f
}()

func TestSQLLogCount(t *testing.T) {
	var n int64
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return logs(db, sqlFilter).Count(&n) })
}

func TestSQLLogPage(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return logPage(db, sqlFilter, 20, 40, &[]json.RawMessage{}) })
}

func TestSQLPurgeBatch(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return purgeBatch(db, PluginWriteRetention) })
}

package account

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the account statements assembled per call: the console list
// and the plugin page (see package gormtest).

var snapshotScope = core.DataScope{Owner: 7, IDs: []int64{3, 4}, Groups: []int64{9}}

// The account list: the total and one page, under the same conditions
// (the caller's scope among them).
func TestSQLAccountList(t *testing.T) {
	var conds store.Conds
	conds.And("a.deleted_at IS NULL")
	conds = append(conds, core.AccountRows.Scope(snapshotScope))
	conds.And("a.plugin_key = ANY(?)", store.Strings{"anthropic", "openai"})
	conds.And("(a.status IN (?, 'error') OR (a.status = 'active' AND NOT a.schedulable))", "auto_disabled")
	conds.And("a.id = ANY(?)", store.Int64s{3, 4})
	conds.And("a.name ILIKE '%' || ? || '%'", "prod")
	t.Run("count", func(t *testing.T) {
		var n int64
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return listCount(db, conds).Count(&n) })
	})
	t.Run("page", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return listRows(db, conds, 3, 20).Scan(&struct{}{}) })
	})
}

func TestSQLPluginAccountsPage(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var rows []pluginAccount
			return pluginAccountsPage(db, "anthropic", core.PluginAccountQuery{AfterID: 3, Type: "apikey"}, 100).Find(&rows)
		})
	})
	t.Run("all", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var rows []pluginAccount
			return pluginAccountsPage(db, "anthropic", core.PluginAccountQuery{IncludeInactive: true}, 100).Find(&rows)
		})
	})
}

package proxy

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the proxy list filter, assembled per request (see package
// gormtest).

var snapshotScope = core.DataScope{Owner: 7, IDs: []int64{3, 4}}

func TestSQLProxyList(t *testing.T) {
	creator := int64(9)
	filter := listFilter("eu", "active", &creator, snapshotScope)
	t.Run("count", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var n int64
			return db.Table("proxies p").Scopes(filter).Count(&n)
		})
	})
	t.Run("page", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var items []*Proxy
			return proxyView(db, nil).Scopes(filter).Order("p.id").Limit(20).Offset(40).Find(&items)
		})
	})
	t.Run("unfiltered", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var n int64
			return db.Table("proxies p").Scopes(listFilter("", "", nil, core.AllScope)).Count(&n)
		})
	})
}

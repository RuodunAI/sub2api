package proxy

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the proxy statements built with GORM (see package gormtest).

var snapshotScope = core.DataScope{Owner: 7, IDs: []int64{3, 4}}

func TestSQLProxyLoad(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var p Proxy
		return proxyView(scoped(db, 5, snapshotScope), []string{"openai"}).Take(&p)
	})
}

// Without a registry every account counts: the keys are NULL, not [].
func TestSQLProxyLoadAllPlugins(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var p Proxy
		return proxyView(scoped(db, 5, core.AllScope), nil).Take(&p)
	})
}

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

func TestSQLProxyLockForUpdate(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var cur row
		return scoped(db, 5, snapshotScope).Select(connectionColumns).Clauses(store.ForUpdate).Take(&cur)
	})
}

// A PATCH writes the fields present only, including a cleared password,
// and moves updated_at by clock_timestamp().
func TestSQLProxyUpdate(t *testing.T) {
	name, port, allow := "eu-1", 8080, false
	in := input{Name: &name, Port: &port, Password: new(string)}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Model(&model.Proxy{}).Where("id = ?", 5).Updates(in.changes(nil, &allow))
	})
}

// A probe writes the address it saw and leaves updated_at, the version of
// the cached client, alone.
func TestSQLProxyObserveIP(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return observeIP(db, 5, "203.0.113.7") })
}

func TestSQLProxyCreate(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(newProxy("eu", "http", "10.0.0.1", 3128, "", nil, "active", false, 7))
	})
}

func TestSQLProxyCandidates(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var found []row
		return candidates(db, Spec{Protocol: "http", Host: "proxy.example", Port: 3128, Username: "u", Password: "p"}, snapshotScope).Find(&found)
	})
}

func TestSQLProxyLifecycleSync(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Exec(lifecycleSyncSQL, store.Int64s{11, 12})
	})
}

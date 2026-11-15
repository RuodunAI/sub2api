package apikey

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the API key statements built with GORM (see package gormtest).
// The per-request lookup (lookupSQL) stays on pgx.

func TestSnapshotKeyList(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/me/api-keys?q=sk-s2a-AbCdEfGh&status=active&group_id=3", nil)
	filter, err := mineFilter(c, 7)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("count", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var n int64
			return keysWithOwner(db).Scopes(filter).Count(&n)
		})
	})
	t.Run("page", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var rows []keyRow
			return keyView(db).Scopes(filter).Order("k.id DESC").Limit(20).Offset(0).Find(&rows)
		})
	})
}

// A PATCH writes the fields present only; expires_at null clears it.
func TestSnapshotKeyUpdate(t *testing.T) {
	name, group := "ci", int64(3)
	in := keyUpdate{Name: &name, GroupID: &group, ResetQuota: true, ExpiresAt: []byte("null")}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var k model.APIKey
		return ownedUpdate(db, &k, 5, 7, in.changes())
	})
}

func TestSnapshotKeyGroupAvailable(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var ok bool
		return usableGroup(db, 7, 3).Take(&ok)
	})
}

func TestSnapshotKeyFlushLastUsed(t *testing.T) {
	at := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Exec(flushLastUsedSQL, store.Int64s{1, 2}, store.Times{at, at.Add(time.Second)})
	})
}

func TestSnapshotKeyUsageTotals(t *testing.T) {
	today := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row struct{}
		return db.Raw(usageTotalsSQL, map[string]any{"key": int64(5), "today": today}).Scan(&row)
	})
}

func TestSnapshotKeyGroupTypes(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []groupTypeRow
		return groupTypesQuery(db, []int64{3, 4}).Find(&rows)
	})
}

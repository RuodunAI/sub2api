package ccgateway

import (
	"encoding/json"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the ccgateway statements built with GORM (see package
// gormtest).

var snapshotAt = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)

// A reported fact writes its column only, never accounts.updated_at.
func TestSQLRecordAccountFact(t *testing.T) {
	for _, column := range []string{"subscription_type", "rate_limit_tier"} {
		t.Run(column, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return recordAccountFact(db, 7, column, "max") })
		})
	}
}

// The upserts of the per-account settings move updated_at by
// clock_timestamp() on conflict; an insert leaves it to the default.
func TestSQLSaveFeaturePolicy(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return saveFeaturePolicy(db, 7, "custom", json.RawMessage(`{"level":"off"}`), 3)
	})
}

func TestSQLSaveRequestLog(t *testing.T) {
	by := int64(2)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return saveRequestLog(db, &model.CcgatewayAccountRequestLog{AccountID: 7, Level: "full", FullUntil: &snapshotAt, UpdatedBy: &by})
	})
}

func TestSQLLoadAccountFeaturePolicy(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row model.CcgatewayAccountFeaturePolicy
		return db.Select("mode", "overrides", "revision").Where("account_id = ?", 7).Take(&row)
	})
}

// Without a scope and for any kind the conditions are left out, as the
// NULL / -1 parameters did.
func TestSQLTouchDraft(t *testing.T) {
	scope := int64(9)
	for name, c := range map[string]struct {
		scope *int64
		kind  int64
	}{
		"any":          {nil, anyDraft},
		"account":      {&scope, accountDraft},
		"reauthorized": {&scope, 7},
	} {
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return touchDraftQuery(db, "d0123456789abcdef", c.scope, c.kind) })
		})
	}
}

func TestSQLInsertDraft(t *testing.T) {
	proxy := int64(4)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertDraft(db, "d0123456789abcdef", &proxy, 9, 5) })
}

func TestSQLSetDraftProxy(t *testing.T) {
	scope := int64(9)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return setDraftProxy(db, "d0123456789abcdef", &scope, nil) })
}

func TestSQLAdoptDraft(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return adoptDraftQuery(db, "d0123456789abcdef", 7, nil) })
}

// A nil settings value is written as NULL, not left to the column default.
func TestSQLInsertPreparation(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return insertPreparation(db, core.AccountPreparation{Key: "d0123456789abcdef", CreatedBy: 9})
	})
}

func TestSQLPreparationMatches(t *testing.T) {
	proxy := int64(4)
	p := core.AccountPreparation{Key: "d0123456789abcdef", ProxyID: &proxy, Settings: json.RawMessage(`{"locale":"en"}`), CreatedBy: 9}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var found bool
		return store.ExistsQuery(db, preparationDraft(db, p)).Scan(&found)
	})
}

func TestSQLMarkBlocked(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markBlockedRow(db, "7", 42, snapshotAt) })
}

func TestSQLMarkAccountDeleted(t *testing.T) {
	var purgeAfter time.Time
	t.Run("retire", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return retireAccountRuntime(db, 7, 30).Scan(&purgeAfter) })
	})
	t.Run("insert", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertRetiredRuntime(db, 7, 30).Scan(&purgeAfter) })
	})
}

func TestSQLRetainedRuntimes(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []retainedRow
		return retainedQuery(db).Scan(&rows)
	})
}

func TestSQLPurgeTarget(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var r purgeTarget
		return purgeTargetQuery(db, "7").Take(&r)
	})
}

func TestSQLReauthKey(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var key string
		return reauthKeyQuery(db, 7).Take(&key)
	})
}

func TestSQLParkedRuntimes(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var ids []int64
		return parkedQuery(db, []int64{7, 8}).Pluck("a.id", &ids)
	})
}

func TestSQLAccountRuntime(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var used bool
		return accountRuntimeQuery(db, 7).Scan(&used)
	})
}

func TestSQLAccountGroups(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var groups []featureGroupRef
		return accountGroupsQuery(db, 7).Scan(&groups)
	})
}

package account

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the account statements built with GORM (see package gormtest).

var (
	snapshotScope = core.DataScope{Owner: 7, IDs: []int64{3, 4}, Groups: []int64{9}}
	snapshotTime  = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
)

// A create writes the defaulted columns the caller chose, false and 0
// included: the pointers of model.Account are never nil here.
func TestSQLAccountCreate(t *testing.T) {
	status, sched, autoDisable, prio, maxc, weight, spmWindow := "disabled", false, false, 0, 0, 1, 60
	a := model.Account{Name: "n", PluginKey: "anthropic", Type: "apikey", CredentialsEnc: []byte{1},
		Settings: json.RawMessage(`{}`), Status: &status, Schedulable: &sched, Priority: &prio, MaxConcurrency: &maxc,
		CreatedBy: nonZero(int64(0)), Weight: &weight, Models: store.Strings{}, ModelMapping: json.RawMessage(`{}`),
		AutoDisable: &autoDisable, SubscriptionType: nonZero(""), ModelLimits: json.RawMessage(`[]`),
		Environment: environmentJSON(core.EnvironmentValues{}), HeaderPassthrough: store.Strings{}, ParamOverride: rawJSON(nil),
		SpmWindowSeconds: &spmWindow}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertAccount(db, &a) })
}

// A PATCH writes the fields present, a false or 0 included, and nothing
// else but updated_at; a present nil list keeps the column.
func TestSQLAccountUpdate(t *testing.T) {
	sched, autoDisable, prio, status := false, false, 0, "active"
	in := input{Schedulable: &sched, AutoDisable: &autoDisable, Priority: &prio, Status: &status,
		HeaderPassthrough: new([]string), Environment: &core.EnvironmentValues{"relay_timeout_seconds": 60}}
	reason, locked := "", false
	set := in.columns(nil, &reason, nil, nil, &locked)
	set["proxy_id"] = (*int64)(nil)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return updateScoped(db, 5, snapshotScope, set) })
}

func TestSQLAccountLoadRow(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var a struct{}
		return rowQuery(db, 5, snapshotScope, true).Scan(&a)
	})
}

func TestSQLAccountSoftDelete(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return softDelete(db, &model.Account{}, 5, snapshotScope) })
}

func TestSQLAccountResetStatus(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return resetStatus(db, 5) })
}

func TestSQLAccountResetForReauth(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return resetAccountState(db, 5, true) })
}

func TestSQLAccountSaveFacts(t *testing.T) {
	settings := `{"email":"a@b.c"}`
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return saveFacts(db, 5, snapshotTime, true, []string{"m"}, nil, &settings)
	})
}

// last_used_at moves alone: updated_at is the version other writers check.
func TestSQLAccountFlushLastUsed(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return flushLastUsed(db, []int64{5, 6}, []time.Time{snapshotTime, snapshotTime})
	})
}

func TestSQLAccountPurge(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return purgeAccounts(db, &[]model.Account{}, "anthropic") })
}

func TestSQLAccountRenewCredentials(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return renewCredentials(db, 5, []byte{1}, []byte{2}) })
}

func TestSQLAccountWriteLastTest(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return writeLastTest(db, 5, TestResult{OK: false, Status: 401, LatencyMs: 12, Model: "m", ErrorCode: "auth", ErrorSource: "response"}, "denied")
	})
}

func TestSQLAccountCount(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var c Counts
		return countAccounts(db, snapshotScope).Scan(&c)
	})
}

func TestSQLAutomaticLock(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var a struct{}
		return lockAutomaticState(db, 5, "anthropic").Scan(&a)
	})
}

func TestSQLAutomaticWrite(t *testing.T) {
	ch := automaticChange{id: 5, to: "auto_disabled", reason: "r", until: &snapshotTime, source: usageLimitSource}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return ch.write(db) })
}

func TestSQLAutomaticDeferDisable(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return deferDisable(db, 5, false, "", nil) })
}

func TestSQLAutomaticRecoverExpired(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []recoveredAccount
		return recoverExpired(db).Scan(&rows)
	})
}

func TestSQLLifecyclePending(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return lifecyclePending(db, 5) })
}

func TestSQLLifecycleSave(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return saveLifecycle(db, 5, snapshotTime, 3, core.AccountSynchronization{Status: "applied"})
	})
}

func TestSQLUsageLimitUpsert(t *testing.T) {
	l := UsageLimit{Enabled: true, Amount: decimal.NewFromInt(10), Period: usagePeriodDay, PeriodCount: 1, Timezone: "UTC"}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return upsertUsageLimit(db, 5, l) })
}

// An early release leaves updated_at, the configuration's version, alone.
func TestSQLUsageLimitWaive(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return waive(db, 5, snapshotTime) })
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

func TestSQLPreparationTouch(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return touchPreparation(db, &model.AccountPreparation{}, "k", 7)
	})
}

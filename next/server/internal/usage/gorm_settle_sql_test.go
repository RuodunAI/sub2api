package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the settlement and task statements built with GORM (see
// package gormtest). Every UPDATE of async_tasks, which has an updated_at
// column, has one: only releaseTaskClaim writes it (now()), as the raw SQL
// did.

var settleSnapshotTime = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// The per-request write of a settled row: request_id is the last argument.
func TestSQLSettleBillUsageRow(t *testing.T) {
	p := &pending{RequestID: "req-1", PriceID: 3, Mode: "expression", Rate: decimal.NewFromInt(1)}
	cost := amounts{base: decimal.RequireFromString("0.5"), list: decimal.RequireFromString("0.5"), total: decimal.RequireFromString("0.6")}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return billUsageRow(db, p, cost, BillingDetail{Tier: "default"}, "hash")
	})
}

// A failed settlement merges its patch into billing_detail and only moves a
// row that is still pending or failed.
func TestSQLSettleMarkFailed(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return markUsageFailed(db, "req-1", []byte(`{"attempts":2}`))
	})
}

// The snapshot is one bytea argument, used twice, not a list of its bytes;
// updated_at is now(), as before.
func TestSQLTaskReleaseClaim(t *testing.T) {
	e := &settleEntry{task: &taskClaim{id: "s2task_1", failureCode: "upstream", failureReason: "boom"}, pollFailed: true}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return releaseTaskClaim(db, e, "succeeded", "closed", []byte(`{"id":"s2task_1"}`), []byte(`["id"]`))
	})
}

// No snapshot: NULL, so the stored one stays.
func TestSQLTaskReleaseClaimPending(t *testing.T) {
	e := &settleEntry{task: &taskClaim{id: "s2task_1"}, pollObserved: true}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return releaseTaskClaim(db, e, "pending", "pending", nil, []byte(`[]`))
	})
}

// The claim leases the rows and leaves updated_at alone.
func TestSQLTaskClaimDue(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []struct{}
		return claimDueTasks(db, 100, "token-1").Scan(&rows)
	})
}

// Rescheduling leaves updated_at alone.
func TestSQLTaskRescheduleTask(t *testing.T) {
	e := &settleEntry{id: 4, task: &taskClaim{id: "s2task_1"}, pollFailed: true}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return rescheduleRow(db, "async_tasks", "public_id", e.task.id, e, 1, settleSnapshotTime, "busy")
	})
}

func TestSQLTaskRescheduleEntry(t *testing.T) {
	e := &settleEntry{id: 4, task: &taskClaim{id: "s2task_1"}}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return rescheduleRow(db, "pending_settlements", "id", e.id, e, 0, settleSnapshotTime, "")
	})
}

// The import copies the counters and leaves updated_at alone.
func TestSQLTaskImportCounters(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return importTaskCounters(db, "s2task_1", 4) })
}

// An empty snapshot stays one empty bytea, as pgx bound it, and does not
// become NULL (which COALESCE would read as "keep the stored snapshot"):
// right after "(" GORM turns an empty []byte into NULL and spreads a longer
// one into a list, a store.Bytes it binds as is (TestGORMBytes in package
// store checks the values PostgreSQL receives).
func TestSQLTaskReleaseClaimEmptySnapshot(t *testing.T) {
	e := &settleEntry{task: &taskClaim{id: "s2task_1"}}
	_, args, err := gormtest.Build(func(db *gorm.DB) *gorm.DB {
		return releaseTaskClaim(db, e, "succeeded", "closed", []byte{}, []byte(`[]`))
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := args[2].(store.Bytes)
	if !ok || len(args) != 15 {
		t.Fatalf("arguments = %#v, want fifteen with a store.Bytes snapshot", args)
	}
	if v, _ := snapshot.Value(); v == nil || len(v.([]byte)) != 0 {
		t.Fatalf("snapshot value = %#v, want a non-nil empty []byte", v)
	}
}

// The lazy import of a legacy task: the usage rows locked first, then the
// settlement entries, then the imported task.
func TestSQLTaskImportLegacy(t *testing.T) {
	q := core.TaskQuery{PluginKey: "video", Kind: "generate", UserID: 1, GroupID: 3, ID: "up-1", SubmitProtocol: "p", IDPaths: []string{"id"}}
	t.Run("owner", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return legacyTaskEntries(db, q, "u.id", "u").Scan(&sqlSink) })
	})
	t.Run("entries", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return legacyTaskEntries(db, q, legacyEntryColumns, "p").Scan(&sqlSink) })
	})
	t.Run("insert", func(t *testing.T) {
		account := int64(4)
		e := &legacyEntry{id: 9, usageLogID: 5, apiKeyID: 2, accountID: &account, model: "m",
			next: settleSnapshotTime, deadline: settleSnapshotTime.Add(time.Hour), created: settleSnapshotTime.Add(-time.Hour)}
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertLegacyTask(db, "s2task_1", q, e) })
	})
}

// The reconcile loop's statements (reconcile.go). pending_settlements and
// usage_logs have no updated_at.
func TestSQLReconcile(t *testing.T) {
	e := &settleEntry{id: 9, usageLogID: 5, pollFailed: true}
	t.Run("claim_due", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return claimDueEntries(db, 100).Scan(&sqlSink) })
	})
	t.Run("reschedule", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return rescheduleEntry(db, e, 1, settleSnapshotTime, "busy") })
	})
	t.Run("close", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return closeEntryRow(db, 9, SettleStateSettled, "usage row left the reserved state")
		})
	})
	t.Run("settle_row", func(t *testing.T) {
		p := &pending{Tokens: core.UsageTokens{Input: 10, Output: 20, CacheRead: 30, CacheCreation: 40, CacheCreation1h: 5},
			Metrics: map[string]any{"seconds": 8}}
		cost := amounts{base: decimal.RequireFromString("0.5"), total: decimal.RequireFromString("0.6")}
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return repriceUsageRow(db, 5, p, cost, BillingDetail{Tier: "default"}, "hash")
		})
	})
	t.Run("settle_entry", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return settleEntryRow(db, 9) })
	})
	t.Run("refund_row", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return refundUsageRow(db, 5, "upstream failed") })
	})
	t.Run("refund_entry", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return failEntryRow(db, e, "upstream failed") })
	})
	// keepEstimate: the row is billed at its estimate with the marker merged
	// into its anomalies (closeUnreconcilable writes the same statement).
	t.Run("estimate_row", func(t *testing.T) {
		marker := map[string]string{core.AnomalyReconcile: core.ReconcileAbandoned, core.AnomalyReconcileError: "deadline"}
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markUsageRow(db, 5, StatusBilled, nil, marker) })
	})
	t.Run("estimate_entry", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return estimateEntryRow(db, 9, SettleStateAbandoned, "deadline", false) })
	})
	t.Run("estimate_entry_attempt", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return estimateEntryRow(db, 9, SettleStateEstimated, "no usage", true) })
	})
}

// The hourly rollup's insert keeps jsonb's ? operator as text and binds
// the two bounds by number.
func TestSQLRollupInsert(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return rollupInsert(db, settleSnapshotTime, settleSnapshotTime.Add(time.Hour))
	})
}

// The helper history outbox's persistence: the advisory lock, the locked
// reads of the receipt and of the usage row, and the receipt.
func TestSQLHelperUsage(t *testing.T) {
	t.Run("lock", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return lockHelperUsage(db, "req-1") })
	})
	t.Run("receipt", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return helperUsageReceipt(db, "req-1").Scan(&sqlSink) })
	})
	t.Run("usage_row", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return lockedUsageID(db, "req-1").Scan(&sqlSink) })
	})
	t.Run("insert_receipt", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Create(&model.ProviderHelperUsageReceipt{RequestID: "req-1", Digest: "d"})
		})
	})
}

// lockSettlement's first lock: the tasks of the usage row, before the row.
func TestSQLLockTasksOfUsage(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return lockTasksOfUsage(db, 5) })
}

// The usage_logs insert of one record (insertUsageSQL[0]): each value goes
// to its column, the documents are one bytea parameter each, and a native
// request's conversion is NULL. A chunk of 2^k rows repeats the row.
func TestSQLInsertUsageRow(t *testing.T) {
	account := int64(7)
	rec := &core.UsageRecord{RequestID: "req-1", UserID: 3, APIKeyID: 4, GroupID: 5, AccountID: &account, PluginKey: "anthropic",
		Platform: "anthropic", Protocol: "anthropic.messages", Endpoint: "/v1/messages", Model: "claude-sonnet-4-5",
		StatusCode: 200, Success: true, Attempts: 1, UsageSemantics: "exclusive", Billable: true,
		Tokens: core.UsageTokens{Input: 1200, Output: 800}, RateMultiplier: decimal.NewFromInt(1),
		Price: &core.PriceRule{ID: 9, Mode: "expression", ExprHash: "hash"}, CreatedAt: settleSnapshotTime}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Raw(insertUsageSQL[0], appendUsageRow(nil, rec)...).Scan(&sqlSink)
	})
	if n := strings.Count(insertUsageSQL[7], "?"); n != 128*usageLogValues || len(appendUsageRow(nil, rec)) != usageLogValues {
		t.Fatalf("%d placeholders in the 128-row insert", n)
	}
}

// settleTx's lock of the usage row; the retry loop skips a locked row.
func TestSQLLockPendingRow(t *testing.T) {
	for name, skip := range map[string]bool{"lock": false, "skip_locked": true} {
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return lockPendingRow(db, "req-1", skip).Scan(&sqlSink) })
		})
	}
}

// A reservation's reconcile entry: ON CONFLICT on the plugin's ref_id
// returns no row, which closes the usage row.
func TestSQLInsertSettlementEntry(t *testing.T) {
	rv := &core.UsageReservation{PluginKey: "video", RefID: "job-1"}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return insertSettlementEntry(db, rv, 5, nil, settleSnapshotTime, settleSnapshotTime.Add(time.Hour)).Scan(&sqlSink)
	})
}

// A client's poll: the task id binds three times, as $5 did on pgx.
func TestSQLTaskByID(t *testing.T) {
	q := core.TaskQuery{PluginKey: "video", Kind: "video", UserID: 3, GroupID: 5, ID: "s2task_1"}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return taskByID(db, q).Scan(&sqlSink) })
}

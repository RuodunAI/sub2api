package usage

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
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

// The body is one bytea argument, not a list of its bytes.
func TestSQLTaskReceive(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return receiveTask(db, "req-1", "received", 200, []byte(`{"id":"up-1"}`), "")
	})
}

// An empty body stays an empty bytea, as pgx bound it, and does not become
// NULL (which COALESCE would read as "keep the stored body"): right after
// "(" GORM would have turned the empty slice into NULL.
func TestSQLTaskReceiveEmptyBody(t *testing.T) {
	_, args, err := gormtest.Build(func(db *gorm.DB) *gorm.DB {
		return receiveTask(db, "req-1", "received", 204, []byte{}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if body, ok := args[2].([]byte); !ok || body == nil {
		t.Fatalf("body argument = %#v, want a non-nil empty []byte", args[2])
	}
}

// The receipt writes the identity columns alone; the database fills the
// state, expiry and timestamps.
func TestSQLTaskInsertReceipt(t *testing.T) {
	in := core.TaskIntent{RequestID: "req-1", PluginKey: "video", Kind: "generate", Model: "m", Protocol: "p",
		UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertReceipt(db, "s2task_1", in) })
}

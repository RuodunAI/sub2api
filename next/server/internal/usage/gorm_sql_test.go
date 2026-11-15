package usage

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the console and query statements of the package built with
// GORM (see package gormtest). The filtered usage reads keep store.Filter's
// numbered conditions and go through store.RawNumbered; the per-request execution
// statements (BeginExecution, ObserveExecution, the commit's lock and mark)
// stay on pgx (BenchmarkHotExecution).

// sqlFilter is parseFilter over query, for an operator with account:read
// (self nil) or for the user self.
func sqlFilter(t *testing.T, query string, self *int64) *store.Filter {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/usage?"+query, nil)
	f, err := parseFilter(c, self, self == nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// sqlSink stands for the row a statement reads, which a snapshot does not see.
var sqlSink struct{}

func TestSQLUsageList(t *testing.T) {
	f := sqlFilter(t, "model=claude&api_key_name=ci_%25&success=true&from=2026-11-01T00:00:00Z&to=2026-11-15T00:00:00Z", nil)
	t.Run("count", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return store.RawNumbered(db, `SELECT count(*) FROM usage_logs u`+f.Cond(), f.Args()...).Scan(&sqlSink)
		})
	})
	t.Run("page", func(t *testing.T) {
		n := f.Len()
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return recordQuery(db, f.Cond(), append(f.Args(), 20, 40),
				` ORDER BY u.created_at DESC, u.id DESC LIMIT $`+strconv.Itoa(n+1)+` OFFSET $`+strconv.Itoa(n+2)).Scan(&sqlSink)
		})
	})
	t.Run("keyset", func(t *testing.T) {
		self := int64(7)
		k := sqlFilter(t, "request_id=r1&from=2026-11-01T00:00:00Z", &self)
		k.And("(u.created_at, u.id) < (?, ?)", time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), int64(99))
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return recordQuery(db, k.Cond(), append(k.Args(), 21, 0),
				` ORDER BY u.created_at DESC, u.id DESC LIMIT $`+strconv.Itoa(k.Len()+1)+` OFFSET $`+strconv.Itoa(k.Len()+2)).Scan(&sqlSink)
		})
	})
}

func TestSQLUsageExportPage(t *testing.T) {
	f := sqlFilter(t, "conversion=lossy&user_email=a@b", nil)
	f.And("(u.created_at, u.id) < (?, ?)", time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), int64(99))
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return recordQuery(db, f.Cond(), f.Args(), ` ORDER BY u.created_at DESC, u.id DESC LIMIT 1001`).Scan(&sqlSink)
	})
}

func TestSQLUsageDetail(t *testing.T) {
	t.Run("record", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return detailQuery(db, 5).Scan(&sqlSink) })
	})
	t.Run("price", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Model(&model.ModelPrice{}).Select("id, model, source").Where("id = ?", int64(3)).Scan(&sqlSink)
		})
	})
	t.Run("ledger", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Model(&model.BalanceLedger{}).Select("id").Where("idempotency_key = ?", "usage:r1").Scan(&sqlSink)
		})
	})
	t.Run("account_names", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Model(&model.Account{}).Select("id, name").Where("id = ANY(?)", store.Int64s{1, 2}).Scan(&sqlSink)
		})
	})
	t.Run("settlement", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return settlementOf(db, 5).Scan(&sqlSink) })
	})
}

// A day summary partly read from the rollup: statRows repeats the filter in
// every part of its UNION, so the filter's values are bound once per part.
func TestSQLUsageSummaryRollup(t *testing.T) {
	f := sqlFilter(t, "group_id=3&from=2026-10-16T00:00:00Z", nil)
	args := f.Args()
	key := `to_char(u.created_at AT TIME ZONE ` + addArg(&args, "Asia/Shanghai") + `::text, 'YYYY-MM-DD')`
	sh := summaryShape{outKey: `u.k`, key: key, extra: `''`, lastAt: `NULL::timestamptz`, groupBy: ` GROUP BY k`, order: ` ORDER BY k`}
	span := &hourSpan{from: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), to: time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return summaryQuery(db, sh, f.Cond(), span, args).Scan(&sqlSink)
	})
}

func TestSQLUsageRealtime(t *testing.T) {
	self := int64(7)
	f := sqlFilter(t, "model=claude", &self)
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	f.And("u.created_at >= ?", now.Add(-realtimeScan))
	f.And("u.created_at + u.latency_ms * interval '1 millisecond' >= ?", now.Add(-realtimeWindow))
	f.And("u.protocol NOT LIKE '%.count_tokens'")
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return realtimeQuery(db, f).Scan(&sqlSink) })
}

func TestSQLUsageOverviewCounts(t *testing.T) {
	t.Run("api_keys", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return apiKeyCounts(db, 7).Scan(&sqlSink) })
	})
	t.Run("users", func(t *testing.T) {
		today := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return userCounts(db, today).Scan(&sqlSink) })
	})
}

func TestSQLUsageExportAudit(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(exportAudit(7, json.RawMessage(`{"rows":3}`), "203.0.113.9"))
	})
}

// The manual reconcile and refund (settlements_api.go) and the monitor's
// pending reschedule: none of these tables has an updated_at.
func TestSQLSettlementActions(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	t.Run("lock_entry", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return settlementOf(db, 5).Clauses(store.ForUpdate).Scan(&sqlSink) })
	})
	t.Run("retry_row", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markUsageRow(db, 5, "reserved", nil, reconcileMarker("retrying")) })
	})
	t.Run("retry_entry", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return settlementByID(db, 9).Updates(map[string]any{"state": "pending", "attempts": 0, "poll_failures": 0,
				"next_check_at": now, "deadline_at": now.Add(time.Hour), "last_error": ""})
		})
	})
	t.Run("retry_task", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return tasksOfUsage(db, 5).Updates(map[string]any{"observation_status": "pending", "claim_token": "", "lease_until": nil,
				"attempts": 0, "poll_failures": 0, "failure_code": "", "next_check_at": now, "deadline_at": now.Add(time.Hour),
				"last_error": ""})
		})
	})
	t.Run("refund_row", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return markUsageRow(db, 5, "free", map[string]any{"total_cost": decimal.Zero, "base_cost": decimal.Zero}, reconcileMarker("refunded"))
		})
	})
	t.Run("refund_task", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return tasksOfUsage(db, 5).Updates(map[string]any{"claim_token": "", "lease_until": nil})
		})
	})
	t.Run("refund_entry", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return settlementByID(db, 9).Update("last_error", "refunded by an administrator: x")
		})
	})
	t.Run("monitor_pending", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return pendingByID(db, 9).Updates(map[string]any{
				"attempts": gorm.Expr("attempts+1"), "poll_failures": 0, "next_check_at": now, "last_error": ""})
		})
	})
	t.Run("monitor_receipt", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Create(&model.PluginMonitorReceipt{ID: "task:token", Digest: "d", TaskID: ""})
		})
	})
}

// Execution recovery and retention. plugin_executions has an updated_at:
// the rotation writes it as the raw SQL did, with clock_timestamp().
func TestSQLExecutionRecovery(t *testing.T) {
	t.Run("stalled", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return stalledExecutions(db).Scan(&sqlSink) })
	})
	t.Run("rotate", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return rotateExecution(db, "exec-1") })
	})
	t.Run("expire", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return expireReceipts(db, "plugin_monitor_receipts") })
	})
}

package billing

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/billing/expr"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the billing statements built with GORM (see package gormtest),
// the ledger's writes of the request path among them (TestSQLLedger).

var snapshotAt = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)

func ledgerFilter() store.Conds {
	var f store.Conds
	f.And("l.user_id = ?", int64(7))
	f.And("l.kind = ?", KindUsage)
	f.And("l.created_at >= ?", snapshotAt)
	return f
}

func TestSQLLedgerCount(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var total int64
		return ledgerRows(db, ledgerFilter()).Count(&total)
	})
}

func TestSQLMyLedgerPage(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var items []LedgerEntrySelf
		return myLedgerPage(db, ledgerFilter(), 3, 20).Scan(&items)
	})
}

func TestSQLLedgerPage(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var items []LedgerEntry
		return ledgerPage(db, nil, 1, 50).Scan(&items)
	})
}

func TestSQLBalance(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row struct{ Balance, LastID int64 }
		return db.Raw(balanceSQL, int64(7), int64(7)).Scan(&row)
	})
}

// A duplicate is read by its idempotency key, the unique key the ledger's
// ON CONFLICT names.
func TestSQLLedgerByKey(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row model.BalanceLedger
		return ledgerByKey(db, "precharge:req-1").Take(&row)
	})
}

func TestSQLExpiredPrecharges(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []model.RequestPrecharge
		return expiredPrecharges(db).Scan(&rows)
	})
}

func TestSQLPricedGroups(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []pricedGroupRow
		return pricedGroups(db, usableBy+" AND g.id = ?", int64(7), int64(3)).Scan(&rows)
	})
}

func TestSQLEnabledPrices(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []model.ModelPrice
		return enabledPrices(db, "model", "mode", "config", "expression").Find(&rows)
	})
}

// The rules of a group for a segment: the segment id goes in twice where
// the pgx statement named $2 twice.
func TestSQLPriceRules(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []ruleRow
		return priceRules(db, rulesFor, int64(3), int64(5), int64(5)).Scan(&rows)
	})
}

func TestSQLAllPriceRules(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []ruleRow
		return priceRules(db, "").Scan(&rows)
	})
}

// An edit locks the price row only (FOR UPDATE OF mp), not its source.
func TestSQLPriceByID(t *testing.T) {
	for name, lock := range map[string]bool{"read": false, "lock": true} {
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
				var row priceRow
				return priceByID(db, 9, lock).Take(&row)
			})
		})
	}
}

func TestSQLPricePage(t *testing.T) {
	var f store.Conds
	f.And("mp.enabled = ?", false)
	f.And("(mp.model ILIKE ? OR mp.note ILIKE ?)", "%claude%", "%claude%")
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []priceRow
		return pricePage(db, f, 2, 25).Scan(&rows)
	})
}

func testProgram(t *testing.T) *expr.Program {
	t.Helper()
	prog, err := expr.CompileCached("flat(0.5)")
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

// A disabled price writes enabled = false (the column defaults to true) and
// its expr_version; updated_at is left to the default.
func TestSQLManualPrice(t *testing.T) {
	prog := testProgram(t)
	by := int64(2)
	ck := &checked{mode: expr.ModeExpression, config: json.RawMessage(`{}`), src: prog.Expression(), prog: prog}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(manualPrice("claude-sonnet-4-5", ck, false, "", &by))
	})
}

// An edit of a price writes every column it names and moves updated_at by
// now(); a cleared sync source is NULL.
func TestSQLUpdatePriceRow(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var syncID *int64
		return updatePriceRow(db, 9, map[string]any{
			"model": "claude-sonnet-4-5", "mode": expr.ModePerToken, "config": json.RawMessage(`{"p":3}`),
			"expression": "tokens(3, 15)", "expr_version": 1, "expr_hash": "h", "enabled": false, "note": "",
			"updated_by": nullID(2), "source": SourceManual, "sync_source_id": syncID,
		})
	})
}

// The history insert, on its own and before a custom group price.
func TestSQLInsertHistory(t *testing.T) {
	prog := testProgram(t)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertHistory(db, prog) })
}

// The rule changes of a PUT, one statement of array parameters: a group
// price and a segment price saved (each upserted on the partial unique
// index of its level, the update moving updated_at by now()), one removed,
// and one removed and saved again (two changes of one rule). A multiplier
// has no config (NULL) and no mode (NULLIF to NULL); the custom price's
// expression also goes to the history. The custom price's config holds no
// number, whose pointer the snapshot would print.
func TestSQLWriteRules(t *testing.T) {
	mult := "1.25"
	group := core.PriceQuery{GroupID: 3, Model: "claude-sonnet-4-5"}
	segment := core.PriceQuery{GroupID: 3, SegmentID: 5, Model: "claude-sonnet-4-5"}
	gone := core.PriceQuery{GroupID: 3, SegmentID: 5, Model: "claude-haiku-4-5"}
	again := core.PriceQuery{GroupID: 3, Model: "claude-opus-4-5"}
	multiplier := &storedRule{Rule: Rule{Kind: core.PriceRuleMultiplier, Multiplier: &mult}}
	custom, err := checkCustom(context.Background(), Rule{Kind: core.PriceRuleCustom, Mode: "per_request",
		Config: map[string]*float64{"price": new(float64)}}, "per_request")
	if err != nil {
		t.Fatal(err)
	}
	custom.Config = map[string]*float64{"price": nil}
	changes := []ruleChange{{segment, custom}, {group, multiplier}, {gone, nil}, {again, nil}, {again, multiplier}}
	slices.SortStableFunc(changes, func(a, b ruleChange) int {
		return cmp.Or(cmp.Compare(a.on.SegmentID, b.on.SegmentID), cmp.Compare(a.on.Model, b.on.Model))
	})
	w, err := collapseChanges(changes)
	if err != nil {
		t.Fatal(err)
	}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return writeRules(db, w, 2) })
}

// The ledger statements of the request path: the CTEs bind each use of a
// numbered parameter on its own, with its cast, so the arguments repeat in
// the order the text uses them.
func TestSQLLedger(t *testing.T) {
	operator := int64(9)
	amount, min := decimal.RequireFromString("0.5"), decimal.RequireFromString("0")
	var row struct{ ID int64 }
	cases := map[string]func(*gorm.DB) *gorm.DB{
		"ensure_balance": func(db *gorm.DB) *gorm.DB { return db.Exec(ensureBalanceSQL, int64(7)) },
		"apply": func(db *gorm.DB) *gorm.DB {
			return applyStmt.Raw(db, int64(7), amount.Neg(), KindAdminAdjust, "manual", "ref-1", "adjust:1", &operator, "", "note").Scan(&row)
		},
		"release": func(db *gorm.DB) *gorm.DB {
			return releaseStmt.Raw(db, "req-1", int64(7), releaseKey("req-1"), "release request precharge").Scan(&row)
		},
		"precharge": func(db *gorm.DB) *gorm.DB {
			return prechargeStmt.Raw(db, int64(7), "req-1", amount, KindUsage, "precharge", "req-1", "precharge:req-1", "request precharge", min).Scan(&row)
		},
		// A precharge's balance row, behind its lock timeout.
		"precharge_ensure": func(db *gorm.DB) *gorm.DB { return db.Exec(prechargeEnsureSQL, int64(7)) },
		"settle": func(db *gorm.DB) *gorm.DB {
			ch := core.LedgerChange{UserID: 7, Amount: amount, Kind: KindUsage, RefType: "usage", RefID: "req-1", IdempotencyKey: "usage:req-1", Note: "note"}
			return settleStmt.Raw(db, settleArgs(7, "req-1", &ch)...).Scan(&row)
		},
		// Without a charge the charge's arguments are inert: $5 is false.
		"settle_release_only": func(db *gorm.DB) *gorm.DB {
			return settleStmt.Raw(db, settleArgs(7, "req-1", nil)...).Scan(&row)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) { gormtest.Assert(t, build) })
	}
}

func TestSQLSource(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row sourceRow
		return sources(db).Where("ps.id = ?", 4).Take(&row)
	})
}

// A disabled source without a key writes enabled = false and a NULL key.
func TestSQLCreateSource(t *testing.T) {
	enabled := false
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(&model.PriceSyncSource{Name: "litellm", Kind: KindLiteLLM, URL: "https://example.invalid/prices.json",
			Options: json.RawMessage(`{}`), Enabled: &enabled})
	})
}

func TestSQLUpdateSourceRow(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var enc []byte
		return updateSourceRow(db, 4, map[string]any{
			"name": "litellm", "kind": KindLiteLLM, "url": "https://example.invalid/prices.json",
			"api_key_enc": enc, "options": json.RawMessage(`{}`), "enabled": false,
		})
	})
}

// The outcome of a fetch or an import never moves updated_at.
func TestSQLSetSourceStatus(t *testing.T) {
	for name, set := range map[string]map[string]any{
		"error":  {"last_error": "fetch failed"},
		"synced": {"last_synced_at": gorm.Expr("now()"), "last_error": ""},
	} {
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return setSourceStatus(db, 4, set) })
		})
	}
}

func TestSQLInsertSyncedPrice(t *testing.T) {
	p := &syncPrice{Model: "gpt-5", Mode: expr.ModePerToken, Config: json.RawMessage(`{"p":1.25,"c":10}`),
		Expression: "tokens(1.25, 10)", hash: "h", version: 1}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertSyncedPrice(db, "gpt-5", p, 4, nil) })
}

func TestSQLAuditSync(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return auditSync(db, 0, 4, []byte(`{"created":1}`))
	})
}

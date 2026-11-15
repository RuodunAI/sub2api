package usage

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// The statements the settler runs for every billable request, each measured
// next to the GORM form of the same statement against a real PostgreSQL
// (TEST_DATABASE_URL), as recorded in internal/store/gormtest/BASELINE.md
// (Batch 4b):
//
//	go test -run '^$' -bench HotStmt -benchmem -benchtime 1000x -count 5 ./internal/usage/
//
// settleTx locks the row and writes its billing fields; reserveTx (a plugin
// reservation) writes the estimate and registers the reconcile entry. The
// GORM forms are the production statements (lockPendingRow, billUsageRow,
// reserveUsageRow, insertSettlementEntry); the pgx forms are the ones they
// had before. The usage_logs insert is BenchmarkHotSettlerInsert* and the
// ledger statements belong to package billing; neither is measured here.

// stmtFixture is one pending usage row and its id.
func stmtFixture(b *testing.B) (*benchSettler, string, int64) {
	s := newBenchSettler(b)
	rec := s.record(0, true)
	s.insert(b, rec)
	var id int64
	if err := s.svc.db.SQL.QueryRowContext(context.Background(), `SELECT id FROM usage_logs WHERE request_id = $1`, rec.RequestID).Scan(&id); err != nil {
		b.Fatal(err)
	}
	return s, rec.RequestID, id
}

// benchStmt runs the raw database/sql (formerly pgx) and the GORM form of one statement; i counts the
// calls of each form across the framework's reruns, for statements that need
// a fresh key each time.
func benchStmt(b *testing.B, sqlPath, gormPath func(i int) error) {
	for _, p := range []struct {
		name string
		run  func(int) error
	}{{"sql", sqlPath}, {"gorm", gormPath}} {
		calls := 0
		next := func() int { calls++; return calls }
		b.Run(p.name, func(b *testing.B) {
			if err := p.run(next()); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := p.run(next()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// settleTx's row lock.
func BenchmarkHotStmtSettleLock(b *testing.B) {
	s, requestID, _ := stmtFixture(b)
	ctx := context.Background()
	pool, raw := s.svc.db.Gorm, s.svc.db.SQL
	benchStmt(b, func(int) error {
		var status string
		return raw.QueryRowContext(ctx, `SELECT billing_status FROM usage_logs WHERE request_id = $1 FOR UPDATE`, requestID).Scan(&status)
	}, func(int) error {
		var status string
		return lockPendingRow(store.G(ctx, pool), requestID, false).Row().Scan(&status)
	})
}

// settleTx's write of the billing fields (billUsageRow).
func BenchmarkHotStmtSettleUpdate(b *testing.B) {
	s, requestID, _ := stmtFixture(b)
	ctx := context.Background()
	pool, raw := s.svc.db.Gorm, s.svc.db.SQL
	total, rate, base := decimal.RequireFromString("0.012345"), decimal.NewFromInt(1), decimal.RequireFromString("0.012345")
	bd := BillingDetail{Tier: "default", Cost: base, TotalCost: total, RateMultiplier: rate}
	p := &pending{RequestID: requestID, PriceID: s.price.ID, Mode: s.price.Mode, Rate: rate}
	benchStmt(b, func(int) error {
		_, err := raw.ExecContext(ctx, `
			UPDATE usage_logs SET total_cost = $2, billing_status = 'billed', billing_detail = $3, matched_tier = $4,
				expr_hash = $5, billing_mode = $6, price_id = $7, rate_multiplier = $8, base_cost = $9
			WHERE request_id = $1`,
			requestID, total, jsonOr(bd, "{}"), trunc(bd.Tier, 100), s.price.ExprHash, p.Mode, &p.PriceID, rate, base)
		return err
	}, func(int) error {
		return billUsageRow(store.G(ctx, pool), p, amounts{total: total, base: base}, bd, s.price.ExprHash).Error
	})
}

// reserveTx's write of the estimate (reserveUsageRow).
func BenchmarkHotStmtReserveUpdate(b *testing.B) {
	s, _, id := stmtFixture(b)
	ctx := context.Background()
	pool, raw := s.svc.db.Gorm, s.svc.db.SQL
	total, base := decimal.RequireFromString("0.5"), decimal.RequireFromString("0.5")
	bd := BillingDetail{Tier: "default", Cost: base, TotalCost: total}
	benchStmt(b, func(int) error {
		_, err := raw.ExecContext(ctx, `
		UPDATE usage_logs SET total_cost = $2, base_cost = $5, billing_detail = $3, matched_tier = $4
		WHERE id = $1`, id, total, jsonOr(bd, "{}"), trunc(bd.Tier, 100), base)
		return err
	}, func(int) error {
		return reserveUsageRow(store.G(ctx, pool), id, amounts{total: total, base: base}, bd).Error
	})
}

// reserveTx's registration of the reconcile entry, a new ref_id each time.
func BenchmarkHotStmtReserveEntry(b *testing.B) {
	s, _, id := stmtFixture(b)
	ctx := context.Background()
	pool, raw := s.svc.db.Gorm, s.svc.db.SQL
	at := time.Now().Add(time.Hour)
	benchStmt(b, func(i int) error {
		var entryID int64
		return raw.QueryRowContext(ctx, `
		INSERT INTO pending_settlements (plugin_key, ref_id, usage_log_id, account_id, next_check_at, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (plugin_key, ref_id) DO NOTHING
		RETURNING id`,
			"bench", "pgx-"+strconv.Itoa(i), id, (*int64)(nil), at, at).Scan(&entryID)
	}, func(i int) error {
		var entryID int64
		rv := &core.UsageReservation{PluginKey: "bench", RefID: "gorm-" + strconv.Itoa(i)}
		return insertSettlementEntry(store.G(ctx, pool), rv, id, nil, at, at).Row().Scan(&entryID)
	})
}

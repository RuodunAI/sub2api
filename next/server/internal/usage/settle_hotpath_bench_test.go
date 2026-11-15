package usage

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"

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
// usage_logs insert is a pgx.Batch and the ledger statements belong to
// package billing; neither is measured here.

// stmtFixture is one pending usage row and its id.
func stmtFixture(b *testing.B) (*benchSettler, string, int64) {
	s := newBenchSettler(b)
	rec := s.record(0, true)
	s.insert(b, rec)
	var id int64
	if err := s.svc.db.Pool.QueryRow(context.Background(), `SELECT id FROM usage_logs WHERE request_id = $1`, rec.RequestID).Scan(&id); err != nil {
		b.Fatal(err)
	}
	return s, rec.RequestID, id
}

// benchStmt runs the pgx and the GORM form of one statement; i counts the
// calls of each form across the framework's reruns, for statements that need
// a fresh key each time.
func benchStmt(b *testing.B, pgxPath, gormPath func(i int) error) {
	for _, p := range []struct {
		name string
		run  func(int) error
	}{{"pgx", pgxPath}, {"gorm", gormPath}} {
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
	pool := s.svc.db.Pool
	benchStmt(b, func(int) error {
		var status string
		return pool.QueryRow(ctx, `SELECT billing_status FROM usage_logs WHERE request_id = $1 FOR UPDATE`, requestID).Scan(&status)
	}, func(int) error {
		var status string
		return store.G(ctx, pool).Raw(`SELECT billing_status FROM usage_logs WHERE request_id = ? FOR UPDATE`, requestID).Row().Scan(&status)
	})
}

// settleTx's write of the billing fields.
func BenchmarkHotStmtSettleUpdate(b *testing.B) {
	s, requestID, _ := stmtFixture(b)
	ctx := context.Background()
	pool := s.svc.db.Pool
	total, rate, base := decimal.RequireFromString("0.012345"), decimal.NewFromInt(1), decimal.RequireFromString("0.012345")
	detail := jsonOr(BillingDetail{Tier: "default", Cost: base, TotalCost: total, RateMultiplier: rate}, "{}")
	priceID := s.price.ID
	benchStmt(b, func(int) error {
		_, err := pool.Exec(ctx, `
			UPDATE usage_logs SET total_cost = $2, billing_status = 'billed', billing_detail = $3, matched_tier = $4,
				expr_hash = $5, billing_mode = $6, price_id = $7, rate_multiplier = $8, base_cost = $9
			WHERE request_id = $1`,
			requestID, total, detail, "default", s.price.ExprHash, s.price.Mode, &priceID, rate, base)
		return err
	}, func(int) error {
		return store.G(ctx, pool).Exec(`
			UPDATE usage_logs SET total_cost = ?, billing_status = 'billed', billing_detail = ?, matched_tier = ?,
				expr_hash = ?, billing_mode = ?, price_id = ?, rate_multiplier = ?, base_cost = ?
			WHERE request_id = ?`,
			total, detail, "default", s.price.ExprHash, s.price.Mode, &priceID, rate, base, requestID).Error
	})
}

// reserveTx's write of the estimate.
func BenchmarkHotStmtReserveUpdate(b *testing.B) {
	s, _, id := stmtFixture(b)
	ctx := context.Background()
	pool := s.svc.db.Pool
	total, base := decimal.RequireFromString("0.5"), decimal.RequireFromString("0.5")
	detail := jsonOr(BillingDetail{Tier: "default", Cost: base, TotalCost: total}, "{}")
	benchStmt(b, func(int) error {
		_, err := pool.Exec(ctx, `
		UPDATE usage_logs SET total_cost = $2, base_cost = $5, billing_detail = $3, matched_tier = $4
		WHERE id = $1`, id, total, detail, "default", base)
		return err
	}, func(int) error {
		return store.G(ctx, pool).Exec(`
		UPDATE usage_logs SET total_cost = ?, base_cost = ?, billing_detail = ?, matched_tier = ?
		WHERE id = ?`, total, base, detail, "default", id).Error
	})
}

// reserveTx's registration of the reconcile entry, a new ref_id each time.
func BenchmarkHotStmtReserveEntry(b *testing.B) {
	s, _, id := stmtFixture(b)
	ctx := context.Background()
	pool := s.svc.db.Pool
	at := time.Now().Add(time.Hour)
	benchStmt(b, func(i int) error {
		var entryID int64
		return pool.QueryRow(ctx, `
		INSERT INTO pending_settlements (plugin_key, ref_id, usage_log_id, account_id, next_check_at, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (plugin_key, ref_id) DO NOTHING
		RETURNING id`,
			"bench", "pgx-"+strconv.Itoa(i), id, (*int64)(nil), at, at).Scan(&entryID)
	}, func(i int) error {
		var entryID int64
		return store.G(ctx, pool).Raw(`
		INSERT INTO pending_settlements (plugin_key, ref_id, usage_log_id, account_id, next_check_at, deadline_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (plugin_key, ref_id) DO NOTHING
		RETURNING id`,
			"bench", "gorm-"+strconv.Itoa(i), id, (*int64)(nil), at, at).Row().Scan(&entryID)
	})
}

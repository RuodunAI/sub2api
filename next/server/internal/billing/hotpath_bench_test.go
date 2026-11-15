package billing

import (
	"context"
	"strconv"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/event"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The request-path database work of billing, measured against a real
// PostgreSQL (TEST_DATABASE_URL). These are the baseline the GORM migration
// is compared with (internal/store/gormtest/BASELINE.md): run them before
// and after with
//
//	go test -run '^$' -bench Hot -benchmem -count 5 ./internal/billing/
//
// Redis is left out on purpose: the measurement is PostgreSQL's alone.

// benchBilling is a billing service on a fresh database with one user whose
// balance no benchmark can exhaust.
func benchBilling(b *testing.B) (*Service, int64) {
	b.Helper()
	db := testutil.DB(b)
	svc := New(db, nil, nil, event.NewPublisher(db), nil)
	b.Cleanup(svc.Close)
	uid := testutil.InsertUser(b, db, "bench@test.invalid")
	testutil.FundUser(b, svc, uid)
	return svc, uid
}

// Precharge runs for every billable request before it is forwarded:
// BEGIN, prechargeSQL (lock timeout, the balance row's lock, the
// reservation and its ledger row), the balance.changed event, COMMIT.
func BenchmarkHotPrecharge(b *testing.B) {
	svc, uid := benchBilling(b)
	ctx := context.Background()
	price := &core.PriceRule{Expression: "flat(0.5)"}
	one := decimal.NewFromInt(1)
	// The first call prepares the statements; a busy node is past that.
	if err := svc.Precharge(ctx, &core.UsageRecord{RequestID: "warmup", UserID: uid, Price: price, RateMultiplier: one}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		rec := &core.UsageRecord{RequestID: "bench-" + strconv.Itoa(i), UserID: uid, Price: price, RateMultiplier: one}
		if err := svc.Precharge(ctx, rec); err != nil {
			b.Fatal(err)
		}
	}
}

// ReleasePrechargeTx runs for every free request the settler records
// (usage insertAtomic), most of which hold no reservation, and for every
// reservation the expiry sweeper returns. Its statement (releaseSQL) runs
// on GORM (releaseStmt); it is measured next to the pgx form it had before,
// for a request without a reservation (miss) and with one (hit).
func BenchmarkHotReleasePrecharge(b *testing.B) {
	svc, uid := benchBilling(b)
	ctx := context.Background()
	pool := svc.db.Pool
	paths := []struct {
		name string
		run  func(requestID string) error
	}{
		{"pgx", func(requestID string) error {
			var got core.LedgerResult
			var released bool
			var amount decimal.Decimal
			return pool.QueryRow(ctx, releaseSQL, requestID, uid, releaseKey(requestID), "release request precharge").
				Scan(&got.LedgerID, &got.BalanceAfter, &released, &amount)
		}},
		{"gorm", func(requestID string) error {
			var got core.LedgerResult
			var released bool
			var amount decimal.Decimal
			return releaseStmt.row(store.G(ctx, pool), requestID, uid, releaseKey(requestID), "release request precharge").
				Scan(&got.LedgerID, &got.BalanceAfter, &released, &amount)
		}},
	}
	reserve := func(b *testing.B, requestID string) {
		b.StopTimer()
		if _, err := pool.Exec(ctx, `INSERT INTO request_precharges (request_id, user_id, amount) VALUES ($1, $2, 0.5)`, requestID, uid); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	for _, p := range paths {
		b.Run("miss/"+p.name, func(b *testing.B) {
			if err := p.run("none"); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := p.run("none"); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("hit/"+p.name, func(b *testing.B) {
			reserve(b, p.name+"-warmup")
			if err := p.run(p.name + "-warmup"); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				id := p.name + "-" + strconv.Itoa(i)
				reserve(b, id)
				if err := p.run(id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

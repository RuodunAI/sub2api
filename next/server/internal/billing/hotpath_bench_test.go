package billing

import (
	"context"
	"strconv"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/event"
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
// BEGIN, one batch (lock_timeout, ensure the balance row, lock it, the
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

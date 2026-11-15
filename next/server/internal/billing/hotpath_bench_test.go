package billing

import (
	"context"
	"strconv"
	"strings"
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

// ReleasePrechargeTx runs for every free request the settler records
// (usage insertAtomic), most of which hold no reservation, and for every
// reservation the expiry sweeper returns. Its statement (releaseSQL) stays
// on pgx; it is measured next to the GORM form of the same statement, for a
// request without a reservation (miss) and with one (hit).
func BenchmarkHotReleasePrecharge(b *testing.B) {
	svc, uid := benchBilling(b)
	ctx := context.Background()
	pool := svc.db.Pool
	gormSQL, order := positional(releaseSQL)
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
			params := [...]any{requestID, uid, releaseKey(requestID), "release request precharge"}
			args := make([]any, len(order))
			for i, n := range order {
				args[i] = params[n]
			}
			return store.G(ctx, pool).Raw(gormSQL, args...).Row().
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

// positional rewrites the $n placeholders of a pgx statement as GORM's ?
// and returns, for each ?, the index of the pgx argument it takes.
func positional(sql string) (string, []int) {
	var b strings.Builder
	var order []int
	for i := 0; i < len(sql); i++ {
		j := i + 1
		for sql[i] == '$' && j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
			j++
		}
		if j == i+1 {
			b.WriteByte(sql[i])
			continue
		}
		n, _ := strconv.Atoi(sql[i+1 : j])
		order = append(order, n-1)
		b.WriteByte('?')
		i = j - 1
	}
	return b.String(), order
}

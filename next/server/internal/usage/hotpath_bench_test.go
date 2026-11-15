package usage

import (
	"context"
	"strconv"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/billing"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/billing/expr"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/event"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The per-request database work of the settler, measured against a real
// PostgreSQL (TEST_DATABASE_URL). These are the baseline the GORM migration
// is compared with (internal/store/gormtest/BASELINE.md): run them before
// and after with
//
//	go test -run '^$' -bench Hot -benchmem -count 5 ./internal/usage/
//
// The settler's workers are not started: each benchmark calls the step it
// measures for one record, which is the shape of a quiet node's batches.

type benchSettler struct {
	svc   *Service
	bill  *billing.Service
	user  int64
	group int64
	price *core.PriceRule
}

func newBenchSettler(b *testing.B) *benchSettler {
	b.Helper()
	ctx := context.Background()
	db := testutil.DB(b)
	pub := event.NewPublisher(db)
	bill := billing.New(db, nil, nil, pub, nil)
	b.Cleanup(bill.Close)
	s := &benchSettler{svc: New(db, bill, bill, pub, Options{}), bill: bill,
		user: testutil.InsertUser(b, db, "bench@test.invalid"), group: testutil.InsertGroup(b, db, "bench")}
	testutil.FundUser(b, bill, s.user)
	prog, err := expr.Compile(a6Expr)
	if err != nil {
		b.Fatal(err)
	}
	s.price = &core.PriceRule{Model: "claude-sonnet-4-5", Mode: "expression", Expression: a6Expr, ExprVersion: 1, ExprHash: prog.Hash()}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO model_prices (model, mode, expression, expr_hash, source)
		VALUES ('claude-sonnet-4-5', 'expression', $1, $2, 'manual') RETURNING id`, a6Expr, prog.Hash()).Scan(&s.price.ID); err != nil {
		b.Fatal(err)
	}
	return s
}

// record is a finished request as the gateway submits it.
func (s *benchSettler) record(i int, billable bool) *core.UsageRecord {
	rec := &core.UsageRecord{
		RequestID: "bench-" + strconv.Itoa(i), UserID: s.user, APIKeyID: 1, GroupID: s.group, PluginKey: "anthropic",
		Platform: "anthropic", Protocol: "anthropic.messages", AccountType: "apikey", UpstreamProtocol: "anthropic.messages",
		Endpoint: "/v1/messages", Model: "claude-sonnet-4-5", StatusCode: 200, Success: true, Attempts: 1,
		UsageSemantics: "exclusive", Tokens: core.UsageTokens{Input: 1200, Output: 800, CacheRead: 4000},
		Billable: billable, RateMultiplier: decimal.NewFromInt(1),
		HookDecisions: []core.HookDecision{{PluginKey: "guard", HookID: "h", Decision: "allow"}},
	}
	if billable {
		rec.Price = s.price
	}
	return rec
}

func (s *benchSettler) insert(b *testing.B, rec *core.UsageRecord) {
	b.Helper()
	if _, err := s.svc.insert(context.Background(), []*core.UsageRecord{rec}); err != nil {
		b.Fatal(err)
	}
}

// A free request's row: one transaction with the usage_logs insert (ON
// CONFLICT (request_id) DO NOTHING RETURNING id), the release of a
// precharge it might hold (releaseSQL) and the usage.recorded event.
func BenchmarkHotSettlerInsertFree(b *testing.B) {
	s := newBenchSettler(b)
	s.insert(b, s.record(-1, false)) // prepares the statements
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		s.insert(b, s.record(i, false))
	}
}

// A billable request's row, written pending: one transaction with the
// usage_logs insert alone.
func BenchmarkHotSettlerInsertPending(b *testing.B) {
	s := newBenchSettler(b)
	s.insert(b, s.record(-1, true))
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		s.insert(b, s.record(i, true))
	}
}

// Settlement of a pending row whose request was precharged: one
// transaction locking the row (SELECT ... FOR UPDATE), the ledger batch
// (ensure the balance row, release the precharge, apply the charge), the
// usage_logs update and the usage.recorded event. The precharge and the
// insert before it are not timed.
func BenchmarkHotSettle(b *testing.B) {
	s := newBenchSettler(b)
	ctx := context.Background()
	prepare := func(i int) *pending {
		rec := s.record(i, true)
		if err := s.bill.Precharge(ctx, rec); err != nil {
			b.Fatal(err)
		}
		s.insert(b, rec)
		return fromRecord(rec)
	}
	if err := s.svc.settle(ctx, prepare(-1), false); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		p := prepare(i)
		b.StartTimer()
		if err := s.svc.settle(ctx, p, false); err != nil {
			b.Fatal(err)
		}
	}
}

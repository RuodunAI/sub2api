package usage

import (
	"context"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/billing/expr"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/shopspring/decimal"
	"testing"
	"time"
)

// The primary usage is empty (it costs 0 at its price): the row is billed
// for its frozen extra alone.
func TestAdditionalSettlementDBRetriesFrozenExtra(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.record("additional-db", true)
	r.Tokens = core.UsageTokens{}
	r.CreatedAt = time.Now().Add(-time.Hour)
	r.Additional = []core.PricedUsage{{Kind: "advisor", Model: "other-model", Price: &core.PriceRule{Expression: `tier("extra",p)`, Mode: "expression"}, UsageSemantics: "exclusive", Tokens: core.UsageTokens{Input: 1_000_000}, RateMultiplier: decimal.NewFromInt(1)}}
	if err := f.bill.Precharge(ctx, r); err != nil {
		t.Fatal(err)
	}
	f.ledger.fail.Store(1)
	f.svc.process(ctx, []*core.UsageRecord{r})
	if f.ledger.fail.Load() != 0 {
		t.Fatal("settlement did not reach the failing ledger")
	}
	if got := f.scalar(`SELECT billing_status FROM usage_logs WHERE request_id='additional-db'`); got != StatusFailed {
		t.Fatal(got)
	}
	// Mutating the caller's object cannot alter the persisted retry snapshot.
	r.Additional[0].Price.Expression = `tier("changed",p*99)`
	if _, err := f.svc.RetryPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(`SELECT billing_status || ' ' || total_cost::text FROM usage_logs WHERE request_id='additional-db'`); got != "billed 1.00000000" {
		t.Fatal(got)
	}
	if !f.balance().Equal(decimal.NewFromInt(9)) {
		t.Fatal("precharge/settlement mismatch", f.balance())
	}
	if got := f.scalar(`SELECT billing_detail->'additional'->0->>'model' FROM usage_logs WHERE request_id='additional-db'`); got != "other-model" {
		t.Fatal("detail lost", got)
	}
	if _, err := f.svc.RetryPending(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.balance().Equal(decimal.NewFromInt(9)) {
		t.Fatal("double charged retry")
	}
}

// A price of 0 the administrator entered is a price: the request settles as
// billed at 0 and the balance does not move.
func TestSettleZeroPrice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const zeroExpr = `tier("base", p*0 + c*0)`
	hash := expr.Hash(zeroExpr)
	if _, err := f.db.SQL.ExecContext(ctx, `INSERT INTO model_price_history (expr_hash, expression, expr_version) VALUES ($1, $2, 1)`, hash, zeroExpr); err != nil {
		t.Fatal(err)
	}
	r := f.record("zero-price", true)
	r.Price = &core.PriceRule{ID: f.price.ID, Model: f.price.Model, Mode: "per_token", Expression: zeroExpr, ExprVersion: 1, ExprHash: hash}
	before := f.balance()
	if err := f.bill.Precharge(ctx, r); err != nil {
		t.Fatal(err)
	}
	f.svc.process(ctx, []*core.UsageRecord{r})
	if got := f.scalar(`SELECT billing_status || ' ' || total_cost::text FROM usage_logs WHERE request_id='zero-price'`); got != "billed 0.00000000" {
		t.Fatal(got)
	}
	if !f.balance().Equal(before) {
		t.Fatal("a zero price moved the balance", before, f.balance())
	}
}

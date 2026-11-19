package usage

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/billing/expr"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// exprPrice is an expression price of model as the price table freezes it.
func exprPrice(t *testing.T, id int64, model, src string) *core.PriceRule {
	t.Helper()
	prog, err := expr.Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	return &core.PriceRule{ID: id, Model: model, Mode: "expression", Expression: src, ExprVersion: 1, ExprHash: prog.Hash()}
}

// A Responses request that used the image_generation tool (CONTRACTS §120):
// the conversation (2304 input, 52 output tokens) at the conversation
// model's price, the image tool (39 input, 515 output tokens, all image) as
// additional usage at the image model's price; the record's cost is the sum
// and its billing detail shows each part. A price without img_o bills the
// image output at the text output price c. A record whose image model had no
// price under missing_price_policy "reject" is not settled: it fails, saying
// why. Under "free" the image usage is unpriced and the conversation alone
// is charged.
func TestImageToolSettledApart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	chat := exprPrice(t, 0, "gpt-5.6", `p*1.25 + c*10`)
	image := func(price *core.PriceRule) []core.PricedUsage {
		return []core.PricedUsage{{Kind: "image_generation", Model: "gpt-image-2-codex", Price: price, UsageSemantics: "inclusive",
			Tokens: core.UsageTokens{Input: 39, Output: 515, ImageOutput: 515}, RateMultiplier: f.record("x", true).RateMultiplier}}
	}
	for _, c := range []struct {
		id, status, total, part string
		additional              []core.PricedUsage
		billingError            string
	}{
		// 2304*1.25 + 52*10 = 3400; 39*5 + 515*30 = 15645 micro-USD.
		{"img-o", "billed", "0.01904500", "0.015645", image(exprPrice(t, 0, "gpt-image-2-codex", `p*5 + cr*1.25 + img*8 + img_o*30 + c*30`)), ""},
		// 39*5 + 515*10 = 5345.
		{"text-only", "billed", "0.00874500", "0.005345", image(exprPrice(t, 0, "gpt-image-2-codex", `p*5 + c*10`)), ""},
		{"free-policy", "billed", "0.00340000", "", image(nil), ""},
		{"rejected", "failed", "0.00000000", "", nil, "additional usage image_generation/gpt-image-2-codex: model_price_not_configured"},
	} {
		rec := f.record("req-"+c.id, true)
		rec.Platform, rec.Protocol, rec.UpstreamProtocol, rec.Endpoint = "openai", "openai.responses", "openai.responses", "/v1/responses"
		rec.Model, rec.UsageSemantics, rec.PriceHeaders, rec.Price = "gpt-5.6", "inclusive", nil, chat
		rec.Tokens = core.UsageTokens{Input: 2304, Output: 52}
		rec.Additional, rec.BillingError = c.additional, c.billingError
		f.svc.process(ctx, []*core.UsageRecord{rec})
		where := `FROM usage_logs WHERE request_id = 'req-` + c.id + `'`
		if s := f.scalar(`SELECT billing_status || ' ' || total_cost::text ` + where); s != c.status+" "+c.total {
			t.Fatalf("%s: row %s", c.id, s)
		}
		if c.status != "billed" {
			if s := f.scalar(`SELECT billing_detail->>'error' ` + where); s != "usage settlement blocked: "+c.billingError {
				t.Fatalf("%s: error %s", c.id, s)
			}
			continue
		}
		if s := f.scalar(`SELECT (billing_detail->'additional'->0->>'kind') || ' ' || (billing_detail->'additional'->0->>'model') || ' ' ||
			COALESCE(billing_detail->'additional'->0->'detail'->>'total_cost', '-') || ' ' ||
			(billing_detail->'inputs'->'additional'->0->'Tokens'->>'ImageOutput') ` + where); s != "image_generation gpt-image-2-codex "+map[bool]string{true: c.part, false: "0"}[c.part != ""]+" 515" {
			t.Fatalf("%s: detail %s", c.id, s)
		}
	}
}

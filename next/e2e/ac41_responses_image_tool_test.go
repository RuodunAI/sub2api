package e2e

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// imageToolPrice prices the image model of the Responses image_generation
// tool (USD per million tokens).
const imageToolPrice = `p*5 + cr*1.25 + img*8 + img_o*30 + c*30`

// The mock's tool_usage.image_gen (deploy/mock-upstream sites.go): input 39
// (no image), output 515 (all image). At imageToolPrice, inclusive
// semantics (billing/expr Normalize): img = 0, img_o = 515, p = 39 - 0 = 39,
// c = 515 - 515 = 0:
//
//	39*5 + 0*8 + 515*30 + 0*30 = 195 + 15450 = 15645 -> 0.015645 USD
//
// The conversation itself is the mock's Responses usage at RunPrice
// (input 120 of which 50 cached, output 42), as every OpenAI request:
//
//	(120-50)*3 + 42*15 + 50*0.3 = 210 + 630 + 15 = 855 -> 0.000855 USD
//
// Both parts are at the group rate imageRate: the record costs
// (0.000855 + 0.015645) * 1.5 = 0.02475 USD.
var imageToolCost = big.NewRat(15645, 1_000_000)

// The mock's marker for a tools echo without a model.
const imageToolNoEchoModel = "MOCK-IMAGE-ECHO-NO-MODEL"

// AC 41: a Responses request using the image_generation tool (CONTRACTS
// §118.10, §120) bills the image part apart, at the image model's price:
// the usage record keeps the conversation's tokens and one additional item
// of kind image_generation for the model the answer's tools echo names
// (else the request's tool model, else gpt-image-2), with its price and
// the tool's 515 image output tokens; the record's cost is the sum of both
// parts at the group rate. An image model without a price leaves the
// image part unbilled and the conversation billed: under
// missing_price_policy reject (the default) the record carries the
// additional_unpriced anomaly (administrators only), under free it does
// not. Over an OpenAI API key (JSON, stream, a
// WebSocket turn) and a codex account (a client JSON request the gateway
// streams and gathers back, the image in it). A request without the tool
// has no additional item.
func TestAC41_ResponsesImageTool(t *testing.T) {
	e := Setup(t)
	admin := e.Admin()
	reqModel := fmt.Sprintf("gpt-image-e2e-%s-tool", e.RunID)
	for _, model := range []string{"gpt-image-2-codex", "gpt-image-2", reqModel} {
		e.ensureExpressionPrice(admin, model, imageToolPrice)
	}
	dialog := ExpectedTokenCost(RunPrice, MockInputTokens-MockCacheReadTokens, MockOutputTokens, MockCacheReadTokens, 0, 0, 1)
	total := func(image bool) *big.Rat {
		sum := new(big.Rat).Set(dialog)
		if image {
			sum.Add(sum, imageToolCost)
		}
		return sum.Mul(sum, new(big.Rat).SetFloat64(imageRate))
	}
	tool := func(model string) map[string]any {
		t := map[string]any{"type": "image_generation"}
		if model != "" {
			t["model"] = model
		}
		return t
	}

	openai, _ := e.imageTenant(admin, "tool-openai", AccountSpec{PluginKey: OpenAIPlugin, Type: APIKeyType})
	openai.Model = e.RunModelOf(admin, "gpt", "imgtool")
	codex, _ := e.imageTenant(admin, "tool-codex", AccountSpec{PluginKey: "codex", Type: "codex_oauth"})
	codex.Model = e.RunModelOf(admin, "gpt", "imgtool")

	for _, c := range []struct {
		name      string
		tn        *Tenant
		stream    bool
		input     string
		tool      map[string]any // nil = no tool
		wantModel string         // "" = no additional item
	}{
		{name: "openai json, echoed model", tn: openai, input: "draw a cat", tool: tool(""), wantModel: "gpt-image-2-codex"},
		{name: "openai stream, echoed model", tn: openai, stream: true, input: "draw a cat", tool: tool(""), wantModel: "gpt-image-2-codex"},
		{name: "openai json, the request's model", tn: openai, input: "draw " + imageToolNoEchoModel, tool: tool(reqModel), wantModel: reqModel},
		{name: "openai json, the default model", tn: openai, input: "draw " + imageToolNoEchoModel, tool: tool(""), wantModel: "gpt-image-2"},
		{name: "codex json gathered from the stream", tn: codex, input: "draw a cat", tool: tool(""), wantModel: "gpt-image-2-codex"},
		{name: "openai json without the tool", tn: openai, input: "no tool"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.With(t)
			body := map[string]any{"model": c.tn.Model, "input": c.input + " " + e.Name("t"), "stream": c.stream}
			if c.tool != nil {
				body["tools"] = []any{c.tool}
			}
			g := e.OpenAI(c.tn.APIKey, "/v1/responses", body)
			if g.Status != 200 {
				t.Fatalf("HTTP %d %s", g.Status, g.Body)
			}
			if c.tool != nil && !c.stream && !g.JSON().Get(`output.#(type=="image_generation_call").result`).Exists() {
				t.Fatalf("no image in the answer: %s", g.Body)
			}
			u := e.UsageByRequest(admin, c.tn.User.UserID, g.RequestID)
			expectImageToolUsage(t, u, c.tn.Accounts[0].ID, c.wantModel, total(c.wantModel != ""))
		})
	}

	// An unpriced image model, named by the request's tool (the echo
	// without a model): the setting is saved on node 1 and the request sent
	// there (the other node learns of a save from a broadcast).
	for _, policy := range []string{"reject", "free"} {
		t.Run("unpriced image model, "+policy, func(t *testing.T) {
			e := e.With(t)
			node := *admin.Client
			node.Base = e.NodeURLs[0]
			prev := node.OK(t, http.MethodGet, "/settings/billing", nil).Get("missing_price_policy").String()
			t.Cleanup(func() { node.OK(t, http.MethodPut, "/settings/billing", map[string]any{"missing_price_policy": prev}) })
			node.OK(t, http.MethodPut, "/settings/billing", map[string]any{"missing_price_policy": policy})
			unpriced := fmt.Sprintf("gpt-image-e2e-%s-unpriced-%s", e.RunID, policy)
			g := e.Gateway(t, node.Base, "/v1/responses", "", map[string]any{"model": openai.Model, "input": "draw " + imageToolNoEchoModel,
				"tools": []any{tool(unpriced)}}, map[string]string{"Authorization": "Bearer " + openai.APIKey})
			if g.Status != 200 {
				t.Fatalf("HTTP %d %s", g.Status, g.Body)
			}
			u := e.UsageByRequest(admin, openai.User.UserID, g.RequestID)
			add := u.Get("billing_detail.additional").Array()
			if u.Get("billing_status").String() != "billed" || len(add) != 1 || add[0].Get("model").String() != unpriced || add[0].Get("price_id").Int() != 0 {
				t.Fatalf("unpriced image part: %s", u.Raw)
			}
			AssertMoney(t, "total_cost", u.Get("total_cost").String(), total(false))
			wantAnomaly := map[string]string{"reject": "image_generation/" + unpriced, "free": ""}[policy]
			if got := u.Get("anomalies.additional_unpriced").String(); got != wantAnomaly {
				t.Fatalf("anomalies.additional_unpriced = %q, want %q: %s", got, wantAnomaly, u.Get("anomalies").Raw)
			}
			own := openai.User.OK(t, http.MethodGet, fmt.Sprintf("/me/usage/%d", u.Get("id").Int()), nil)
			if own.Get("anomalies").Exists() {
				t.Fatalf("/me/usage shows anomalies: %s", own.Raw)
			}
		})
	}

	t.Run("openai websocket turn", func(t *testing.T) {
		e := e.With(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		ws, _ := e.imageTenant(admin, "tool-ws", AccountSpec{PluginKey: OpenAIPlugin, Type: APIKeyType})
		ws.Model = e.RunModelOf(admin, "gpt", "imgtool")
		conn := gatewayWS(ctx, t, e.BaseURL, ws.APIKey)
		defer conn.CloseNow()
		events := wsTurn(ctx, t, conn, map[string]any{"type": "response.create", "model": ws.Model, "input": "draw " + e.RunID,
			"tools": []any{tool("")}})
		if last := events[len(events)-1]; last.Get("type").String() != "response.completed" || last.Get("response.tool_usage.image_gen.output_tokens").Int() != 515 {
			t.Fatalf("websocket turn: %v", events)
		}
		conn.Close(websocket.StatusNormalClosure, "")
		var rec gjson.Result
		Eventually(t, 30*time.Second, time.Second, "the websocket turn's usage record", func() bool {
			for _, r := range admin.ListAll(t, "/usage", "user_id", fmt.Sprint(ws.User.UserID)) {
				if r.Get("billing_status").String() == "billed" {
					rec = admin.OK(t, http.MethodGet, fmt.Sprintf("/usage/%d", r.Get("id").Int()), nil)
					return true
				}
			}
			return false
		})
		expectImageToolUsage(t, rec, ws.Accounts[0].ID, "gpt-image-2-codex", total(true))
	})
}

// expectImageToolUsage checks a billed Responses record: the conversation's
// tokens, one additional image_generation item for model with its price
// and 515 image output tokens (none when model is ""), and the cost want.
func expectImageToolUsage(t testing.TB, u gjson.Result, accountID int64, model string, want *big.Rat) {
	t.Helper()
	if u.Get("account_id").Int() != accountID || u.Get("billing_status").String() != "billed" ||
		u.Get("input_tokens").Int() != MockInputTokens || u.Get("output_tokens").Int() != MockOutputTokens || u.Get("cache_read_tokens").Int() != MockCacheReadTokens {
		t.Fatalf("usage: %s", u.Raw)
	}
	add := u.Get("billing_detail.additional").Array()
	switch {
	case model == "" && len(add) != 0:
		t.Fatalf("a request without the tool has additional usage: %s", u.Get("billing_detail").Raw)
	case model != "" && (len(add) != 1 || add[0].Get("kind").String() != "image_generation" || add[0].Get("model").String() != model ||
		add[0].Get("price_id").Int() == 0 || u.Get("billing_detail.inputs.additional.0.Tokens.ImageOutput").Int() != 515):
		t.Fatalf("additional usage: want one image_generation item for %s: %s", model, u.Get("billing_detail").Raw)
	}
	AssertMoney(t, "total_cost", u.Get("total_cost").String(), want)
}

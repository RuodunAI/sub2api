package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// A request is pre-charged and charged at the price of the model the client
// asked for (CONTRACTS §17): not at the account's mapping, not at the model
// the upstream echoes, though both have a price.
func TestBilledAtTheRequestedModel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e := newEnv(t)
		for id, model := range map[int64]string{30: "up-mapped", 31: upModel} {
			e.pricer.rules[model] = &core.PriceRule{ID: id, Model: model, Mode: "per_token", Expression: "p*7", ExprHash: model}
		}
		e.accounts.set(1, func(a *core.Account) { a.ModelMapping = map[string]string{testModel: "up-mapped"} })
		if r := e.messages(body(testModel, stream)); r.status != 200 {
			t.Fatalf("stream=%v: %d %s", stream, r.status, r.body)
		}
		if sent := gjson.GetBytes(e.up.last().body, "model").String(); sent != "up-mapped" {
			t.Fatalf("upstream model %q", sent)
		}
		rec := e.record()
		if len(e.balance.precharged) != 1 || e.balance.precharged[0] == nil || e.balance.precharged[0].ID != 9 {
			t.Fatalf("stream=%v: pre-charged at %+v, want the requested model's price", stream, e.balance.precharged)
		}
		if !rec.Billable || rec.Price == nil || rec.Price.ID != 9 || rec.Model != testModel {
			t.Fatalf("stream=%v: charged at %+v (billable %v, model %q)", stream, rec.Price, rec.Billable, rec.Model)
		}
	}
}

// imageToolUpstream answers a Responses request the way the upstreams
// measured on 2026-11-18 do when the image_generation tool ran: the
// conversation's usage, and beside it the image tool's under
// tool_usage.image_gen, with tools echoing an image model.
func imageToolUpstream(t *testing.T) *httptest.Server {
	response := map[string]any{"id": "resp_1", "object": "response", "model": "gpt-up", "status": "completed", "output": []any{},
		"tools": []any{map[string]any{"type": "image_generation", "model": "gpt-image-2-codex"}},
		"usage": map[string]any{"input_tokens": 2304, "output_tokens": 52, "input_tokens_details": map[string]any{"cached_tokens": 0}},
		"tool_usage": map[string]any{"image_gen": map[string]any{
			"input_tokens": 39, "input_tokens_details": map[string]any{"text_tokens": 39, "image_tokens": 0},
			"output_tokens": 515, "output_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": 515}, "total_tokens": 554}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !gjson.GetBytes(raw, "stream").Bool() {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The image tool's tokens are the request's own usage, priced at the
// request's one price: input and output include them, the image tokens
// among them are img / img_o. The image model the response echoes has a
// price and is not used.
func TestImageGenerationToolIsTheRequestsUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		srv := imageToolUpstream(t)
		plat := &fakePlatform{route: func(*pluginv1.ExecuteRequest) string { return srv.URL + "/v1/responses" }}
		e := newEnv(t)
		e.addAccountType("oai", "apikey", plat, manifest.AccountPlatform{Platform: manifest.PlatformOpenAI})
		e.accounts.addTyped(testGroup, 11, 200, "oai-11", "oai", "apikey")
		e.pricer.rules["gpt-5.6"] = &core.PriceRule{ID: 20, Model: "gpt-5.6", Mode: "expression", Expression: "p*1.25 + img_o*30 + c*10", ExprHash: "chat"}
		e.pricer.rules["gpt-image-2-codex"] = &core.PriceRule{ID: 21, Model: "gpt-image-2-codex", Mode: "expression", Expression: "p*5", ExprHash: "img"}
		in := map[string]any{"model": "gpt-5.6", "stream": stream, "input": "draw a fox", "tools": []any{map[string]any{"type": "image_generation"}}}
		if r := e.post("/v1/responses", in, bearer()); r.status != 200 {
			t.Fatalf("stream=%v: %d %s", stream, r.status, r.body)
		}
		rec := e.record()
		if !rec.Success || !rec.Billable || rec.Price == nil || rec.Price.ID != 20 || len(rec.Additional) != 0 || rec.BillingError != "" {
			t.Fatalf("stream=%v: record %+v", stream, rec)
		}
		if want := (core.UsageTokens{Input: 2304 + 39, Output: 52 + 515, ImageOutput: 515}); rec.Tokens != want {
			t.Fatalf("stream=%v: tokens %+v, want %+v", stream, rec.Tokens, want)
		}
	}
}

// A WebSocket turn reads the image tool's tokens the same way.
func TestImageGenerationToolWebSocketTurn(t *testing.T) {
	e := newWSEnv(t)
	e.pricer.rules["gpt-image-2-codex"] = &core.PriceRule{ID: 21, Model: "gpt-image-2-codex", Mode: "per_token", Expression: "p*5"}
	c := e.mustDial()
	turn := create(wsModel, "image")
	turn["tools"] = []any{map[string]any{"type": "image_generation"}}
	c.send(turn)
	c.until("response.completed")
	r := e.record()
	if !r.Success || !r.Billable || r.Price == nil || r.Price.ID != 10 || len(r.Additional) != 0 ||
		r.Tokens != (core.UsageTokens{Input: wsInput + 39, Output: wsOutput + 515, CacheRead: wsCacheRead, ImageOutput: 515}) {
		t.Fatalf("image turn: %+v", r)
	}
}

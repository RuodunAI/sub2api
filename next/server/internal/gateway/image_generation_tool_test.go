package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// imageToolUpstream answers Responses requests carrying the image_generation
// tool the way the upstreams measured on 2026-11-18 do (CONTRACTS §120): the
// conversation usage, the image tool's usage under tool_usage.image_gen and
// the tools echo naming the image model. A stream ends in response.completed
// with "output": [] (the Codex backend; the image is only in
// output_item.done). echoModel "" leaves the model out of the echo; noTool
// answers as for an ordinary conversation (an all-zero tool usage).
type imageToolUpstream struct {
	srv       *httptest.Server
	echoModel string
	noTool    bool
	bodies    chan []byte
}

func newImageToolUpstream(t *testing.T) *imageToolUpstream {
	u := &imageToolUpstream{echoModel: "gpt-image-2-codex", bodies: make(chan []byte, 16)}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *imageToolUpstream) response(output []any) map[string]any {
	tool := map[string]any{"type": "image_generation", "quality": "auto", "size": "auto"}
	if u.echoModel != "" {
		tool["model"] = u.echoModel
	}
	in, out := 39, 515
	if u.noTool {
		in, out = 0, 0
	}
	return map[string]any{"id": "resp_1", "object": "response", "model": "gpt-up", "status": "completed", "output": output,
		"tools": []any{tool},
		"usage": map[string]any{"input_tokens": 2304, "output_tokens": 52, "input_tokens_details": map[string]any{"cached_tokens": 0}},
		"tool_usage": map[string]any{"image_gen": map[string]any{
			"input_tokens": in, "input_tokens_details": map[string]any{"text_tokens": in, "image_tokens": 0},
			"output_tokens": out, "output_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": out}, "total_tokens": in + out}}}
}

func (u *imageToolUpstream) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	u.bodies <- raw
	image := map[string]any{"id": "ig_1", "type": "image_generation_call", "status": "completed", "result": "aGk="}
	if !gjson.GetBytes(raw, "stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(u.response([]any{image}))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], b)
		w.(http.Flusher).Flush()
	}
	created := u.response([]any{})
	created["tool_usage"] = map[string]any{"image_gen": map[string]any{"input_tokens": 0, "output_tokens": 0}}
	send(map[string]any{"type": "response.created", "response": created})
	send(map[string]any{"type": "response.image_generation_call.completed", "output_index": 0, "item_id": "ig_1"})
	send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": image})
	send(map[string]any{"type": "response.completed", "response": u.response([]any{})})
}

// imageToolEnv serves the built-in openai responses endpoint with an API key
// like type ("oai/apikey", account 11) and a stream-only Codex like type
// ("cdx/chatgpt", account 12), each in its own group order: priority picks
// the API key account unless only the Codex like type may serve.
func imageToolEnv(t *testing.T, codexOnly bool) (*env, *imageToolUpstream) {
	up := newImageToolUpstream(t)
	plat := &fakePlatform{route: func(*pluginv1.ExecuteRequest) string { return up.srv.URL + "/v1/responses" }}
	e := newEnv(t)
	if codexOnly {
		e.addAccountType("cdx", "chatgpt", plat, manifest.AccountPlatform{Platform: manifest.PlatformOpenAI,
			Endpoints: []string{"responses", "responses_websocket"}, StreamOnly: []string{"openai.responses"}})
		e.accounts.addTyped(testGroup, 12, 200, "cdx-12", "cdx", "chatgpt")
	} else {
		e.addAccountType("oai", "apikey", plat, manifest.AccountPlatform{Platform: manifest.PlatformOpenAI})
		e.accounts.addTyped(testGroup, 11, 200, "oai-11", "oai", "apikey")
	}
	e.pricer.rules["gpt-5.6"] = &core.PriceRule{ID: 20, Model: "gpt-5.6", Mode: "expression", Expression: "p*1.25 + c*10", ExprHash: "chat"}
	e.pricer.rules["gpt-image-2-codex"] = &core.PriceRule{ID: 21, Model: "gpt-image-2-codex", Mode: "expression",
		Expression: "p*5 + cr*1.25 + img*8 + img_o*30 + c*30", ExprHash: "img"}
	return e, up
}

func imageToolBody(stream bool, tool map[string]any) map[string]any {
	return map[string]any{"model": "gpt-5.6", "stream": stream, "input": "draw a fox", "tools": []any{tool}}
}

// assertImageRecord checks a record whose conversation used 2304/52 tokens
// and whose image tool used 39 input and 515 image output tokens of model.
func assertImageRecord(t *testing.T, rec *core.UsageRecord, model string, priceID int64) {
	t.Helper()
	if !rec.Success || !rec.Billable || rec.BillingError != "" || rec.Price == nil || rec.Price.ID != 20 ||
		rec.Tokens != (core.UsageTokens{Input: 2304, Output: 52}) {
		t.Fatalf("record %+v", rec)
	}
	if len(rec.Additional) != 1 {
		t.Fatalf("additional %+v", rec.Additional)
	}
	a := rec.Additional[0]
	if a.Kind != "image_generation" || a.Model != model || a.UsageSemantics != "inclusive" ||
		a.Tokens != (core.UsageTokens{Input: 39, Output: 515, ImageOutput: 515}) || !a.RateMultiplier.Equal(rec.RateMultiplier) {
		t.Fatalf("image usage %+v", a)
	}
	if priceID == 0 && a.Price != nil || priceID != 0 && (a.Price == nil || a.Price.ID != priceID) {
		t.Fatalf("image price %+v, want id %d", a.Price, priceID)
	}
}

// The three HTTP forms: an API key account's JSON and stream, a Codex like
// account's stream, and its forced stream folded back into JSON for a client
// that did not ask for one.
func TestImageGenerationToolBilledApart(t *testing.T) {
	tool := map[string]any{"type": "image_generation", "output_format": "png"}
	for _, codex := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			e, up := imageToolEnv(t, codex)
			r := e.post("/v1/responses", imageToolBody(stream, tool), bearer())
			if r.status != 200 {
				t.Fatalf("codex=%v stream=%v: %d %s", codex, stream, r.status, r.body)
			}
			if sent := <-up.bodies; gjson.GetBytes(sent, `tools.0.type`).String() != "image_generation" ||
				gjson.GetBytes(sent, "stream").Bool() != (stream || codex) {
				t.Fatalf("upstream body %s", sent)
			}
			if !stream && gjson.GetBytes(r.body, `output.#(type=="image_generation_call").result`).String() != "aGk=" {
				t.Fatalf("codex=%v: the image is not in the JSON answer: %s", codex, r.body)
			}
			rec := e.record()
			if rec.Stream != stream {
				t.Fatalf("stream = %v", rec.Stream)
			}
			assertImageRecord(t, rec, "gpt-image-2-codex", 21)
		}
	}
}

// The image model: the echo, else the tool in the request, else the rule's
// default.
func TestImageGenerationToolModel(t *testing.T) {
	e, up := imageToolEnv(t, false)
	e.pricer.rules["gpt-image-1.5"] = &core.PriceRule{ID: 22, Model: "gpt-image-1.5", Mode: "expression", Expression: "p*5 + img_o*32 + c*10"}
	e.pricer.rules["gpt-image-2"] = &core.PriceRule{ID: 23, Model: "gpt-image-2", Mode: "expression", Expression: "p*5 + img_o*30 + c*30"}
	up.echoModel = ""
	e.post("/v1/responses", imageToolBody(true, map[string]any{"type": "image_generation", "model": "gpt-image-1.5"}), bearer())
	<-up.bodies
	assertImageRecord(t, e.record(), "gpt-image-1.5", 22)
	e.post("/v1/responses", imageToolBody(false, map[string]any{"type": "image_generation"}), bearer())
	<-up.bodies
	assertImageRecord(t, e.record(), "gpt-image-2", 23)
}

// Without a price for the image model, missing_price_policy decides as it
// does for any model, now that the model is known: "free" leaves the image
// usage unpriced (it shows in the record, nothing is charged for it);
// "reject" cannot refuse a request already answered, so the record's billing
// is blocked and says why - the image is never silently free.
func TestImageGenerationToolWithoutPrice(t *testing.T) {
	e, up := imageToolEnv(t, false)
	delete(e.pricer.rules, "gpt-image-2-codex")
	e.post("/v1/responses", imageToolBody(true, map[string]any{"type": "image_generation"}), bearer())
	<-up.bodies
	rec := e.record()
	if !rec.Billable || len(rec.Additional) != 0 || !strings.Contains(rec.BillingError, "image_generation/gpt-image-2-codex") ||
		!strings.Contains(rec.BillingError, core.ErrPriceNotConfigured.Code) {
		t.Fatalf("reject: record %+v", rec)
	}
	e.pricer.free = true
	e.post("/v1/responses", imageToolBody(false, map[string]any{"type": "image_generation"}), bearer())
	<-up.bodies
	rec = e.record()
	assertImageRecord(t, rec, "gpt-image-2-codex", 0)
}

// An ordinary conversation, whose answer carries an all-zero tool usage, has
// no additional usage and resolves no other price.
func TestImageGenerationToolUnused(t *testing.T) {
	e, up := imageToolEnv(t, true)
	up.noTool = true
	for _, stream := range []bool{false, true} {
		e.pricer.queries = nil
		e.post("/v1/responses", map[string]any{"model": "gpt-5.6", "stream": stream, "input": "hi"}, bearer())
		<-up.bodies
		rec := e.record()
		if !rec.Success || rec.Additional != nil || rec.BillingError != "" || rec.Tokens != (core.UsageTokens{Input: 2304, Output: 52}) {
			t.Fatalf("record %+v", rec)
		}
		for _, q := range e.pricer.queries {
			if q.Model != "gpt-5.6" {
				t.Fatalf("priced %s", q.Model)
			}
		}
	}
}

// A WebSocket turn is billed like an HTTP request: its image tool usage is
// additional usage of the image model, the turn before it has none.
func TestImageGenerationToolWebSocketTurn(t *testing.T) {
	e := newWSEnv(t)
	e.pricer.rules["gpt-image-2-codex"] = &core.PriceRule{ID: 21, Model: "gpt-image-2-codex", Mode: "expression", Expression: "p*5 + img_o*30 + c*30"}
	c := e.mustDial()
	c.send(create(wsModel, "hi"))
	c.until("response.completed")
	if r := e.record(); r.Additional != nil || r.BillingError != "" {
		t.Fatalf("plain turn: %+v", r)
	}
	turn := create(wsModel, "image")
	turn["tools"] = []any{map[string]any{"type": "image_generation"}}
	c.send(turn)
	c.until("response.completed")
	r := e.record()
	if !r.Success || !r.Billable || r.BillingError != "" || r.Tokens.Input != wsInput || len(r.Additional) != 1 {
		t.Fatalf("image turn: %+v", r)
	}
	if a := r.Additional[0]; a.Kind != "image_generation" || a.Model != "gpt-image-2-codex" || a.Price == nil || a.Price.ID != 21 ||
		a.Tokens != (core.UsageTokens{Input: 39, Output: 515, ImageOutput: 515}) {
		t.Fatalf("image usage %+v", a)
	}
}

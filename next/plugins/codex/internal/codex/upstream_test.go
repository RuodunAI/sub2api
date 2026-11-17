package codex

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

const (
	deviceCreds = `{"access_token":"at-device","refresh_token":"rt-device","chatgpt_account_id":"acct-1"}`
)

// accountTypes are the plugin's account types; they behave alike.
var accountTypes = []string{TypeDevice, TypeOAuth}

func patchMap(r *pluginv1.BuildUpstreamRequestResponse) (sets map[string]string, dels []string) {
	sets = map[string]string{}
	for _, p := range r.GetPatches() {
		switch p.GetOp() {
		case pluginv1.BodyPatch_OP_SET:
			sets[p.GetPath()] = p.GetValueJson()
		case pluginv1.BodyPatch_OP_DELETE:
			dels = append(dels, p.GetPath())
		}
	}
	sort.Strings(dels)
	return sets, dels
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	sort.Strings(s)
	return s
}

// The client's own headers never reach the upstream: the plugin names
// sup2api, truthfully, and nothing else.
var clientHeaders = map[string]string{"user-agent": "codex_cli_rs/0.99", "openai-beta": "responses=experimental", "x-stainless-lang": "js"}

func TestBuildUpstreamRequestDevice(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	r, err := h.Platform.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
		Meta:           &pluginv1.RequestMeta{Protocol: ProtocolResponses, ClientProtocol: "openai.chat", Model: "gpt-5.4"},
		Account:        account(TypeDevice, deviceCreds),
		Fields:         map[string]string{"model": `"gpt-5.4"`, FieldHasInstructions: "0", FieldStringInput: `"hello"`},
		InboundHeaders: clientHeaders,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"authorization":      "Bearer at-device",
		"chatgpt-account-id": "acct-1",
		"originator":         "sup2api",
		"user-agent":         "sup2api-codex/" + Version,
		"accept":             "text/event-stream",
		"content-type":       "application/json",
	}
	if r.GetMethod() != "POST" || r.GetUrl() != "https://chatgpt.com/backend-api/codex/responses" || r.GetUpstreamModel() != "gpt-5.4" {
		t.Fatalf("request = %s %s %s", r.GetMethod(), r.GetUrl(), r.GetUpstreamModel())
	}
	if len(r.GetHeaders()) != len(want) {
		t.Fatalf("headers = %v, want exactly %v", r.GetHeaders(), want)
	}
	for k, v := range want {
		if r.GetHeaders()[k] != v {
			t.Errorf("header %s = %q, want %q", k, r.GetHeaders()[k], v)
		}
	}
	sets, dels := patchMap(r)
	if sets["store"] != "false" || sets["instructions"] != `""` {
		t.Fatalf("sets = %v", sets)
	}
	if _, ok := sets["stream"]; ok {
		t.Fatal("stream is the core's to set (streamOnly)")
	}
	var input []map[string]any
	mustJSON(t, []byte(sets["input"]), &input)
	if len(input) != 1 || input[0]["role"] != "user" || input[0]["type"] != "message" {
		t.Fatalf("input = %s", sets["input"])
	}
	if c, _ := input[0]["content"].([]any); len(c) != 1 || c[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("input content = %s", sets["input"])
	}
	if !slices.Equal(dels, sorted(unsupportedFields)) {
		t.Fatalf("deletes = %v", dels)
	}

	// Instructions present, input already an array, no account id: no
	// instructions or input patch, no account header.
	r, err = h.Platform.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
		Meta:    &pluginv1.RequestMeta{Protocol: ProtocolResponses, Model: "gpt-5.1-codex", Stream: true},
		Account: account(TypeDevice, `{"access_token":"at"}`),
		Fields:  map[string]string{"model": `"gpt-5.1-codex"`, FieldHasInstructions: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sets, _ = patchMap(r)
	if _, ok := sets["instructions"]; ok {
		t.Errorf("instructions patched although present: %v", sets)
	}
	if _, ok := sets["input"]; ok {
		t.Errorf("array input patched: %v", sets)
	}
	if _, ok := r.GetHeaders()["chatgpt-account-id"]; ok {
		t.Errorf("empty account id sent: %v", r.GetHeaders())
	}
}

// A Sign in with Codex account holds the same tokens: its request is the
// device code account's.
func TestBuildUpstreamRequestOAuth(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	build := func(typ string) *pluginv1.BuildUpstreamRequestResponse {
		r, err := h.Platform.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
			Meta:    &pluginv1.RequestMeta{Protocol: ProtocolResponses, Model: "gpt-5.4"},
			Account: account(typ, deviceCreds),
			Fields:  map[string]string{"model": `"gpt-5.4"`, FieldHasInstructions: "0", FieldStringInput: `"  "`},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	device, oauth := build(TypeDevice), build(TypeOAuth)
	deviceSets, deviceDels := patchMap(device)
	sets, dels := patchMap(oauth)
	if oauth.GetMethod() != device.GetMethod() || oauth.GetUrl() != device.GetUrl() || !maps.Equal(oauth.GetHeaders(), device.GetHeaders()) ||
		!maps.Equal(sets, deviceSets) || !slices.Equal(dels, deviceDels) {
		t.Fatalf("codex_oauth = %v, want the device request %v", oauth, device)
	}
	if sets["input"] != "[]" || sets["instructions"] != `""` {
		t.Fatalf("sets = %v", sets)
	}
}

func TestBuildUpstreamRequestWebSocket(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	for _, typ := range accountTypes {
		r, err := h.Platform.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
			Meta:           &pluginv1.RequestMeta{Protocol: ProtocolResponsesWS, Model: "gpt-5.4"},
			Account:        account(typ, deviceCreds),
			Fields:         map[string]string{"model": `"gpt-5.4"`, FieldHasInstructions: "0", FieldStringInput: `"x"`},
			InboundHeaders: clientHeaders,
		})
		if err != nil {
			t.Fatal(err)
		}
		hd := r.GetHeaders()
		if r.GetMethod() != "GET" || r.GetUrl() != "wss://chatgpt.com/backend-api/codex/responses" || hd["openai-beta"] != WebSocketBeta ||
			hd["user-agent"] != UserAgent || hd["originator"] != Originator || hd["chatgpt-account-id"] != "acct-1" ||
			hd["content-type"] != "" || hd["accept"] != "" {
			t.Fatalf("%s: %s %s %v", typ, r.GetMethod(), r.GetUrl(), hd)
		}
		// Every turn's message gets the HTTP patches (CONTRACTS §83.6),
		// without stream fields.
		sets, dels := patchMap(r)
		if sets["store"] != "false" || len(sets["input"]) == 0 || sets["instructions"] != `""` {
			t.Fatalf("%s ws sets = %v", typ, sets)
		}
		for path := range sets {
			if strings.HasPrefix(path, "stream") {
				t.Fatalf("%s ws patches a stream field: %v", typ, sets)
			}
		}
		if !slices.Equal(dels, sorted(removedFields(true))) || slices.Contains(dels, wsKept) || len(dels) != len(unsupportedFields)-1 {
			t.Fatalf("%s ws deletes = %v", typ, dels)
		}
		// The same body over HTTP deletes the kept field too.
		r, err = h.Platform.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
			Meta: &pluginv1.RequestMeta{Protocol: ProtocolResponses, Model: "gpt-5.4"}, Account: account(typ, deviceCreds),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, httpDels := patchMap(r); !slices.Equal(httpDels, sorted(unsupportedFields)) {
			t.Fatalf("%s http deletes = %v", typ, httpDels)
		}
	}
	// A test server root keeps its scheme family.
	p := mustPlugin(testEndpoints("http://127.0.0.1:9"))
	r, err := p.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
		Meta: &pluginv1.RequestMeta{Protocol: ProtocolResponsesWS}, Account: account(TypeOAuth, deviceCreds),
	})
	if err != nil || r.GetUrl() != "ws://127.0.0.1:9/backend-api/codex/responses" {
		t.Fatalf("ws url = %v %v", r.GetUrl(), err)
	}
}

// The upstream session follows the conversation's prompt_cache_key, isolated
// per API key, over HTTP and WebSocket alike; without a usable key none is
// sent.
func TestBuildUpstreamRequestSession(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	build := func(proto string, apiKeyID int64, fields map[string]string) map[string]string {
		r, err := h.Platform.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
			Meta:           &pluginv1.RequestMeta{Protocol: proto, Model: "gpt-5.4", ApiKeyId: apiKeyID},
			Account:        account(TypeDevice, deviceCreds),
			Fields:         fields,
			InboundHeaders: map[string]string{"session_id": "client-session"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r.GetHeaders()
	}
	conv := map[string]string{FieldPromptCacheKey: `"conv-1"`}
	first := build(ProtocolResponses, 7, conv)
	id := first["session_id"]
	if len(id) != 36 || id[14] != '8' || first["conversation_id"] != id || strings.Contains(id, "conv-1") || id == "client-session" {
		t.Fatalf("session headers = %v", first)
	}
	if again := build(ProtocolResponses, 7, conv); again["session_id"] != id {
		t.Fatalf("same conversation, other session: %q vs %q", again["session_id"], id)
	}
	if ws := build(ProtocolResponsesWS, 7, conv); ws["session_id"] != id || ws["conversation_id"] != id {
		t.Fatalf("ws session headers = %v, want %q", ws, id)
	}
	if other := build(ProtocolResponses, 8, conv); other["session_id"] == id {
		t.Fatal("two API keys share an upstream session")
	}
	if next := build(ProtocolResponses, 7, map[string]string{FieldPromptCacheKey: `"conv-2"`}); next["session_id"] == id {
		t.Fatal("two conversations share an upstream session")
	}
	for _, fields := range []map[string]string{nil, {FieldPromptCacheKey: `""`}, {FieldPromptCacheKey: `123`}} {
		hd := build(ProtocolResponses, 7, fields)
		if _, ok := hd["session_id"]; ok {
			t.Errorf("fields %v: session sent: %v", fields, hd)
		}
		if _, ok := hd["conversation_id"]; ok {
			t.Errorf("fields %v: conversation sent: %v", fields, hd)
		}
	}
}

func TestBuildUpstreamRequestErrors(t *testing.T) {
	p := mustPlugin(DefaultEndpoints)
	for _, tc := range []struct {
		name string
		meta *pluginv1.RequestMeta
		acc  *pluginv1.Account
		code codes.Code
	}{
		{"chat protocol", &pluginv1.RequestMeta{Protocol: "openai.chat"}, account(TypeDevice, deviceCreds), codes.InvalidArgument},
		{"embeddings", &pluginv1.RequestMeta{Protocol: "openai.embeddings"}, account(TypeOAuth, deviceCreds), codes.InvalidArgument},
		{"foreign type", &pluginv1.RequestMeta{Protocol: ProtocolResponses}, account("apikey", deviceCreds), codes.FailedPrecondition},
		{"no token", &pluginv1.RequestMeta{Protocol: ProtocolResponses}, account(TypeDevice, `{"refresh_token":"x"}`), codes.FailedPrecondition},
		{"bad json", &pluginv1.RequestMeta{Protocol: ProtocolResponses}, account(TypeDevice, `{`), codes.InvalidArgument},
	} {
		_, err := p.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{Meta: tc.meta, Account: tc.acc})
		if status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.code)
		}
	}
}

func TestBuildTestRequest(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	for _, tc := range []struct {
		typ, model, wantModel string
	}{
		{TypeDevice, "", DefaultTestModel},
		{TypeOAuth, "gpt-5.4", "gpt-5.4"},
	} {
		r, err := h.Platform.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{Account: account(tc.typ, deviceCreds), Model: tc.model})
		if err != nil {
			t.Fatal(err)
		}
		if r.GetMethod() != "POST" || r.GetUrl() != "https://chatgpt.com/backend-api/codex/responses" || r.GetHeaders()["chatgpt-account-id"] != "acct-1" || r.GetModel() != tc.wantModel || r.GetUsageProtocol() != ProtocolResponses ||
			r.GetHeaders()["user-agent"] != UserAgent || r.GetHeaders()["accept"] != "text/event-stream" {
			t.Fatalf("%s: %v", tc.typ, r)
		}
		var body map[string]any
		mustJSON(t, []byte(r.GetBodyJson()), &body)
		if body["model"] != tc.wantModel || body["store"] != false || body["stream"] != true {
			t.Fatalf("%s body = %s", tc.typ, r.GetBodyJson())
		}
		if in, _ := body["input"].([]any); len(in) != 1 {
			t.Fatalf("%s input = %s", tc.typ, r.GetBodyJson())
		}
		if body["instructions"] != "" {
			t.Fatalf("%s instructions = %s", tc.typ, r.GetBodyJson())
		}
	}
}

// The Codex backend's model list is the Codex CLI's internal endpoint: the
// plugin lists none and the console keeps the manifest defaults.
func TestNoModelList(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	for _, typ := range accountTypes {
		if _, err := h.Platform.BuildModelsRequest(context.Background(), &pluginv1.BuildModelsRequestRequest{Account: account(typ, deviceCreds)}); status.Code(err) != codes.Unimplemented {
			t.Fatalf("%s models: %v", typ, err)
		}
	}
}

func TestExecuteForwardsOnce(t *testing.T) {
	var stage atomic.Int32
	fh := pluginsdktest.NewFakeHost()
	fh.ForwardUpstreamFunc = func(_ context.Context, in *pluginv1.ForwardUpstreamRequest) (*pluginv1.ForwardUpstreamResponse, error) {
		if !stage.CompareAndSwap(0, 1) {
			t.Error("forwarded more than once")
		}
		r := in.GetRequest()
		if r.GetUrl() != "https://chatgpt.com/backend-api/codex/responses" || r.GetHeaders()["authorization"] != "Bearer at-device" {
			t.Errorf("request = %v", r)
		}
		return &pluginv1.ForwardUpstreamResponse{}, nil
	}
	fh.RecordUsageFunc = func(_ context.Context, _ *pluginv1.RecordUsageRequest) (*pluginv1.ExecutionReceipt, error) {
		if !stage.CompareAndSwap(1, 2) {
			t.Error("usage recorded out of order")
		}
		return &pluginv1.ExecutionReceipt{}, nil
	}
	h := pluginsdktest.Start(t, mustPlugin(DefaultEndpoints), pluginsdktest.Options{Host: fh, SDK: []pluginsdk.Option{pluginsdk.WithInfo("codex", Version)}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := h.Platform.Execute(ctx, &pluginv1.ExecuteRequest{ExecutionToken: "req-1", Request: &pluginv1.BuildUpstreamRequestRequest{
		Meta: &pluginv1.RequestMeta{Protocol: ProtocolResponses, Model: "gpt-5.4", Stream: true}, Account: account(TypeOAuth, deviceCreds),
	}})
	if err != nil || r.GetClassification() != nil || stage.Load() != 2 {
		t.Fatalf("Execute: stage=%d %v %v", stage.Load(), r, err)
	}
}

func TestValidateCredentials(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	validate := func(typ, creds string) *pluginv1.ValidateCredentialsResponse {
		t.Helper()
		r, err := h.Platform.ValidateCredentials(context.Background(), &pluginv1.ValidateCredentialsRequest{AccountType: typ, CredentialsJson: creds})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Identity fields come from the id_token; expires_at becomes a number.
	tok := idToken("me@example.com", "acct-7", "plus")
	for _, typ := range accountTypes {
		r := validate(typ, `{"access_token":" at ","id_token":"`+tok+`","expires_at":"1800000123"}`)
		if len(r.GetErrors()) != 0 {
			t.Fatalf("%s errors = %v", typ, r.GetErrors())
		}
		var norm map[string]any
		mustJSON(t, []byte(r.GetNormalizedCredentialsJson()), &norm)
		if norm["access_token"] != "at" || norm["email"] != "me@example.com" || norm["chatgpt_account_id"] != "acct-7" ||
			norm["plan_type"] != "plus" || norm["expires_at"] != float64(1800000123) {
			t.Fatalf("%s normalized = %v", typ, norm)
		}
	}
	// Already normal: nothing to rewrite. Present fields are not overwritten.
	if r := validate(TypeOAuth, deviceCreds); len(r.GetErrors()) != 0 || r.GetNormalizedCredentialsJson() != "" {
		t.Fatalf("normal = %v", r)
	}
	r := validate(TypeOAuth, `{"access_token":"at","email":"kept@example.com","chatgpt_account_id":"kept","id_token":"`+tok+`"}`)
	var norm map[string]any
	mustJSON(t, []byte(r.GetNormalizedCredentialsJson()), &norm)
	if norm["email"] != "kept@example.com" || norm["chatgpt_account_id"] != "kept" || norm["plan_type"] != "plus" {
		t.Fatalf("kept = %v", norm)
	}
	for _, tc := range []struct {
		typ, creds, field, code string
	}{
		{"apikey", `{"access_token":"x"}`, "account_type", "unsupported"},
		{TypeDevice, `{}`, "access_token", "required"},
		{TypeDevice, `{"access_token":"  "}`, "access_token", "required"},
		{TypeDevice, `{"access_token":5}`, "access_token", "pattern"},
		{TypeDevice, `[1]`, "access_token", "invalid"},
		{TypeDevice, `{"access_token":"x","expires_at":"soon"}`, "expires_at", "pattern"},
		{TypeDevice, `{"access_token":"x","refresh_token":1}`, "refresh_token", "pattern"},
		{"chatgpt_siwc", `{"access_token":"x"}`, "account_type", "unsupported"},
		{TypeOAuth, `{"access_token":"x","email":1}`, "email", "pattern"},
	} {
		r := validate(tc.typ, tc.creds)
		if len(r.GetErrors()) != 1 || r.GetErrors()[0].GetField() != tc.field || r.GetErrors()[0].GetCode() != tc.code {
			t.Errorf("%s %s: errors = %v, want %s/%s", tc.typ, tc.creds, r.GetErrors(), tc.field, tc.code)
		}
	}
}

func TestIdentity(t *testing.T) {
	// Account id and plan fall back to the access token's auth claim.
	at := fakeJWT(map[string]any{"exp": 1800003600, authClaim: map[string]any{"chatgpt_account_id": "acct-at", "chatgpt_plan_type": "pro"}})
	id := parseIdentity(fakeJWT(map[string]any{"email": "a@b.c"}), at)
	if id.Email != "a@b.c" || id.AccountID != "acct-at" || id.PlanType != "pro" {
		t.Fatalf("identity = %+v", id)
	}
	if tokenExpiry(at) != 1800003600 || tokenExpiry("opaque") != 0 {
		t.Fatal("token expiry")
	}
	if parseIdentity("not.a.jwt!", "x") != (identity{}) {
		t.Fatal("garbage token must yield nothing")
	}
}

// System input items become developer items for both account types, over
// HTTP and in WebSocket messages; the patch paths come from the roles field
// as the core reads it (gjson over the body) and apply as the core applies
// them (sjson).
func TestSystemItemsBecomeDeveloper(t *testing.T) {
	h := startPlugin(t, mustPlugin(DefaultEndpoints))
	body := `{"model":"gpt-5.4","input":[{"role":"system","content":"be brief"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},` +
		`{"role":"user","content":"hi"},{"type":"message","role":"system","content":[{"type":"input_text","text":"x"}]},{"role":"developer","content":"d"},"odd",{"role":{"nested":"system"}}]}`
	roles := gjson.Get(body, FieldSystemRoles).Raw
	if roles != `[["system"],[],[],["system"],[],[],[]]` {
		t.Fatalf("roles field = %s", roles)
	}
	for _, typ := range accountTypes {
		for _, proto := range []string{ProtocolResponses, ProtocolResponsesWS} {
			r, err := h.Platform.BuildUpstreamRequest(context.Background(), &pluginv1.BuildUpstreamRequestRequest{
				Meta: &pluginv1.RequestMeta{Protocol: proto, Model: "gpt-5.4"}, Account: account(typ, deviceCreds),
				Fields: map[string]string{"model": `"gpt-5.4"`, FieldHasInstructions: "1", FieldSystemRoles: roles},
			})
			if err != nil {
				t.Fatal(err)
			}
			sets, _ := patchMap(r)
			if sets["input.0.role"] != `"developer"` || sets["input.3.role"] != `"developer"` {
				t.Fatalf("%s %s sets = %v", typ, proto, sets)
			}
			out := body
			for _, p := range r.GetPatches() {
				if p.GetOp() == pluginv1.BodyPatch_OP_SET {
					out, err = sjson.SetRaw(out, p.GetPath(), p.GetValueJson())
				} else {
					out, err = sjson.Delete(out, p.GetPath())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			got := gjson.Get(out, "input.#.role").Raw
			if got != `["developer","user","developer","developer",{"nested":"system"}]` || gjson.Get(out, "input.1.type").String() != "function_call" ||
				gjson.Get(out, "input.5").String() != "odd" || gjson.Get(out, "input.0.content").String() != "be brief" {
				t.Fatalf("%s %s patched input = %s", typ, proto, gjson.Get(out, "input").Raw)
			}
		}
	}
	// Absent (string or missing input), malformed or system-free fields
	// patch no role.
	for _, raw := range []string{"", "[]", `[[],[]]`, `{}`, `"system"`, `[["user"]]`} {
		if ps := systemToDeveloper(raw); len(ps) != 0 {
			t.Errorf("systemToDeveloper(%s) = %v", raw, ps)
		}
	}
	for _, b := range []string{`{"input":"system"}`, `{}`, `{"input":{"role":"system"}}`} {
		if ps := systemToDeveloper(gjson.Get(b, FieldSystemRoles).Raw); len(ps) != 0 {
			t.Errorf("%s: %v", b, ps)
		}
	}
	// At most maxRoleRewrites patches.
	many := "[" + strings.TrimSuffix(strings.Repeat(`["system"],`, maxRoleRewrites+10), ",") + "]"
	if ps := systemToDeveloper(many); len(ps) != maxRoleRewrites || ps[len(ps)-1].GetPath() != fmt.Sprintf("input.%d.role", maxRoleRewrites-1) {
		t.Fatalf("capped patches = %d", len(ps))
	}
}

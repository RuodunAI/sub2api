package conformance

import (
	"bytes"
	"cmp"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// fake is a minimal platform plugin that follows every rule, unless
// violate names one violation to commit.
type fake struct {
	violate string

	mu    sync.Mutex
	now   time.Time
	cache map[string]*pluginv1.UpstreamRequest // only for the failover violation
	nonce atomic.Int64
}

const (
	wsEndpoint = "/v1/ws"
	tokenURL   = "https://auth.conformance.invalid/token"
)

func (f *fake) setClock(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

func (f *fake) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fake) Execute(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
	switch {
	case f.violate == "no relay":
		return &pluginv1.ExecuteResponse{}, nil
	case f.violate == "nothing after waiting" && in.GetExtractUsage():
		request, err := f.BuildUpstream(ctx, in)
		if err != nil {
			return nil, err
		}
		_, err = pluginsdk.RelayAndWait(ctx, request)
		return &pluginv1.ExecuteResponse{}, err
	case f.violate == "ignores cancellation" && !in.GetExtractUsage() && !in.GetTaskSubmission():
		request, err := f.BuildUpstream(ctx, in)
		if err != nil {
			return nil, err
		}
		if err := pluginsdk.Relay(context.WithoutCancel(ctx), request); err != nil {
			return nil, err
		}
		return pluginsdk.ReportHostUsage(), nil
	}
	return pluginsdk.ExecuteDefault(ctx, f, in)
}

func (f *fake) BuildUpstream(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error) {
	if cached := f.cached(in); cached != nil {
		return cached, nil
	}
	credential, err := f.credential(ctx, in.GetAccount())
	if err != nil {
		return nil, err
	}
	body := f.body(in.GetBody())
	if f.violate == "dials" {
		if resp, err := http.DefaultClient.Get("https://dial.conformance.invalid/"); err == nil {
			_ = resp.Body.Close()
		}
	}
	u := pluginsdk.NewUpstream(http.MethodPost, "https://upstream.conformance.invalid/v1/chat")
	if in.GetMeta().GetEndpoint() == wsEndpoint {
		u = pluginsdk.NewUpstream(http.MethodGet, "wss://upstream.conformance.invalid/v1/ws")
	}
	u.Header("x-conformance-date", f.clock().Format(time.RFC3339)).
		DefaultHeader("content-type", "application/json").
		Body(body).Model(pluginsdk.RequestModel(in))
	if f.violate != "drops a client header" {
		for name, value := range in.GetInboundHeaders() {
			u.DefaultHeader(name, value)
		}
	}
	f.headerViolations(u, credential, in)
	request, err := u.Build()
	f.remember(in, request)
	return request, err
}

func (f *fake) headerViolations(u *pluginsdk.Upstream, credential string, in *pluginv1.ExecuteRequest) {
	switch f.violate {
	case "credential default":
		u.DefaultHeader("authorization", credential)
	case "undeclared authoritative":
		u.Header("authorization", credential).Header("x-conformance-extra", "1")
	case "nonce":
		u.Header("authorization", credential).DefaultHeader("x-conformance-nonce", strconv.FormatInt(f.nonce.Add(1), 10))
	case "response stream on a turn":
		u.Header("authorization", credential)
		if in.GetMeta().GetEndpoint() == wsEndpoint {
			u.ResponseStream("aws-eventstream-chunk")
		}
	default:
		u.Header("authorization", credential)
	}
}

func (f *fake) body(body []byte) []byte {
	switch f.violate {
	case "drops stream":
		body, _ = sjson.DeleteBytes(body, "stream")
	case "copies the body":
		for range 8 {
			body = bytes.Clone(body)
		}
	}
	return body
}

// credential is the API key, or a token fetched for the refresh token.
func (f *fake) credential(ctx context.Context, a *pluginv1.Account) (string, error) {
	creds := gjson.Parse(a.GetCredentialsJson())
	if a.GetType() != "oauth" {
		return "Bearer " + creds.Get("api_key").String(), nil
	}
	exchange := &pluginv1.ExecutionHTTPRequest{Method: http.MethodPost, Url: tokenURL, Body: []byte(`{"refresh_token":"` + creds.Get("refresh_token").String() + `"}`)}
	answer, err := pluginsdk.Fetch(ctx, exchange)
	if err != nil {
		return "", err
	}
	if answer.GetTransportError() != "" || answer.GetStatus() >= 300 {
		if f.violate == "refetches" {
			_, _ = pluginsdk.Fetch(ctx, exchange)
		}
		return "", pluginsdk.Classified(&pluginv1.ClassifyErrorResponse{Reason: "token exchange failed"})
	}
	return "Bearer " + gjson.GetBytes(answer.GetBody(), "access_token").String(), nil
}

// cached and remember are the failover violation: a per-request cache.
func (f *fake) cached(in *pluginv1.ExecuteRequest) *pluginv1.UpstreamRequest {
	if f.violate != "reuses" || in.GetAttempt() == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cache[in.GetMeta().GetRequestId()]
}

func (f *fake) remember(in *pluginv1.ExecuteRequest, r *pluginv1.UpstreamRequest) {
	if f.violate != "reuses" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = map[string]*pluginv1.UpstreamRequest{}
	}
	f.cache[in.GetMeta().GetRequestId()] = r
}

func (f *fake) ExtractUsage(_ context.Context, in *pluginv1.ExtractUsageRequest) (*pluginv1.UsageReport, error) {
	return &pluginv1.UsageReport{Tokens: &pluginv1.UsageTokens{OutputTokens: gjson.GetBytes(in.GetBody(), "usage.output_tokens").Int()}}, nil
}

func (f *fake) ParseTaskSubmission(_ context.Context, in *pluginv1.ExtractUsageRequest) (*pluginv1.TaskSubmission, error) {
	return &pluginv1.TaskSubmission{UpstreamRefId: gjson.GetBytes(in.GetBody(), "id").String()}, nil
}

func (f *fake) ClassifyError(context.Context, *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error) {
	return &pluginv1.ClassifyErrorResponse{Reason: "upstream failed"}, nil
}

func (f *fake) ValidateCredentials(context.Context, *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
	return &pluginv1.ValidateCredentialsResponse{}, nil
}

func (f *fake) BuildTestRequest(context.Context, *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
	return &pluginv1.BuildTestRequestResponse{}, nil
}

const fakeManifest = `{
  "key": "conformance-fake", "version": "1.0.0",
  "capabilities": [{"id": "platform.adapter.v1"}, {"id": "platform.websocket.v1"}],
  "platforms": [{"id": "conformance", "passHeaders": ["x-conformance-beta"], "endpoints": [
    {"id": "chat", "method": "POST", "path": "/v1/chat", "protocol": "conformance.chat", "kind": "proxy",
     "request": {"modelPath": "model", "streamPath": "stream"}, "response": {"stream": "sse", "nonStream": "json"}},
    {"id": "count", "method": "POST", "path": "/v1/count", "protocol": "conformance.count", "kind": "proxy",
     "request": {"modelPath": "model"}, "response": {"nonStream": "json"}},
    {"id": "ws", "method": "GET", "path": "/v1/ws", "protocol": "conformance.ws", "kind": "websocket",
     "request": {"modelPath": "model"}, "response": {"stream": "websocket", "nonStream": "json"}}
  ]}],
  "accountTypes": [
    {"id": "apikey", "platforms": [{"platform": "conformance"}]},
    {"id": "oauth", "platforms": [{"platform": "conformance", "passHeaders": ["x-conformance-beta", "x-conformance-oauth"]}]}
  ]
}`

func fakePlugin(violate string) Plugin {
	f := &fake{violate: violate}
	return Plugin{
		Platform:    f,
		Manifest:    []byte(fakeManifest),
		Protocols:   []string{"conformance.chat", "conformance.count"},
		Credentials: []string{"authorization", "x-conformance-date"},
		Fetch: func(_ context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
			token := "at-" + gjson.GetBytes(in.GetBody(), "refresh_token").String()
			return &pluginv1.ExecutionHTTPResponse{Status: 200, Body: []byte(`{"access_token":"` + token + `"}`)}, nil
		},
		FixClock:    f.setClock,
		BodyLimit:   1 << 20,
		CallTimeout: 300 * time.Millisecond,
	}
}

func fakeCases() []Case {
	apikey := &pluginv1.Account{Id: 1, Type: "apikey", CredentialsJson: `{"api_key":"sk-conformance-key"}`}
	oauth := &pluginv1.Account{Id: 2, Type: "oauth", CredentialsJson: `{"refresh_token":"rt-conformance-token"}`}
	meta := func(stream bool, endpoint string) *pluginv1.RequestMeta {
		protocol := map[string]string{"/v1/count": "conformance.count", wsEndpoint: "conformance.ws"}[endpoint]
		return &pluginv1.RequestMeta{Protocol: cmp.Or(protocol, "conformance.chat"), Model: "m1", Stream: stream, Endpoint: endpoint}
	}
	return []Case{
		{Name: "apikey", Account: apikey, Meta: meta(false, "/v1/chat"), Body: []byte(`{"model":"m1","messages":[]}`)},
		{Name: "apikey stream", Account: apikey, Meta: meta(true, "/v1/chat"), Body: []byte(`{"model":"m1","stream":true}`)},
		{Name: "oauth", Account: oauth, Meta: meta(false, "/v1/chat"), Body: []byte(`{"model":"m1"}`)},
		{Name: "count", Account: apikey, Meta: meta(false, "/v1/count"), Body: []byte(`{"model":"m1"}`)},
		{Name: "usage", Account: apikey, Meta: meta(false, "/v1/usage"), Body: []byte(`{"model":"m1"}`), Endpoint: PluginUsage,
			Observed: pluginsdktest.Observed(200, []byte(`{"usage":{"output_tokens":7}}`))},
		{Name: "task", Account: apikey, Meta: meta(false, "/v1/tasks"), Body: []byte(`{"model":"m1"}`), Endpoint: TaskSubmission,
			Observed: pluginsdktest.Observed(200, []byte(`{"id":"task-1"}`))},
		{Name: "turn", Account: apikey, Meta: meta(false, wsEndpoint), Body: []byte(`{"type":"response.create","model":"m1"}`), WebSocket: true},
	}
}

// record runs the suite (only the rule named only, when set) and returns
// the failures of each rule instead of failing.
func record(t *testing.T, p Plugin, only string) map[string][]string {
	out := map[string][]string{}
	setup := runItem(t, "setup", "", true, func(tb testing.TB) {
		run(tb, p, fakeCases(), func(name string, fn func(testing.TB)) {
			if only == "" || name == only {
				out[name] = runItem(tb, name, "", true, fn).failures
			}
		})
	})
	if setup.Failed() {
		out["setup"] = setup.failures
	}
	return out
}

func TestConformingPluginPasses(t *testing.T) {
	got := record(t, fakePlugin(""), "")
	if len(got) != len(checks) {
		t.Fatalf("ran %d rules, want %d: %v", len(got), len(checks), got)
	}
	for name, failures := range got {
		if len(failures) > 0 {
			t.Errorf("%s: %v", name, failures)
		}
	}
}

// Every rule catches the violation of it: one violation per rule (three
// for the headers).
func TestEveryRuleCatchesItsViolation(t *testing.T) {
	for _, v := range []struct{ violate, rule string }{
		{"no relay", "01_one_relay"},
		{"nothing after waiting", "02_form_and_report"},
		{"drops stream", "03_body"},
		{"undeclared authoritative", "04_headers"},
		{"credential default", "04_headers"},
		{"drops a client header", "04_headers"},
		{"refetches", "05_no_retry"},
		{"nonce", "06_deterministic"},
		{"reuses", "07_failover"},
		{"dials", "08_no_sockets"},
		{"ignores cancellation", "09_cancellation"},
		{"copies the body", "10_large_body"},
		{"response stream on a turn", "11_websocket"},
	} {
		t.Run(v.violate, func(t *testing.T) {
			if v.rule == "10_large_body" && raceEnabled {
				t.Skip("the large-body rule is skipped under -race")
			}
			failures := record(t, fakePlugin(v.violate), v.rule)[v.rule]
			if len(failures) == 0 {
				t.Fatalf("%s did not catch %q", v.rule, v.violate)
			}
			t.Logf("%s caught it: %s", v.rule, failures[0])
		})
	}
}

func TestExceptions(t *testing.T) {
	excuse := Exception{Check: "03_body", Case: "apikey stream", Reason: "the upstream streams by URL"}
	violator := fakePlugin("drops stream")
	violator.Exceptions = []Exception{excuse}
	if got := record(t, violator, "03_body")["03_body"]; len(got) > 0 {
		t.Errorf("an excused violation failed: %v", got)
	}

	conforming := fakePlugin("")
	conforming.Exceptions = []Exception{excuse}
	if got := record(t, conforming, "03_body")["03_body"]; len(got) != 1 || !strings.Contains(got[0], "no longer needed") {
		t.Errorf("an unneeded exception: %v", got)
	}

	conforming.Exceptions = []Exception{{Check: "03_body", Case: "apikey stream"}, {Check: "03_body", Case: "nonexistent", Reason: "x"}}
	got := strings.Join(record(t, conforming, "03_body")["setup"], "\n")
	if !strings.Contains(got, "has no reason") || !strings.Contains(got, "names no rule and case") {
		t.Errorf("bad exceptions were accepted: %q", got)
	}
}

// The forms of a protocol come from its endpoint: a missing one is reported,
// an invented one (a stream case of an endpoint without a stream form, a
// turn of an HTTP protocol) too, and a WebSocket protocol is not listed.
func TestForms(t *testing.T) {
	failures := func(p Plugin, cases []Case) string {
		var out []string
		setup := runItem(t, "setup", "", true, func(tb testing.TB) {
			run(tb, p, cases, func(name string, fn func(testing.TB)) {
				if name == "01_one_relay" {
					out = runItem(tb, name, "", true, fn).failures
				}
			})
		})
		return strings.Join(append(out, setup.failures...), "\n")
	}
	cases := fakeCases()
	if got := failures(fakePlugin(""), append(cases[:1:1], cases[2:]...)); !strings.Contains(got, "coverage: conformance.chat stream: no HTTP case covers it") {
		t.Errorf("a missing form: %q", got)
	}
	invented := append(fakeCases(), Case{Name: "count stream", Account: cases[0].Account, Body: []byte(`{"model":"m1","stream":true}`),
		Meta: &pluginv1.RequestMeta{Protocol: "conformance.count", Model: "m1", Stream: true}})
	if got := failures(fakePlugin(""), invented); !strings.Contains(got, `coverage: count stream: the core never sends protocol "conformance.count" as a stream request`) {
		t.Errorf("an invented stream form: %q", got)
	}
	turn := fakeCases()
	turn[len(turn)-1].Meta.Protocol = "conformance.chat"
	if got := failures(fakePlugin(""), turn); !strings.Contains(got, `has no websocket endpoint`) {
		t.Errorf("a turn of an HTTP protocol: %q", got)
	}
	ws := fakePlugin("")
	ws.Protocols = append(ws.Protocols, "conformance.ws")
	if got := failures(ws, fakeCases()); !strings.Contains(got, "is a WebSocket endpoint's protocol") {
		t.Errorf("a WebSocket protocol in Protocols: %q", got)
	}
}

// The client headers come from the manifest: without one, or with a case
// on an account type it does not declare, the suite cannot tell what the
// core hands over and refuses to start.
func TestClientHeadersFromTheManifest(t *testing.T) {
	p := fakePlugin("")
	p.Manifest = nil
	if got := strings.Join(record(t, p, "04_headers")["setup"], "\n"); !strings.Contains(got, "Plugin.Manifest is required") {
		t.Errorf("no manifest: %q", got)
	}
	cases := fakeCases()
	cases[0].Account = &pluginv1.Account{Id: 9, Type: "undeclared", CredentialsJson: `{"api_key":"k"}`}
	setup := runItem(t, "setup", "", true, func(tb testing.TB) { run(tb, fakePlugin(""), cases, func(string, func(testing.TB)) {}) })
	if got := strings.Join(setup.failures, "\n"); !strings.Contains(got, `no account type "undeclared"`) {
		t.Errorf("undeclared account type: %q", got)
	}
}

func TestStaleAuthoritativeHeader(t *testing.T) {
	p := fakePlugin("")
	p.Authoritative = map[string]string{"x-conformance-never": "kept as today"}
	got := record(t, p, "04_headers")["04_headers"]
	if len(got) != 1 || !strings.Contains(got[0], "x-conformance-never") {
		t.Errorf("a stale declaration: %v", got)
	}
	p.Authoritative = map[string]string{"x-conformance-never": ""}
	if got := record(t, p, "04_headers")["setup"]; len(got) == 0 {
		t.Errorf("an authoritative header without a reason was accepted")
	}
}

func TestDuplicateKey(t *testing.T) {
	for body, want := range map[string]string{
		`{"a":1,"b":{"c":[{"d":1},{"d":2}]}}`: "",
		`{"a":1,"a":2}`:                       "a",
		`{"a":{"b":[1,{"c":1,"c":2}]}}`:       "a.b.c",
		`[{"x":{}},{"x":{}}]`:                 "",
		`{"a":{"x":1},"b":{"x":1},"b":2}`:     "b",
	} {
		if got := duplicateKey([]byte(body)); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}

// TestRun is the one line a plugin's test needs.
func TestRun(t *testing.T) { Run(t, fakePlugin(""), fakeCases()...) }

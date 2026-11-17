package upstream

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/paramoverride"
)

// The golden test pins the four presets to the plugins they replace; these
// cover what no plugin did: templates, query auth, body filters, the fixed
// rewrite and the URL re-render.

func resolve(t *testing.T, upstream string, passHeaders ...string) *Profile {
	t.Helper()
	at := &manifest.AccountType{ID: "custom", Upstream: &manifest.Upstream{}}
	if err := json.Unmarshal([]byte(upstream), at.Upstream); err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(at, passHeaders)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func account(settings string) *pluginv1.Account {
	return &pluginv1.Account{Id: 7, Type: "custom", CredentialsJson: `{"api_key":"k-123456789","org":"o-1"}`, SettingsJson: settings}
}

func TestTemplatesAndQueryAuth(t *testing.T) {
	p := resolve(t, `{
		"preset": "gemini",
		"baseURL": {"required": true},
		"credentials": {"fields": ["org"]},
		"auth": {"type": "query", "name": "key", "value": "{api_key}"},
		"headers": {"x-org": {"set": "{credentials.org}"}, "x-ua": {"default": "s2a/{client_header.user-agent}"}, "x-none": {"set": "{credentials.org}-{client_header.x-missing}"}}
	}`, "user-agent", "x-missing")
	out, err := p.Build(&pluginv1.BuildUpstreamRequestRequest{
		Meta:           &pluginv1.RequestMeta{Protocol: "gemini.stream_generate", Model: "models/a b"},
		Account:        account(`{"base_url":"https://g.example.com"}`),
		InboundHeaders: map[string]string{"user-agent": " cli/1 "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://g.example.com/v1beta/models/a%20b:streamGenerateContent?alt=sse&key=k-123456789"; out.Url != want {
		t.Errorf("url %s, want %s", out.Url, want)
	}
	want := map[string]string{"content-type": "application/json", "x-org": "o-1", "user-agent": "cli/1", "x-ua": "s2a/cli/1"}
	if !reflect.DeepEqual(out.Headers, want) {
		t.Errorf("headers %v, want %v", out.Headers, want)
	}
}

func TestHeaderAppend(t *testing.T) {
	p := resolve(t, `{"preset":"openai"}`, "openai-beta")
	for inbound, want := range map[string]string{
		"":                                "responses_websockets=2026-02-06",
		"assistants=v2":                   "assistants=v2,responses_websockets=2026-02-06",
		"responses_websockets=2025-01-01": "responses_websockets=2025-01-01",
	} {
		out, err := p.Build(&pluginv1.BuildUpstreamRequestRequest{
			Meta: &pluginv1.RequestMeta{Protocol: "openai.responses_ws"}, Account: account(""),
			InboundHeaders: map[string]string{"openai-beta": inbound},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := out.Headers["openai-beta"]; got != want || out.Method != "GET" || out.Url != "wss://api.openai.com/v1/responses" {
			t.Errorf("%q: %s %s openai-beta %q, want %q", inbound, out.Method, out.Url, got, want)
		}
		if _, ok := out.Headers["content-type"]; ok {
			t.Errorf("%q: websocket handshake keeps content-type", inbound)
		}
	}
}

func TestPrepareBodyAndURL(t *testing.T) {
	p := resolve(t, `{"preset":"gemini","filterFields":["cachedContent"],"paramOverride":{"operations":[{"mode":"set","path":"labels.via","value":"s2a"}]}}`)
	res, err := p.PrepareBody([]byte(`{"cachedContent":"c","contents":[]}`), &paramoverride.Context{})
	if err != nil || string(res.Body) != `{"contents":[],"labels":{"via":"s2a"}}` {
		t.Errorf("body %s, %v", res.Body, err)
	}
	in := &pluginv1.BuildUpstreamRequestRequest{Meta: &pluginv1.RequestMeta{Protocol: "gemini.generate", Model: "a"}, Account: account("")}
	u, err := p.URL(in, "b")
	if err != nil || u != "https://generativelanguage.googleapis.com/v1beta/models/b:generateContent" {
		t.Errorf("url %s, %v", u, err)
	}
}

func TestResolveProblems(t *testing.T) {
	at := &manifest.AccountType{ID: "x", Upstream: &manifest.Upstream{}}
	_ = json.Unmarshal([]byte(`{"preset":"anthropic","routes":{"anthropic.messages":{"path":"v1"}},"test":{"protocol":"none"}}`), at.Upstream)
	_, err := Resolve(at, nil)
	var probs Problems
	if !errors.As(err, &probs) {
		t.Fatalf("error %v", err)
	}
	got := map[string]string{}
	for _, p := range probs {
		got[p.Path] = p.Code
	}
	want := map[string]string{"routes.anthropic.messages.path": "invalid", "test.protocol": "unknown_protocol"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("problems %v, want %v", got, want)
	}
}

// A route's null removes the shared write of that header for the route only.
func TestRouteHeaderTombstone(t *testing.T) {
	p := resolve(t, `{"preset":"openai","routes":{"openai.embeddings":{"headers":{"content-type":null}}}}`)
	for proto, want := range map[string]string{"openai.embeddings": "", "openai.chat": "application/json"} {
		out, err := p.Build(&pluginv1.BuildUpstreamRequestRequest{Meta: &pluginv1.RequestMeta{Protocol: proto}, Account: account("")})
		if err != nil || out.Headers["content-type"] != want {
			t.Errorf("%s: content-type %q, want %q (%v)", proto, out.GetHeaders()["content-type"], want, err)
		}
	}
}

// classify names one of the provider policies; azure_openai is the openai
// policy that also fails a missing deployment over to another account.
func TestClassifyPolicies(t *testing.T) {
	at := &manifest.AccountType{ID: "x", Upstream: &manifest.Upstream{}}
	_ = json.Unmarshal([]byte(`{"preset":"openai","classify":"azure"}`), at.Upstream)
	_, err := Resolve(at, nil)
	var probs Problems
	if !errors.As(err, &probs) || len(probs) != 1 || probs[0].Path != "classify" || probs[0].Code != "invalid" ||
		probs[0].Message != "classify must be anthropic, azure_openai, gemini or openai" {
		t.Fatalf("unknown policy: %v", err)
	}

	const (
		toClient = pluginv1.ClassifyErrorResponse_ACTION_RETURN_TO_CLIENT
		failover = pluginv1.ClassifyErrorResponse_ACTION_FAILOVER
		none     = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED
		disable  = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE
	)
	for _, tc := range []struct {
		policy string
		status int32
		body   string
		action pluginv1.ClassifyErrorResponse_Action
		effect pluginv1.ClassifyErrorResponse_AccountEffect
	}{
		{"openai", 404, `{"error":{"code":"DeploymentNotFound","message":"no such deployment"}}`, toClient, none},
		{"azure_openai", 404, `{"error":{"code":"DeploymentNotFound","message":"no such deployment"}}`, failover, none},
		// Only that code of that status: the rest is the openai policy.
		{"azure_openai", 404, `{"error":{"code":"model_not_found","message":"no such model"}}`, toClient, none},
		{"azure_openai", 400, `{"error":{"code":"DeploymentNotFound","message":"no such deployment"}}`, toClient, none},
		{"azure_openai", 401, `{"error":{"code":"401","message":"bad key"}}`, failover, disable},
	} {
		p := resolve(t, `{"preset":"openai","classify":"`+tc.policy+`"}`)
		out := p.Classify(time.Unix(1_800_000_000, 0), &pluginv1.ClassifyErrorRequest{
			Meta: &pluginv1.RequestMeta{Protocol: "openai.chat"}, Status: tc.status, BodyPrefix: []byte(tc.body),
		})
		if out.GetAction() != tc.action || out.GetAccountEffect() != tc.effect || out.GetCooldownUntilUnix() != 0 {
			t.Errorf("%s %d %s: %v %v", tc.policy, tc.status, tc.body, out.GetAction(), out.GetAccountEffect())
		}
	}
}

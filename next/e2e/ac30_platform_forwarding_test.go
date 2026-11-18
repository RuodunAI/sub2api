package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// platformCase is one platform plugin's account type, driven through the
// gateway against the mock.
type platformCase struct {
	name, plugin, typ string
	family            string      // model family of the run price ("claude", "gpt", "gemini")
	usage             usageFamily // how the mock's usage is billed
	streams           []bool      // the request forms the protocol has
	keyPrefix         string      // a vendor prefix the key must start with
	// creds is the account's credentials for upstream key k.
	creds func(e *Env, k string) map[string]any
	// call sends the request for model through the gateway.
	call func(e *Env, apiKey, model, prompt string, stream bool) *GatewayResult
	// check inspects the recorded upstream request.
	check func(t *testing.T, r MockRequest, k, model, prompt string, stream bool)
}

func messagesCall(e *Env, apiKey, model, prompt string, stream bool) *GatewayResult {
	return e.Messages(apiKey, MessagesBody(model, prompt, stream), nil)
}

func chatCall(e *Env, apiKey, model, prompt string, stream bool) *GatewayResult {
	return e.OpenAI(apiKey, "/v1/chat/completions", ChatBody(model, prompt, stream))
}

func responsesCall(e *Env, apiKey, model, prompt string, stream bool) *GatewayResult {
	return e.OpenAI(apiKey, "/v1/responses", map[string]any{"model": model, "input": prompt, "stream": stream})
}

func geminiCall(e *Env, apiKey, model, prompt string, stream bool) *GatewayResult {
	if stream {
		return e.Gemini(apiKey, model, "streamGenerateContent", "alt=sse", GeminiBody(prompt))
	}
	return e.Gemini(apiKey, model, "generateContent", "", GeminiBody(prompt))
}

// expectPath requires the recorded path (and Host, "" = the mock's own).
func expectPath(t *testing.T, r MockRequest, host, path string) {
	t.Helper()
	if r.Path != path || (host != "" && r.Host != host) || (host == "" && r.TLS) || r.Proxied {
		t.Fatalf("upstream %s %s (tls=%v proxied=%v), want %s %s direct", r.Host, r.Path, r.TLS, r.Proxied, host, path)
	}
}

func platformCases() []platformCase {
	keyURL := func(e *Env, k string) map[string]any {
		return map[string]any{"api_key": k, "base_url": e.MockInternalURL}
	}
	both := []bool{false, true}
	return []platformCase{
		{name: "anthropic", plugin: AnthropicPlugin, typ: AnthropicAPIKey, family: "claude", usage: usageAnthropic, streams: both,
			creds: keyURL, call: messagesCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				expectPath(t, r, "", "/v1/messages")
				if r.Header("x-api-key") != k || r.Header("anthropic-version") == "" {
					t.Fatalf("anthropic headers: %s", r.Headers.Raw)
				}
				validJSONBody(t, r, model, prompt, "anthropic")
			}},
		{name: "relay", plugin: RelayPlugin, typ: RelayKey, family: "claude", usage: usageAnthropic, streams: both,
			creds: keyURL, call: messagesCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				expectPath(t, r, "", "/v1/messages")
				if r.Header("x-api-key") != k {
					t.Fatalf("relay headers: %s", r.Headers.Raw)
				}
				validJSONBody(t, r, model, prompt, "relay")
			}},
		{name: "openai", plugin: OpenAIPlugin, typ: APIKeyType, family: "gpt", usage: usageOpenAI, streams: both, keyPrefix: "sk-",
			creds: keyURL, call: chatCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				expectPath(t, r, "", "/v1/chat/completions")
				if r.Header("authorization") != "Bearer "+k {
					t.Fatalf("openai headers: %s", r.Headers.Raw)
				}
				validJSONBody(t, r, model, prompt, "openai")
				if stream && !gjson.Get(r.Body, "stream_options.include_usage").Bool() {
					t.Fatalf("openai stream without include_usage: %s", r.Body)
				}
			}},
		{name: "gemini", plugin: GeminiPlugin, typ: APIKeyType, family: "gemini", usage: usageGemini, streams: both,
			creds: keyURL, call: geminiCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				action := "generateContent"
				if stream {
					action = "streamGenerateContent"
				}
				expectPath(t, r, "", "/v1beta/models/"+model+":"+action)
				if r.Key() != k {
					t.Fatalf("gemini key: %+v", r)
				}
				validJSONBody(t, r, "", prompt, "gemini")
			}},
		{name: "azure_openai", plugin: AzurePlugin, typ: AzureV1Type, family: "gpt", usage: usageOpenAI, streams: both,
			creds: keyURL, call: chatCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				expectPath(t, r, "", "/openai/v1/chat/completions")
				if r.Header("api-key") != k || r.Header("authorization") != "" {
					t.Fatalf("azure headers: %s", r.Headers.Raw)
				}
				validJSONBody(t, r, model, prompt, "azure")
			}},
		{name: "volcengine", plugin: "volcengine", typ: "relay", family: "gpt", usage: usageOpenAI, streams: both,
			creds: keyURL, call: chatCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				expectPath(t, r, "", "/v1/chat/completions")
				if r.Header("authorization") != "Bearer "+k {
					t.Fatalf("volcengine headers: %s", r.Headers.Raw)
				}
				validJSONBody(t, r, model, prompt, "volcengine")
			}},
		// No configurable address: the plugins call the vendor hosts, which
		// the e2e network resolves to the mock (TLS, the mock's CA).
		{name: "vertex", plugin: "vertex", typ: "vertex_apikey", family: "gemini", usage: usageGemini, streams: both,
			creds: func(_ *Env, k string) map[string]any { return map[string]any{"api_key": k} }, call: geminiCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				action := "generateContent"
				if stream {
					action = "streamGenerateContent"
				}
				expectPath(t, r, "aiplatform.googleapis.com", "/v1/publishers/google/models/"+model+":"+action)
				if r.Key() != k || !r.TLS {
					t.Fatalf("vertex key/tls: %+v", r)
				}
				validJSONBody(t, r, "", prompt, "vertex")
			}},
		{name: "bedrock", plugin: "bedrock", typ: "bedrock_aksk", family: "claude", usage: usageAnthropic, streams: both,
			creds: func(_ *Env, k string) map[string]any {
				// The mock checks SigV4 with the secret mock-secret-<access key id>.
				return map[string]any{"access_key_id": k, "secret_access_key": "mock-secret-" + k, "region": "us-east-1", "inference_profile_prefix": "none"}
			},
			call: messagesCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				action := "invoke"
				if stream {
					action = "invoke-with-response-stream"
				}
				expectPath(t, r, "bedrock-runtime.us-east-1.amazonaws.com", "/model/"+model+"/"+action)
				if r.Key() != k || !strings.HasPrefix(r.Header("authorization"), "AWS4-HMAC-SHA256") || r.Header("x-amz-date") == "" {
					t.Fatalf("bedrock signature headers: %s", r.Headers.Raw)
				}
				// InvokeModel names the model in the path and the version in the body.
				if !strings.Contains(r.Body, prompt) || gjson.Get(r.Body, "model").Exists() || gjson.Get(r.Body, "anthropic_version").String() == "" {
					t.Fatalf("bedrock body: %s", r.Body)
				}
			}},
		{name: "claude_oauth", plugin: "claude_oauth", typ: "claude_setup_token", family: "claude", usage: usageAnthropic, streams: both,
			creds: func(_ *Env, k string) map[string]any { return map[string]any{"access_token": k} }, call: messagesCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				expectPath(t, r, "api.anthropic.com", "/v1/messages")
				if r.Header("authorization") != "Bearer "+k || r.Header("x-api-key") != "" || !r.TLS {
					t.Fatalf("claude_oauth headers: %s", r.Headers.Raw)
				}
				validJSONBody(t, r, model, "", "claude_oauth")
			}},
		// The Codex backend only streams: a non-stream request is sent as a
		// stream (streamOnly) and answered as JSON.
		{name: "codex", plugin: "codex", typ: "codex_oauth", family: "gpt", usage: usageOpenAI, streams: both,
			creds: func(_ *Env, k string) map[string]any {
				return map[string]any{"access_token": k, "chatgpt_account_id": "acct-e2e"}
			},
			call: responsesCall,
			check: func(t *testing.T, r MockRequest, k, model, prompt string, stream bool) {
				expectPath(t, r, "chatgpt.com", "/backend-api/codex/responses")
				if r.Header("authorization") != "Bearer "+k || r.Header("chatgpt-account-id") != "acct-e2e" || !r.TLS {
					t.Fatalf("codex headers: %s", r.Headers.Raw)
				}
				validJSONBody(t, r, model, prompt, "codex")
				if !gjson.Get(r.Body, "stream").Bool() || gjson.Get(r.Body, "store").Bool() {
					t.Fatalf("codex body must stream and not store: %s", r.Body)
				}
			}},
	}
}

// AC 30: every platform plugin forwards through Execute (forwarding v2).
// For each of the ten plugins, a non-stream and a stream request reach the
// mock as the request the plugin built (address, credential header, body),
// the client gets a complete answer, and the request is billed once from
// the usage the core read under the facts the plugin returned (host_usage).
func TestAC30_PlatformPluginsForwardThroughExecute(t *testing.T) {
	e := Setup(t)
	admin := e.Admin()
	m := e.Mock()
	for _, pc := range platformCases() {
		t.Run(pc.name, func(t *testing.T) {
			e := e.With(t)
			e.EnsurePlugin(admin, pc.plugin, "")
			st := e.newSingleTenant(admin, "grp-fwd-"+pc.name)
			k := pc.keyPrefix + fmt.Sprintf("AKIAE2E%s%d%s", strings.ToUpper(e.RunID), nextSeq(), "MOCKKEY")
			accountID := e.CreateAccount(admin, AccountSpec{PluginKey: pc.plugin, Type: pc.typ, GroupIDs: []int64{st.GroupID}, Credentials: pc.creds(e, k)})
			model := e.RunModelOf(admin, pc.family, "fwd-"+strings.ReplaceAll(pc.name, "_", "-"))
			for _, stream := range pc.streams {
				what := fmt.Sprintf("%s stream=%v", pc.name, stream)
				prompt := "fwd-" + e.Name(pc.name)
				mark := m.Mark(t)
				g := pc.call(e, st.APIKey, model, prompt, stream)
				if g.Status != 200 {
					t.Fatalf("%s: HTTP %d %s", what, g.Status, g.Body)
				}
				if stream && len(g.Events) == 0 {
					t.Fatalf("%s: no stream events: %s", what, g.Body)
				}
				if !stream && (len(g.Events) != 0 || !g.JSON().IsObject()) {
					t.Fatalf("%s: want a JSON answer, got %d events %s", what, len(g.Events), g.Body)
				}
				upstreamStream := stream || pc.name == "codex"
				r := oneMockCall(t, m, mark, k, what)
				pc.check(t, r, k, model, prompt, upstreamStream)
				e.expectBilled(admin, st.User.UserID, g.RequestID, accountID, pc.usage, stream)
			}
		})
	}
}

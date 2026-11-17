package bedrock

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/reqsign"
)

const (
	useast1 = "https://bedrock-runtime.us-east-1.amazonaws.com"
	// The model ids of the tests as one path segment.
	haikuSegment = "anthropic.claude-haiku-4-5-20251001-v1%3A0"
	arnSegment   = "arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Aapplication-inference-profile%2Fabc123def456"
)

// TestInvokeModelURLs covers every shape of model id
// (DESIGN-2026-11-17-CLOUD-PROVIDERS §9.2).
func TestInvokeModelURLs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		region, prefix string
		model          string
		stream         bool
		wantURL        string
		wantModel      string
	}{
		{"base id with a colon", "us-east-1", "", testHaiku, false, useast1 + "/model/" + haikuSegment + "/invoke", testHaiku},
		{"base id, streamed", "us-east-1", "", testHaiku, true, useast1 + "/model/" + haikuSegment + "/invoke-with-response-stream", testHaiku},
		{"base id without a colon", "us-east-1", "", "anthropic.claude-sonnet-4-6", false, useast1 + "/model/anthropic.claude-sonnet-4-6/invoke", "anthropic.claude-sonnet-4-6"},
		{"no region means us-east-1", "", "", testHaiku, false, useast1 + "/model/" + haikuSegment + "/invoke", testHaiku},
		{"another region", "ap-northeast-1", "", testHaiku, true,
			"https://bedrock-runtime.ap-northeast-1.amazonaws.com/model/" + haikuSegment + "/invoke-with-response-stream", testHaiku},
		{"govcloud region", "us-gov-west-1", "", testHaiku, false,
			"https://bedrock-runtime.us-gov-west-1.amazonaws.com/model/" + haikuSegment + "/invoke", testHaiku},
		{"inference profile id", "us-east-1", "", "us." + testHaiku, false, useast1 + "/model/us." + haikuSegment + "/invoke", "us." + testHaiku},
		{"prefix none", "us-east-1", NoProfilePrefix, testHaiku, false, useast1 + "/model/" + haikuSegment + "/invoke", testHaiku},
		{"prefix setting us", "us-east-1", "us", testHaiku, false, useast1 + "/model/us." + haikuSegment + "/invoke", "us." + testHaiku},
		{"prefix setting eu, streamed", "eu-central-1", "eu", testHaiku, true,
			"https://bedrock-runtime.eu-central-1.amazonaws.com/model/eu." + haikuSegment + "/invoke-with-response-stream", "eu." + testHaiku},
		{"prefix setting global", "us-east-1", "global", "anthropic.claude-sonnet-4-6", false,
			useast1 + "/model/global.anthropic.claude-sonnet-4-6/invoke", "global.anthropic.claude-sonnet-4-6"},
		{"prefix setting keeps an id that has one", "us-east-1", "eu", "us." + testHaiku, false, useast1 + "/model/us." + haikuSegment + "/invoke", "us." + testHaiku},
		{"prefix setting keeps a us-gov id", "us-gov-west-1", "us", "us-gov." + testHaiku, false,
			"https://bedrock-runtime.us-gov-west-1.amazonaws.com/model/us-gov." + haikuSegment + "/invoke", "us-gov." + testHaiku},
		{"ARN with a slash", "us-east-1", "", testARN, false, useast1 + "/model/" + arnSegment + "/invoke", testARN},
		{"ARN, streamed", "us-east-1", "", testARN, true, useast1 + "/model/" + arnSegment + "/invoke-with-response-stream", testARN},
		{"prefix setting keeps an ARN", "us-east-1", "us", testARN, false, useast1 + "/model/" + arnSegment + "/invoke", testARN},
		{"id is one segment whatever it holds", "us-east-1", "", "my model/v1?x#y", false, useast1 + "/model/my%20model%2Fv1%3Fx%23y/invoke", "my model/v1?x#y"},
		{"surrounding space trimmed", "us-east-1", "apac", "  " + testHaiku + " ", false, useast1 + "/model/apac." + haikuSegment + "/invoke", "apac." + testHaiku},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := build(t, buildRequest(stored(TypeAPIKey, apiKeyCreds, settingsOf(tc.region, tc.prefix)), ProtocolMessages, tc.model, tc.stream))
			if resp.GetMethod() != http.MethodPost || resp.GetUrl() != tc.wantURL || resp.GetUpstreamModel() != tc.wantModel {
				t.Fatalf("request = %s %s (model %s)\nwant POST %s (model %s)", resp.GetMethod(), resp.GetUrl(), resp.GetUpstreamModel(), tc.wantURL, tc.wantModel)
			}
			wantStream := ""
			if tc.stream {
				wantStream = "aws-eventstream"
			}
			if resp.GetResponseStream() != wantStream {
				t.Fatalf("response_stream = %q, want %q", resp.GetResponseStream(), wantStream)
			}
			// The model is exactly one path segment: /model/<id>/<action>.
			path := strings.TrimPrefix(resp.GetUrl(), "https://")
			if n := strings.Count(path[strings.Index(path, "/"):], "/"); n != 3 || strings.Contains(path, ":") {
				t.Fatalf("path of %s has %d slashes or a raw colon", resp.GetUrl(), n)
			}
		})
	}
}

func TestInvokeModelWithoutModel(t *testing.T) {
	_, err := newTestPlugin(Endpoints{}).BuildUpstreamRequest(context.Background(),
		buildRequest(stored(TypeAPIKey, apiKeyCreds, nil), ProtocolMessages, "  ", false))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no model: %v", err)
	}
}

// The OpenAI-compatible routes: the body, and the model in it, stay as the
// client sent them, so neither the prefix setting nor the framing applies.
func TestOpenAIRoutes(t *testing.T) {
	const model = "openai.gpt-oss-120b-1:0"
	for _, tc := range []struct {
		name        string
		protocol    string
		stream      bool
		wantPath    string
		wantPatches []string
	}{
		{"chat", ProtocolChat, false, "/openai/v1/chat/completions", nil},
		{"chat, streamed", ProtocolChat, true, "/openai/v1/chat/completions", []string{"set stream_options.include_usage=true"}},
		{"responses", ProtocolResponses, false, "/openai/v1/responses", nil},
		{"responses, streamed", ProtocolResponses, true, "/openai/v1/responses", nil},
	} {
		for typ, creds := range typeCreds {
			t.Run(tc.name+"/"+typ, func(t *testing.T) {
				resp := build(t, buildRequest(stored(typ, creds, settingsOf("us-west-2", "us")), tc.protocol, model, tc.stream))
				if resp.GetMethod() != http.MethodPost || resp.GetUrl() != "https://bedrock-runtime.us-west-2.amazonaws.com"+tc.wantPath || resp.GetUpstreamModel() != model {
					t.Fatalf("request = %s %s (model %s)", resp.GetMethod(), resp.GetUrl(), resp.GetUpstreamModel())
				}
				if got := patchList(resp.GetPatches()); !slices.Equal(got, tc.wantPatches) {
					t.Fatalf("patches = %v, want %v", got, tc.wantPatches)
				}
				if resp.GetResponseStream() != "" {
					t.Fatalf("response_stream = %q: the endpoint answers in SSE", resp.GetResponseStream())
				}
				wantHeaders := map[string]string{"content-type": "application/json"}
				if typ == TypeAPIKey {
					wantHeaders["authorization"] = "Bearer " + testAPIKey
				}
				if !maps.Equal(resp.GetHeaders(), wantHeaders) || (resp.GetSigning() != nil) != (typ != TypeAPIKey) {
					t.Fatalf("headers = %v, signing set = %v", resp.GetHeaders(), resp.GetSigning() != nil)
				}
			})
		}
	}
}

func TestUnsupportedProtocol(t *testing.T) {
	for _, protocol := range []string{"", "anthropic.count_tokens", "openai.embeddings", "openai.responses_ws", "gemini.generate"} {
		_, err := newTestPlugin(Endpoints{}).BuildUpstreamRequest(context.Background(),
			buildRequest(stored(TypeAPIKey, apiKeyCreds, nil), protocol, testHaiku, false))
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v", protocol, err)
		}
	}
}

// The InvokeModel body: the path carries model and stream, so both leave the
// body; anthropic_version is always set.
func TestInvokeModelPatches(t *testing.T) {
	base := []string{"delete model", "delete stream", `set anthropic_version="bedrock-2023-05-31"`}
	for _, tc := range []struct {
		name   string
		header string
		// present tells whether the client sent the header at all.
		present bool
		want    []string
	}{
		{"no beta header", "", false, nil},
		{"empty beta header", "  ", true, nil},
		{"supported feature", "context-1m-2025-08-07", true, []string{`set anthropic_beta=["context-1m-2025-08-07"]`}},
		{"several, spaced", " context-1m-2025-08-07 ,computer-use-2025-01-24,  compact-2026-01-12", true,
			[]string{`set anthropic_beta=["context-1m-2025-08-07","computer-use-2025-01-24","compact-2026-01-12"]`}},
		{"unsupported ones dropped", "claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,oauth-2025-04-20", true,
			[]string{`set anthropic_beta=["context-management-2025-06-27"]`}},
		{"only unsupported ones", "claude-code-20250219,oauth-2025-04-20,prompt-caching-2024-07-31", true, nil},
		{"duplicates once", "context-1m-2025-08-07,context-1m-2025-08-07,fine-grained-tool-streaming-2025-05-14", true,
			[]string{`set anthropic_beta=["context-1m-2025-08-07","fine-grained-tool-streaming-2025-05-14"]`}},
		{"advanced tool use is tool search on Bedrock, with tool examples", "advanced-tool-use-2025-11-20", true,
			[]string{`set anthropic_beta=["tool-search-tool-2025-10-19","tool-examples-2025-10-29"]`}},
		{"tool search brings tool examples", "tool-search-tool-2025-10-19,computer-use-2025-11-24", true,
			[]string{`set anthropic_beta=["tool-search-tool-2025-10-19","computer-use-2025-11-24","tool-examples-2025-10-29"]`}},
		{"tool examples not repeated", "tool-examples-2025-10-29,advanced-tool-use-2025-11-20,tool-search-tool-2025-10-19", true,
			[]string{`set anthropic_beta=["tool-examples-2025-10-29","tool-search-tool-2025-10-19"]`}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				in := buildRequest(stored(TypeAccessKey, akskCreds, nil), ProtocolMessages, testHaiku, stream)
				if tc.present {
					in.InboundHeaders = map[string]string{"anthropic-beta": tc.header, "user-agent": "claude-cli/2.1.90"}
				}
				resp := build(t, in)
				if got, want := patchList(resp.GetPatches()), append(slices.Clone(base), tc.want...); !slices.Equal(got, want) {
					t.Fatalf("patches = %v\nwant %v", got, want)
				}
				// No client header is forwarded, the beta header included.
				if want := map[string]string{"content-type": "application/json", "accept": "application/json"}; !maps.Equal(resp.GetHeaders(), want) {
					t.Fatalf("headers = %v", resp.GetHeaders())
				}
			})
		}
	}
}

// Every beta feature InvokeModel accepts passes, in the list's own order.
func TestSupportedBetasPass(t *testing.T) {
	if got := bedrockBetas(strings.Join(supportedBetas, ",")); !slices.Equal(got, supportedBetas) {
		t.Fatalf("betas = %v", got)
	}
	if len(supportedBetas) != 8 || !slices.Contains(supportedBetas, betaAliases["advanced-tool-use-2025-11-20"]) {
		t.Fatalf("supported = %v, aliases = %v", supportedBetas, betaAliases)
	}
}

// How a request is authorized, per account type.
func TestAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typ      string
		creds    map[string]any
		region   string
		wantAuth string
		want     *pluginv1.RequestSigning
	}{
		{"api key: bearer header, nothing to sign", TypeAPIKey, apiKeyCreds, "us-east-1", "Bearer " + testAPIKey, nil},
		{"access key: signed by the core", TypeAccessKey, akskCreds, "us-east-1", "", &pluginv1.RequestSigning{
			Scheme: "aws-sigv4", Region: "us-east-1", Service: "bedrock", AccessKeyId: testAccessKeyID, SecretAccessKey: mockSecret(testAccessKeyID)}},
		{"access key: the signing region is the account's", TypeAccessKey, akskCreds, "eu-west-3", "", &pluginv1.RequestSigning{
			Scheme: "aws-sigv4", Region: "eu-west-3", Service: "bedrock", AccessKeyId: testAccessKeyID, SecretAccessKey: mockSecret(testAccessKeyID)}},
		{"access key: a stray session token is not used", TypeAccessKey, with(akskCreds, map[string]any{fieldSessionToken: testSessionToken}), "us-east-1", "",
			&pluginv1.RequestSigning{Scheme: "aws-sigv4", Region: "us-east-1", Service: "bedrock", AccessKeyId: testAccessKeyID, SecretAccessKey: mockSecret(testAccessKeyID)}},
		{"temporary credentials: signed with the session token", TypeSTS, stsCreds, "us-west-2", "", &pluginv1.RequestSigning{
			Scheme: "aws-sigv4", Region: "us-west-2", Service: "bedrock", AccessKeyId: testSTSKeyID, SecretAccessKey: mockSecret(testSTSKeyID), SessionToken: testSessionToken}},
		{"credentials are trimmed", TypeSTS, map[string]any{fieldAccessKeyID: " " + testSTSKeyID, fieldSecretAccessKey: mockSecret(testSTSKeyID) + "\n", fieldSessionToken: "\t" + testSessionToken + " "},
			"us-west-2", "", &pluginv1.RequestSigning{
				Scheme: "aws-sigv4", Region: "us-west-2", Service: "bedrock", AccessKeyId: testSTSKeyID, SecretAccessKey: mockSecret(testSTSKeyID), SessionToken: testSessionToken}},
	} {
		for _, protocol := range []string{ProtocolMessages, ProtocolChat, ProtocolResponses} {
			for _, stream := range []bool{false, true} {
				t.Run(tc.name+"/"+protocol, func(t *testing.T) {
					resp := build(t, buildRequest(stored(tc.typ, tc.creds, settingsOf(tc.region, "")), protocol, testHaiku, stream))
					if got := resp.GetHeaders()["authorization"]; got != tc.wantAuth {
						t.Fatalf("authorization = %q, want %q", got, tc.wantAuth)
					}
					for name := range resp.GetHeaders() {
						if strings.HasPrefix(name, "x-amz-") {
							t.Fatalf("the plugin does not sign gateway requests itself: header %s", name)
						}
					}
					got := resp.GetSigning()
					if (got == nil) != (tc.want == nil) {
						t.Fatalf("signing = %v, want %v", got != nil, tc.want != nil)
					}
					if got == nil {
						return
					}
					if got.GetScheme() != tc.want.GetScheme() || got.GetRegion() != tc.want.GetRegion() || got.GetService() != tc.want.GetService() ||
						got.GetAccessKeyId() != tc.want.GetAccessKeyId() || got.GetSecretAccessKey() != tc.want.GetSecretAccessKey() || got.GetSessionToken() != tc.want.GetSessionToken() {
						t.Fatalf("signing = scheme %s region %s service %s key %s, session token set: %v",
							got.GetScheme(), got.GetRegion(), got.GetService(), got.GetAccessKeyId(), got.GetSessionToken() != "")
					}
					// The scheme is one the core's signer knows.
					if !reqsign.Known(got.GetScheme()) || got.GetScheme() != reqsign.SchemeAWSSigV4 || got.GetService() != SigningService {
						t.Fatalf("scheme %q / service %q", got.GetScheme(), got.GetService())
					}
				})
			}
		}
	}
}

func TestBuildFromBrokenAccount(t *testing.T) {
	p := newTestPlugin(Endpoints{})
	for name, tc := range map[string]struct {
		acc  *pluginv1.Account
		want codes.Code
	}{
		"credentials not JSON": {&pluginv1.Account{Type: TypeAPIKey, CredentialsJson: "{"}, codes.InvalidArgument},
		"settings not JSON":    {&pluginv1.Account{Type: TypeAPIKey, CredentialsJson: mustJSON(apiKeyCreds), SettingsJson: "[1]"}, codes.InvalidArgument},
		"no credentials":       {&pluginv1.Account{Type: TypeAccessKey}, codes.FailedPrecondition},
		"another plugin's":     {stored("apikey", apiKeyCreds, nil), codes.FailedPrecondition},
	} {
		if _, err := p.BuildUpstreamRequest(context.Background(), buildRequest(tc.acc, ProtocolMessages, testHaiku, false)); status.Code(err) != tc.want {
			t.Errorf("%s: BuildUpstreamRequest: %v", name, err)
		}
		if _, err := p.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{Account: tc.acc}); status.Code(err) != tc.want {
			t.Errorf("%s: BuildTestRequest: %v", name, err)
		}
	}
}

// received is the test request as an upstream receives it from the host,
// which sends the described request unchanged (account/testreq.go).
func received(t *testing.T, tr *pluginv1.BuildTestRequestResponse) *http.Request {
	t.Helper()
	req, err := http.NewRequest(tr.GetMethod(), tr.GetUrl(), strings.NewReader(tr.GetBodyJson()))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range tr.GetHeaders() {
		req.Header.Set(k, v)
	}
	req.Host = req.URL.Host
	return req
}

func TestBuildTestRequest(t *testing.T) {
	for _, tc := range []struct {
		name           string
		typ            string
		region, prefix string
		model          string
		wantURL        string
		wantModel      string
	}{
		{"api key, default model", TypeAPIKey, "", "", "", useast1 + "/model/" + haikuSegment + "/invoke", testHaiku},
		{"api key, prefix", TypeAPIKey, "eu-west-1", "eu", "", "https://bedrock-runtime.eu-west-1.amazonaws.com/model/eu." + haikuSegment + "/invoke", "eu." + testHaiku},
		{"access key, default model", TypeAccessKey, "", "", "", useast1 + "/model/" + haikuSegment + "/invoke", testHaiku},
		{"access key, prefix and model", TypeAccessKey, "us-west-2", "us", "anthropic.claude-sonnet-4-5-20250929-v1:0",
			"https://bedrock-runtime.us-west-2.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"},
		{"access key, ARN", TypeAccessKey, "us-east-1", "us", testARN, useast1 + "/model/" + arnSegment + "/invoke", testARN},
		{"temporary credentials, default model", TypeSTS, "ap-southeast-2", "au", "",
			"https://bedrock-runtime.ap-southeast-2.amazonaws.com/model/au." + haikuSegment + "/invoke", "au." + testHaiku},
		{"temporary credentials, ARN", TypeSTS, "us-east-1", "", testARN, useast1 + "/model/" + arnSegment + "/invoke", testARN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := newTestPlugin(Endpoints{}).BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{
				Account: stored(tc.typ, typeCreds[tc.typ], settingsOf(tc.region, tc.prefix)), Model: tc.model,
			})
			if err != nil {
				t.Fatalf("BuildTestRequest: %v", err)
			}
			if tr.GetMethod() != http.MethodPost || tr.GetUrl() != tc.wantURL || tr.GetModel() != tc.wantModel || tr.GetUsageProtocol() != "anthropic.messages" {
				t.Fatalf("request = %s %s (model %s, usage %s)", tr.GetMethod(), tr.GetUrl(), tr.GetModel(), tr.GetUsageProtocol())
			}
			// The Messages ping without its model, with Bedrock's version.
			body := gjson.Parse(tr.GetBodyJson())
			if !gjson.Valid(tr.GetBodyJson()) || body.Get("model").Exists() || body.Get("stream").Exists() || body.Get("anthropic_version").String() != AnthropicVersion ||
				body.Get("max_tokens").Int() != 1 || body.Get("messages.0.role").String() != "user" || body.Get("messages.0.content").String() != "ping" {
				t.Fatalf("body = %s", tr.GetBodyJson())
			}
			h := tr.GetHeaders()
			if h["content-type"] != "application/json" || h["accept"] != "application/json" {
				t.Fatalf("headers lack content-type / accept")
			}
			if tc.typ == TypeAPIKey {
				if h["authorization"] != "Bearer "+testAPIKey || len(h) != 3 {
					t.Fatalf("an API key request has %d headers", len(h))
				}
				return
			}

			// The signature is recomputed from the request as received.
			req := received(t, tr)
			sig, err := verifySigV4(req, []byte(tr.GetBodyJson()), testNow)
			if err != nil {
				t.Fatalf("signature: %v", err)
			}
			region := tc.region
			if region == "" {
				region = DefaultRegion
			}
			wantSigned, wantKey := "content-type;host;x-amz-date", testAccessKeyID
			if tc.typ == TypeSTS {
				wantSigned, wantKey = "content-type;host;x-amz-date;x-amz-security-token", testSTSKeyID
			}
			if sig.AccessKeyID != wantKey || sig.Region != region || sig.Service != "bedrock" || sig.SignedHeaders != wantSigned {
				t.Fatalf("signature = %+v", sig)
			}
			if h["x-amz-date"] != testNow.UTC().Format("20060102T150405Z") || (h["x-amz-security-token"] == testSessionToken) != (tc.typ == TypeSTS) {
				t.Fatalf("x-amz-date = %s, session token header set = %v", h["x-amz-date"], h["x-amz-security-token"] != "")
			}

			// The signature covers the body, the path and the session token.
			tampered := received(t, tr)
			if _, err := verifySigV4(tampered, []byte(tr.GetBodyJson()+" "), testNow); err == nil {
				t.Fatal("a changed body still verifies")
			}
			tampered = received(t, tr)
			tampered.URL.RawPath = strings.Replace(tampered.URL.EscapedPath(), "/invoke", "/invoke-with-response-stream", 1)
			tampered.URL.Path = strings.Replace(tampered.URL.Path, "/invoke", "/invoke-with-response-stream", 1)
			if _, err := verifySigV4(tampered, []byte(tr.GetBodyJson()), testNow); err == nil {
				t.Fatal("a changed path still verifies")
			}
			if tc.typ == TypeSTS {
				tampered = received(t, tr)
				tampered.Header.Set("X-Amz-Security-Token", testSessionToken+"x")
				if _, err := verifySigV4(tampered, []byte(tr.GetBodyJson()), testNow); err == nil {
					t.Fatal("a changed session token still verifies")
				}
			}
		})
	}
}

// A secret that is not the access key's fails the independent check: the
// test above proves more than "some signature is present".
func TestBuildTestRequestWrongSecret(t *testing.T) {
	tr, err := newTestPlugin(Endpoints{}).BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{
		Account: stored(TypeAccessKey, with(akskCreds, map[string]any{fieldSecretAccessKey: "another-secret"}), nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifySigV4(received(t, tr), []byte(tr.GetBodyJson()), testNow); err == nil {
		t.Fatal("a signature made with another secret verifies")
	}
}

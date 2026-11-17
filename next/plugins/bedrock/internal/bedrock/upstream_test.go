package bedrock

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
		{"base id with a colon", "us-east-1", NoProfilePrefix, testHaiku, false, useast1 + "/model/" + haikuSegment + "/invoke", testHaiku},
		{"base id, streamed", "us-east-1", NoProfilePrefix, testHaiku, true, useast1 + "/model/" + haikuSegment + "/invoke-with-response-stream", testHaiku},
		{"base id without a colon", "us-east-1", NoProfilePrefix, "anthropic.claude-sonnet-4-6", false, useast1 + "/model/anthropic.claude-sonnet-4-6/invoke", "anthropic.claude-sonnet-4-6"},
		{"no region means us-east-1", "", NoProfilePrefix, testHaiku, false, useast1 + "/model/" + haikuSegment + "/invoke", testHaiku},
		{"another region", "ap-northeast-1", NoProfilePrefix, testHaiku, true,
			"https://bedrock-runtime.ap-northeast-1.amazonaws.com/model/" + haikuSegment + "/invoke-with-response-stream", testHaiku},
		{"govcloud region", "us-gov-west-1", NoProfilePrefix, testHaiku, false,
			"https://bedrock-runtime.us-gov-west-1.amazonaws.com/model/" + haikuSegment + "/invoke", testHaiku},
		{"inference profile id", "us-east-1", NoProfilePrefix, "us." + testHaiku, false, useast1 + "/model/us." + haikuSegment + "/invoke", "us." + testHaiku},
		{"no prefix choice means global", "us-east-1", "", testHaiku, false, useast1 + "/model/global." + haikuSegment + "/invoke", "global." + testHaiku},
		{"no prefix choice keeps an id that has one", "us-east-1", "", "us.anthropic.claude-opus-4-1-20250805-v1:0", false,
			useast1 + "/model/us.anthropic.claude-opus-4-1-20250805-v1%3A0/invoke", "us.anthropic.claude-opus-4-1-20250805-v1:0"},
		{"prefix none keeps a global id", "us-east-1", NoProfilePrefix, "global." + testHaiku, false, useast1 + "/model/global." + haikuSegment + "/invoke", "global." + testHaiku},
		{"prefix setting us", "us-east-1", "us", testHaiku, false, useast1 + "/model/us." + haikuSegment + "/invoke", "us." + testHaiku},
		{"prefix setting eu, streamed", "eu-central-1", "eu", testHaiku, true,
			"https://bedrock-runtime.eu-central-1.amazonaws.com/model/eu." + haikuSegment + "/invoke-with-response-stream", "eu." + testHaiku},
		{"prefix setting global", "us-east-1", "global", "anthropic.claude-sonnet-4-6", false,
			useast1 + "/model/global.anthropic.claude-sonnet-4-6/invoke", "global.anthropic.claude-sonnet-4-6"},
		{"prefix setting keeps an id that has one", "us-east-1", "eu", "us." + testHaiku, false, useast1 + "/model/us." + haikuSegment + "/invoke", "us." + testHaiku},
		{"prefix setting in", "ap-south-1", "in", "anthropic.claude-sonnet-5", false,
			"https://bedrock-runtime.ap-south-1.amazonaws.com/model/in.anthropic.claude-sonnet-5/invoke", "in.anthropic.claude-sonnet-5"},
		{"prefix setting keeps an in. id", "us-east-1", "us", "in.anthropic.claude-sonnet-5", false,
			useast1 + "/model/in.anthropic.claude-sonnet-5/invoke", "in.anthropic.claude-sonnet-5"},
		{"prefix setting keeps a us-gov id", "us-gov-west-1", "us", "us-gov." + testHaiku, false,
			"https://bedrock-runtime.us-gov-west-1.amazonaws.com/model/us-gov." + haikuSegment + "/invoke", "us-gov." + testHaiku},
		{"ARN with a slash", "us-east-1", NoProfilePrefix, testARN, false, useast1 + "/model/" + arnSegment + "/invoke", testARN},
		{"ARN, streamed", "us-east-1", NoProfilePrefix, testARN, true, useast1 + "/model/" + arnSegment + "/invoke-with-response-stream", testARN},
		{"prefix setting keeps an ARN", "us-east-1", "us", testARN, false, useast1 + "/model/" + arnSegment + "/invoke", testARN},
		{"id is one segment whatever it holds", "us-east-1", NoProfilePrefix, "my model/v1?x#y", false, useast1 + "/model/my%20model%2Fv1%3Fx%23y/invoke", "my model/v1?x#y"},
		{"surrounding space trimmed", "us-east-1", "apac", "  " + testHaiku + " ", false, useast1 + "/model/apac." + haikuSegment + "/invoke", "apac." + testHaiku},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := build(t, buildRequest(stored(TypeAPIKey, apiKeyCreds, settingsOf(tc.region, tc.prefix)), ProtocolMessages, tc.model, tc.stream))
			if resp.GetMethod() != http.MethodPost || resp.GetUrl() != tc.wantURL || resp.GetUpstreamModel() != tc.wantModel {
				t.Fatalf("request = %s %s (model %s)\nwant POST %s (model %s)", resp.GetMethod(), resp.GetUrl(), resp.GetUpstreamModel(), tc.wantURL, tc.wantModel)
			}
			wantStream := ""
			if tc.stream {
				wantStream = "aws-eventstream-chunk"
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

// Only the messages endpoint is served. The OpenAI protocols are not: no
// type declares the openai platform, so the core converts such a request
// into a Messages one instead of asking for an OpenAI route.
func TestUnsupportedProtocol(t *testing.T) {
	for _, protocol := range []string{"", "anthropic.count_tokens", "openai.chat", "openai.responses", "openai.embeddings", "openai.responses_ws", "gemini.generate"} {
		_, err := newTestPlugin(Endpoints{}).BuildUpstreamRequest(context.Background(),
			buildRequest(stored(TypeAPIKey, apiKeyCreds, nil), protocol, testHaiku, false))
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v", protocol, err)
		}
	}
}

// The InvokeModel body: the path carries model and stream, so both leave the
// body; anthropic_version is always set, and the top-level fields InvokeModel
// has no use for leave. The beta features of the header and of the body's
// own anthropic_beta are settled together.
func TestInvokeModelPatches(t *testing.T) {
	const noContextManagement = "delete context_management"
	base := []string{"delete model", "delete stream", `set anthropic_version="bedrock-2023-05-31"`, "delete metadata", "delete provider"}
	for _, tc := range []struct {
		name string
		// header and body are the anthropic-beta header and the body's
		// anthropic_beta as raw JSON; absent tells that the client sent none.
		header, body string
		absent       bool
		want         []string
	}{
		// Neither the header nor the body.
		{name: "no beta header", absent: true, want: []string{noContextManagement}},
		{name: "empty beta header", header: "  ", want: []string{noContextManagement}},

		// The header only.
		{name: "supported feature", header: "context-1m-2025-08-07", want: []string{`set anthropic_beta=["context-1m-2025-08-07"]`, noContextManagement}},
		{name: "several, spaced", header: " context-1m-2025-08-07 ,computer-use-2025-01-24,  compact-2026-01-12",
			want: []string{`set anthropic_beta=["context-1m-2025-08-07","computer-use-2025-01-24","compact-2026-01-12"]`, noContextManagement}},
		{name: "unsupported ones dropped; context management keeps its field", header: "claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,oauth-2025-04-20",
			want: []string{`set anthropic_beta=["context-management-2025-06-27"]`}},
		{name: "only unsupported ones", header: "claude-code-20250219,oauth-2025-04-20,prompt-caching-2024-07-31", want: []string{noContextManagement}},
		{name: "duplicates once", header: "context-1m-2025-08-07,context-1m-2025-08-07,fine-grained-tool-streaming-2025-05-14",
			want: []string{`set anthropic_beta=["context-1m-2025-08-07","fine-grained-tool-streaming-2025-05-14"]`, noContextManagement}},
		{name: "advanced tool use is tool search on Bedrock, with tool examples", header: "advanced-tool-use-2025-11-20",
			want: []string{`set anthropic_beta=["tool-search-tool-2025-10-19","tool-examples-2025-10-29"]`, noContextManagement}},
		{name: "tool search brings tool examples", header: "tool-search-tool-2025-10-19,computer-use-2025-11-24",
			want: []string{`set anthropic_beta=["tool-search-tool-2025-10-19","computer-use-2025-11-24","tool-examples-2025-10-29"]`, noContextManagement}},
		{name: "tool examples not repeated", header: "tool-examples-2025-10-29,advanced-tool-use-2025-11-20,tool-search-tool-2025-10-19",
			want: []string{`set anthropic_beta=["tool-examples-2025-10-29","tool-search-tool-2025-10-19"]`, noContextManagement}},

		// The body only: filtered like the header, never sent on as it came.
		{name: "features of the AWS model cards", header: "compact-2026-09-04,fallback-credit-2026-07-01,thinking-display-updates-2026-08-18,per-turn-control-2026-07-01",
			want: []string{`set anthropic_beta=["compact-2026-09-04","fallback-credit-2026-07-01","thinking-display-updates-2026-08-18","per-turn-control-2026-07-01"]`, noContextManagement}},
		{name: "body, supported", absent: true, body: `["context-1m-2025-08-07"]`, want: []string{`set anthropic_beta=["context-1m-2025-08-07"]`, noContextManagement}},
		{name: "body, unsupported ones dropped", absent: true, body: `["claude-code-20250219","compact-2026-01-12","advanced-tool-use-2025-11-20"]`,
			want: []string{`set anthropic_beta=["compact-2026-01-12","tool-search-tool-2025-10-19","tool-examples-2025-10-29"]`, noContextManagement}},
		{name: "body, only unsupported ones: the field leaves", absent: true, body: `["claude-code-20250219"]`, want: []string{"delete anthropic_beta", noContextManagement}},
		{name: "body, empty array: the field leaves", absent: true, body: `[]`, want: []string{"delete anthropic_beta", noContextManagement}},
		{name: "body, one string in the header's format", absent: true, body: `"context-1m-2025-08-07, compact-2026-01-12"`,
			want: []string{`set anthropic_beta=["context-1m-2025-08-07","compact-2026-01-12"]`, noContextManagement}},
		{name: "body, not a list of names: the field leaves", absent: true, body: `{"context-1m-2025-08-07":true}`, want: []string{"delete anthropic_beta", noContextManagement}},
		{name: "body, names among other values", absent: true, body: `[1,"context-management-2025-06-27",null]`, want: []string{`set anthropic_beta=["context-management-2025-06-27"]`}},

		// Both: the header's features first, then the body's, each once.
		{name: "header and body merged", header: "context-1m-2025-08-07,claude-code-20250219", body: `["compact-2026-01-12","context-1m-2025-08-07"]`,
			want: []string{`set anthropic_beta=["context-1m-2025-08-07","compact-2026-01-12"]`, noContextManagement}},
		{name: "header and body, nothing usable in either", header: "oauth-2025-04-20", body: `["claude-code-20250219"]`, want: []string{"delete anthropic_beta", noContextManagement}},
		{name: "header unusable, body usable", header: "oauth-2025-04-20", body: `["context-management-2025-06-27"]`, want: []string{`set anthropic_beta=["context-management-2025-06-27"]`}},
		{name: "empty header does not hide the body", header: "", body: `["computer-use-2025-11-24"]`, want: []string{`set anthropic_beta=["computer-use-2025-11-24"]`, noContextManagement}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				in := buildRequest(stored(TypeAccessKey, akskCreds, nil), ProtocolMessages, testHaiku, stream)
				if !tc.absent {
					in.InboundHeaders = map[string]string{"anthropic-beta": tc.header, "user-agent": "claude-cli/2.1.90"}
				}
				if tc.body != "" {
					in.Fields[fieldBetas] = tc.body
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

// The patches applied the way the core applies them (sjson): removing a
// field the body does not have changes nothing, output_config stays, and
// the fallbacks the core guards are never touched.
func TestInvokeModelBody(t *testing.T) {
	for _, tc := range []struct {
		name, header, body, want string
	}{
		{"a plain request", "",
			`{"model":"claude-haiku-4-5","max_tokens":8,"messages":[]}`,
			`{"max_tokens":8,"messages":[],"anthropic_version":"bedrock-2023-05-31"}`},
		{"what Claude Code sends Anthropic", "claude-code-20250219,context-1m-2025-08-07",
			`{"model":"claude-haiku-4-5","stream":true,"max_tokens":8,"messages":[],"metadata":{"user_id":"u"},"provider":{"only":["x"]},` +
				`"context_management":{"edits":[]},"output_config":{"effort":"high"},"fallbacks":[{"model":"claude-opus-4-8"}],"anthropic_beta":["oauth-2025-04-20"]}`,
			`{"max_tokens":8,"messages":[],"output_config":{"effort":"high"},"fallbacks":[{"model":"claude-opus-4-8"}],"anthropic_beta":["context-1m-2025-08-07"],"anthropic_version":"bedrock-2023-05-31"}`},
		{"context management with its feature", "context-management-2025-06-27",
			`{"model":"claude-haiku-4-5","max_tokens":8,"messages":[],"context_management":{"edits":[]}}`,
			`{"max_tokens":8,"messages":[],"context_management":{"edits":[]},"anthropic_version":"bedrock-2023-05-31","anthropic_beta":["context-management-2025-06-27"]}`},
		{"a body feature nobody supports", "",
			`{"model":"claude-haiku-4-5","max_tokens":8,"anthropic_beta":["claude-code-20250219"],"messages":[]}`,
			`{"max_tokens":8,"messages":[],"anthropic_version":"bedrock-2023-05-31"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := buildRequest(stored(TypeAPIKey, apiKeyCreds, nil), ProtocolMessages, testHaiku, false)
			in.InboundHeaders = map[string]string{"anthropic-beta": tc.header}
			if betas := gjson.Get(tc.body, fieldBetas); betas.Exists() {
				in.Fields[fieldBetas] = betas.Raw
			}
			body := []byte(tc.body)
			var err error
			for _, p := range build(t, in).GetPatches() {
				if p.GetOp() == pluginv1.BodyPatch_OP_DELETE {
					body, err = sjson.DeleteBytes(body, p.GetPath())
				} else {
					body, err = sjson.SetRawBytes(body, p.GetPath(), []byte(p.GetValueJson()))
				}
				if err != nil {
					t.Fatalf("patch %s: %v", p.GetPath(), err)
				}
			}
			if string(body) != tc.want {
				t.Fatalf("body = %s\nwant   %s", body, tc.want)
			}
		})
	}
}

// Every beta feature InvokeModel accepts passes, in the list's own order,
// from the header as from the body.
func TestSupportedBetasPass(t *testing.T) {
	if got := bedrockBetas(headerBetas(strings.Join(supportedBetas, ","))...); !slices.Equal(got, supportedBetas) {
		t.Fatalf("betas = %v", got)
	}
	if got := bedrockBetas(bodyBetas(mustJSON(supportedBetas))...); !slices.Equal(got, supportedBetas) {
		t.Fatalf("betas = %v", got)
	}
	if len(supportedBetas) != 15 || !slices.Contains(supportedBetas, betaAliases["advanced-tool-use-2025-11-20"]) || !slices.Contains(supportedBetas, betaContextManagement) {
		t.Fatalf("supported = %v, aliases = %v", supportedBetas, betaAliases)
	}
}

// An id that already names its geography keeps it whatever the account's
// prefix: the prefixes an account can choose, and us-gov.
func TestRegionalPrefixes(t *testing.T) {
	want := []string{"us-gov."}
	for _, prefix := range ProfilePrefixes[1:] {
		want = append(want, prefix+".")
	}
	if ProfilePrefixes[0] != NoProfilePrefix || !slices.Equal(regionalPrefixes, want) || len(regionalPrefixes) != len(ProfilePrefixes) {
		t.Fatalf("regional prefixes = %v, profile prefixes = %v", regionalPrefixes, ProfilePrefixes)
	}
	for _, prefix := range regionalPrefixes {
		if !hasRegionalPrefix(prefix+testHaiku) || hasRegionalPrefix(strings.TrimSuffix(prefix, ".")+testHaiku) {
			t.Errorf("prefix %q is not recognized as one", prefix)
		}
	}
	if hasRegionalPrefix(testHaiku) || hasRegionalPrefix(NoProfilePrefix+"."+testHaiku) {
		t.Error("a plain id counts as an inference profile id")
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
		for _, stream := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				resp := build(t, buildRequest(stored(tc.typ, tc.creds, settingsOf(tc.region, "")), ProtocolMessages, testHaiku, stream))
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
		// The default model is a global inference profile id (Haiku 4.5 has
		// no bare id on bedrock-runtime): sent as it is whatever the prefix.
		{"api key, default model", TypeAPIKey, "", "", "", useast1 + "/model/global." + haikuSegment + "/invoke", DefaultTestModel},
		{"api key, default model, prefix none", TypeAPIKey, "", NoProfilePrefix, "", useast1 + "/model/global." + haikuSegment + "/invoke", DefaultTestModel},
		{"api key, default model, prefix global", TypeAPIKey, "", "global", "", useast1 + "/model/global." + haikuSegment + "/invoke", DefaultTestModel},
		{"api key, default model, prefix us", TypeAPIKey, "", "us", "", useast1 + "/model/global." + haikuSegment + "/invoke", DefaultTestModel},
		{"api key, prefix and a plain model", TypeAPIKey, "eu-west-1", "eu", testHaiku, "https://bedrock-runtime.eu-west-1.amazonaws.com/model/eu." + haikuSegment + "/invoke", "eu." + testHaiku},
		{"api key, prefix none and a plain model", TypeAPIKey, "eu-west-2", NoProfilePrefix, "anthropic.claude-sonnet-5", "https://bedrock-runtime.eu-west-2.amazonaws.com/model/anthropic.claude-sonnet-5/invoke", "anthropic.claude-sonnet-5"},
		{"access key, default model", TypeAccessKey, "", "", "", useast1 + "/model/global." + haikuSegment + "/invoke", DefaultTestModel},
		{"access key, prefix and model", TypeAccessKey, "us-west-2", "us", "anthropic.claude-sonnet-4-5-20250929-v1:0",
			"https://bedrock-runtime.us-west-2.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"},
		{"access key, ARN", TypeAccessKey, "us-east-1", "us", testARN, useast1 + "/model/" + arnSegment + "/invoke", testARN},
		{"temporary credentials, default model", TypeSTS, "ap-southeast-2", "au", "",
			"https://bedrock-runtime.ap-southeast-2.amazonaws.com/model/global." + haikuSegment + "/invoke", DefaultTestModel},
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

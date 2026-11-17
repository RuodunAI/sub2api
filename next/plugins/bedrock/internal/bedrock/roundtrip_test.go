package bedrock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/Sub2API-Devs/sup2api/next/sdk/awseventstream"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/reqsign"
)

// fakeBedrock is a Bedrock runtime: InvokeModel for Claude and the
// OpenAI-compatible endpoints. It accepts the test API key and SigV4
// requests whose signature it recomputes itself (verifySigV4), and refuses
// what the real service refuses: a body with "model" or "stream", or without
// anthropic_version.
type fakeBedrock struct {
	srv *httptest.Server
	// expired makes the fake treat every session token as expired.
	expired bool

	mu     sync.Mutex
	served []servedRequest
}

// servedRequest is one request the fake answered with 200.
type servedRequest struct {
	// Path is the path as it came over the wire, still escaped.
	Path string
	// Model is the model the request invoked.
	Model string
	// Signed tells how it was authorized.
	Signed bool
	Body   string
}

func newFakeBedrock(t *testing.T) *fakeBedrock {
	f := &fakeBedrock{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /model/{model}/invoke", f.invoke)
	mux.HandleFunc("POST /model/{model}/invoke-with-response-stream", f.invoke)
	mux.HandleFunc("POST /openai/v1/chat/completions", f.openAI)
	mux.HandleFunc("POST /openai/v1/responses", f.openAI)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBedrock) requests() []servedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.served)
}

func (f *fakeBedrock) record(r *http.Request, model string, signed bool, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, servedRequest{Path: r.URL.EscapedPath(), Model: model, Signed: signed, Body: string(body)})
}

func awsFailure(w http.ResponseWriter, status int, typ, message string) {
	w.Header().Set("x-amzn-errortype", typ+":http://internal.amazon.com/coral/com.amazon.bedrock/")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(mustJSON(map[string]string{"message": message})))
}

// authorize checks the credential of r. It answers the failure itself and
// reports whether the request may go on, and whether it was signed.
func (f *fakeBedrock) authorize(w http.ResponseWriter, r *http.Request, body []byte) (ok, signed bool) {
	auth := r.Header.Get("Authorization")
	if auth == "Bearer "+testAPIKey {
		return true, false
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		awsFailure(w, http.StatusForbidden, "AccessDeniedException", "Invalid API Key format: Must start with pre-defined prefix")
		return false, false
	}
	sig, err := verifySigV4(r, body, testNow)
	switch token := r.Header.Get("X-Amz-Security-Token"); {
	case err != nil:
		awsFailure(w, http.StatusForbidden, "InvalidSignatureException", err.Error())
	case sig.Service != SigningService || sig.Region != "us-west-2":
		awsFailure(w, http.StatusForbidden, "InvalidSignatureException", "Credential should be scoped to a valid region and service.")
	case sig.AccessKeyID == testSTSKeyID && token != testSessionToken, sig.AccessKeyID == testAccessKeyID && token != "":
		awsFailure(w, http.StatusForbidden, "UnrecognizedClientException", "The security token included in the request is invalid.")
	case token != "" && f.expired:
		awsFailure(w, http.StatusForbidden, "ExpiredTokenException", "The security token included in the request is expired")
	default:
		return true, true
	}
	return false, false
}

// claudeEvents are the events of a streamed Claude answer.
var claudeEvents = []string{
	`{"type":"message_start","message":{"id":"msg_bdrk_01","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[],"usage":{"input_tokens":9,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
	`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":9,"outputTokenCount":3,"invocationLatency":100,"firstByteLatency":10}}`,
}

func (f *fakeBedrock) invoke(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ok, signed := f.authorize(w, r, body)
	if !ok {
		return
	}
	fields := gjson.ParseBytes(body)
	switch {
	case !gjson.ValidBytes(body):
		awsFailure(w, http.StatusBadRequest, "ValidationException", "Malformed input request")
		return
	case fields.Get("model").Exists(), fields.Get("stream").Exists():
		awsFailure(w, http.StatusBadRequest, "ValidationException", "Malformed input request: #: extraneous key is not permitted")
		return
	case fields.Get("anthropic_version").String() != AnthropicVersion:
		awsFailure(w, http.StatusBadRequest, "ValidationException", "anthropic_version must be "+AnthropicVersion)
		return
	case r.Header.Get("Anthropic-Beta") != "":
		awsFailure(w, http.StatusBadRequest, "ValidationException", "beta features go into anthropic_beta")
		return
	case strings.HasPrefix(r.PathValue("model"), "anthropic.claude-opus"):
		awsFailure(w, http.StatusForbidden, "AccessDeniedException", "You don't have access to the model with the specified model ID.")
		return
	}
	f.record(r, r.PathValue("model"), signed, body)
	if !strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_bdrk_01","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`)
		return
	}
	w.Header().Set("Content-Type", awseventstream.ContentType)
	headers := []awseventstream.Header{
		{Name: awseventstream.HeaderEventType, Value: "chunk"},
		{Name: awseventstream.HeaderContentType, Value: "application/json"},
		{Name: awseventstream.HeaderMessageType, Value: awseventstream.MessageTypeEvent},
	}
	for _, event := range claudeEvents {
		payload := mustJSON(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
		frame, err := awseventstream.AppendMessage(nil, headers, []byte(payload))
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(frame)
	}
}

func (f *fakeBedrock) openAI(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ok, signed := f.authorize(w, r, body)
	if !ok {
		return
	}
	fields := gjson.ParseBytes(body)
	model := fields.Get("model").String()
	if model == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"you must provide a model parameter","type":"invalid_request_error","code":null}}`)
		return
	}
	f.record(r, model, signed, body)
	usage := `"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}`
	if !fields.Get("stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],`+usage+`}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"id":"chatcmpl-1","model":"`+model+`","choices":[{"index":0,"delta":{"content":"pong"}}]}`+"\n\n")
	// Like OpenAI, usage is only streamed when the request asks for it.
	if fields.Get("stream_options.include_usage").Bool() {
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-1","model":"`+model+`","choices":[],`+usage+`}`+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// ---------------------------------------------------------------- the core's part

// answer is an upstream answer as the core relays it.
type answer struct {
	Status      int
	ContentType string
	Headers     map[string]string
	Body        []byte
}

// coreForward does the core's part of an attempt (gateway forwardBuilt):
// apply the patches to the client's body, write the plugin's headers, sign
// the final request when the plugin asked for it, send it, and decode the
// answer's framing into SSE when the plugin named one.
func coreForward(t *testing.T, built *pluginv1.BuildUpstreamRequestResponse, clientBody string) answer {
	t.Helper()
	body := []byte(clientBody)
	var err error
	for _, p := range built.GetPatches() {
		switch p.GetOp() {
		case pluginv1.BodyPatch_OP_DELETE:
			body, err = sjson.DeleteBytes(body, p.GetPath())
		case pluginv1.BodyPatch_OP_SET:
			body, err = sjson.SetRawBytes(body, p.GetPath(), []byte(p.GetValueJson()))
		default:
			t.Fatalf("patch %s has no op", p.GetPath())
		}
		if err != nil {
			t.Fatalf("patch %s: %v", p.GetPath(), err)
		}
	}
	req, err := http.NewRequest(built.GetMethod(), built.GetUrl(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range built.GetHeaders() {
		req.Header.Set(k, v)
	}
	if s := built.GetSigning(); s != nil {
		err := reqsign.Sign(req, body, reqsign.Directive{Scheme: s.GetScheme(), Region: s.GetRegion(), Service: s.GetService(),
			AccessKeyID: s.GetAccessKeyId(), SecretAccessKey: s.GetSecretAccessKey(), SessionToken: s.GetSessionToken()}, testNow)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	a := answer{Status: resp.StatusCode, Headers: map[string]string{}}
	for name := range resp.Header {
		a.Headers[strings.ToLower(name)] = resp.Header.Get(name)
	}
	a.ContentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var stream io.Reader = resp.Body
	if built.GetResponseStream() == ResponseStream && resp.StatusCode/100 == 2 && a.ContentType == awseventstream.ContentType {
		stream, a.ContentType = awseventstream.NewChunkSSEReader(resp.Body), "text/event-stream"
	}
	if a.Body, err = io.ReadAll(stream); err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return a
}

// sseEvents are the "event:" names of an SSE body, in order.
func sseEvents(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			out = append(out, name)
		}
	}
	return out
}

// startPlugin runs the plugin in the SDK harness (over gRPC, as the core
// calls it) against the fake Bedrock.
func startPlugin(t *testing.T, f *fakeBedrock) *pluginsdktest.Harness {
	t.Helper()
	return pluginsdktest.Start(t, newTestPlugin(Endpoints{API: f.srv.URL}), pluginsdktest.Options{SDK: []pluginsdk.Option{pluginsdk.WithInfo("bedrock", Version)}})
}

// save runs a submitted form through ValidateCredentials and returns the
// account the core would store.
func save(t *testing.T, h *pluginsdktest.Harness, typ string, creds, settings map[string]any) *pluginv1.Account {
	t.Helper()
	resp, err := h.Platform.ValidateCredentials(context.Background(), &pluginv1.ValidateCredentialsRequest{
		AccountType: typ, CredentialsJson: mustJSON(creds), SettingsJson: mustJSON(settings),
	})
	if err != nil || len(resp.GetErrors()) > 0 {
		t.Fatalf("save: %v %v", fieldErrors(resp), err)
	}
	return stored(typ, normalized(t, resp.GetNormalizedCredentialsJson(), creds), normalized(t, resp.GetNormalizedSettingsJson(), settings))
}

// forward is one gateway request for the account: the plugin describes it,
// the core's part sends it.
func forward(t *testing.T, h *pluginsdktest.Harness, in *pluginv1.BuildUpstreamRequestRequest, clientBody string) answer {
	t.Helper()
	built, err := h.Platform.BuildUpstreamRequest(context.Background(), in)
	if err != nil {
		t.Fatalf("BuildUpstreamRequest: %v", err)
	}
	return coreForward(t, built, clientBody)
}

// classifyAnswer hands a failed answer to the plugin, as the core does.
func classifyAnswer(t *testing.T, h *pluginsdktest.Harness, acc *pluginv1.Account, protocol string, a answer) *pluginv1.ClassifyErrorResponse {
	t.Helper()
	cls, err := h.Platform.ClassifyError(context.Background(), &pluginv1.ClassifyErrorRequest{
		Meta: &pluginv1.RequestMeta{Protocol: protocol}, Account: acc, Status: int32(a.Status), Headers: a.Headers, BodyPrefix: a.Body,
	})
	if err != nil {
		t.Fatalf("ClassifyError: %v", err)
	}
	return cls
}

const (
	messagesBody = `{"model":"claude-haiku-4-5","max_tokens":64,"stream":%s,"messages":[{"role":"user","content":"ping"}]}`
	chatBody     = `{"model":"openai.gpt-oss-120b-1:0","stream":%s,"messages":[{"role":"user","content":"ping"}]}`
)

func withStream(body string, stream bool) string {
	return strings.Replace(body, "%s", map[bool]string{false: "false", true: "true"}[stream], 1)
}

// TestRoundTrip follows an account of every type from the form to answered
// requests on every route: InvokeModel plain and streamed (the event stream
// arrives as SSE), an ARN, and the OpenAI-compatible endpoints.
func TestRoundTrip(t *testing.T) {
	for typ, creds := range typeCreds {
		t.Run(typ, func(t *testing.T) {
			f := newFakeBedrock(t)
			h := startPlugin(t, f)
			acc := save(t, h, typ, creds, settingsOf("us-west-2", "us"))
			signed := typ != TypeAPIKey

			// InvokeModel.
			in := buildRequest(acc, ProtocolMessages, testHaiku, false)
			in.InboundHeaders = map[string]string{"anthropic-beta": "claude-code-20250219,context-1m-2025-08-07"}
			a := forward(t, h, in, withStream(messagesBody, false))
			if a.Status != http.StatusOK || a.ContentType != "application/json" || gjson.GetBytes(a.Body, "usage.output_tokens").Int() != 3 {
				t.Fatalf("invoke: %d %s %s", a.Status, a.ContentType, a.Body)
			}

			// InvokeModelWithResponseStream: the frames come out as SSE with
			// the usage the anthropic rules read.
			a = forward(t, h, buildRequest(acc, ProtocolMessages, testHaiku, true), withStream(messagesBody, true))
			wantEvents := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
			if a.Status != http.StatusOK || a.ContentType != "text/event-stream" || !slices.Equal(sseEvents(a.Body), wantEvents) ||
				!bytes.Contains(a.Body, []byte(`"usage":{"output_tokens":3}`)) {
				t.Fatalf("invoke-with-response-stream: %d %s\n%s", a.Status, a.ContentType, a.Body)
			}

			// An ARN: one path segment on the wire, signed as sent.
			a = forward(t, h, buildRequest(acc, ProtocolMessages, testARN, false), withStream(messagesBody, false))
			if a.Status != http.StatusOK {
				t.Fatalf("ARN: %d %s", a.Status, a.Body)
			}

			// The OpenAI-compatible endpoints.
			a = forward(t, h, buildRequest(acc, ProtocolChat, "openai.gpt-oss-120b-1:0", true), withStream(chatBody, true))
			if a.Status != http.StatusOK || a.ContentType != "text/event-stream" || !bytes.Contains(a.Body, []byte(`"completion_tokens":3`)) {
				t.Fatalf("chat stream: %d %s\n%s", a.Status, a.ContentType, a.Body)
			}
			a = forward(t, h, buildRequest(acc, ProtocolChat, "openai.gpt-oss-120b-1:0", false), withStream(chatBody, false))
			if a.Status != http.StatusOK || gjson.GetBytes(a.Body, "usage.completion_tokens").Int() != 3 {
				t.Fatalf("chat: %d %s", a.Status, a.Body)
			}
			a = forward(t, h, buildRequest(acc, ProtocolResponses, "openai.gpt-oss-120b-1:0", false), `{"model":"openai.gpt-oss-120b-1:0","input":"ping"}`)
			if a.Status != http.StatusOK {
				t.Fatalf("responses: %d %s", a.Status, a.Body)
			}

			got := f.requests()
			want := []servedRequest{
				{Path: "/model/us." + haikuSegment + "/invoke", Model: "us." + testHaiku, Signed: signed},
				{Path: "/model/us." + haikuSegment + "/invoke-with-response-stream", Model: "us." + testHaiku, Signed: signed},
				{Path: "/model/" + arnSegment + "/invoke", Model: testARN, Signed: signed},
				{Path: "/openai/v1/chat/completions", Model: "openai.gpt-oss-120b-1:0", Signed: signed},
				{Path: "/openai/v1/chat/completions", Model: "openai.gpt-oss-120b-1:0", Signed: signed},
				{Path: "/openai/v1/responses", Model: "openai.gpt-oss-120b-1:0", Signed: signed},
			}
			if len(got) != len(want) {
				t.Fatalf("served %d requests: %+v", len(got), got)
			}
			for i := range want {
				if got[i].Path != want[i].Path || got[i].Model != want[i].Model || got[i].Signed != want[i].Signed {
					t.Errorf("request %d = %s (model %s, signed %v), want %s (model %s, signed %v)",
						i, got[i].Path, got[i].Model, got[i].Signed, want[i].Path, want[i].Model, want[i].Signed)
				}
			}
			// What InvokeModel received: the client's body with the patches.
			var invoked map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got[0].Body), &invoked); err != nil {
				t.Fatal(err)
			}
			if string(invoked["anthropic_beta"]) != `["context-1m-2025-08-07"]` || string(invoked["max_tokens"]) != "64" || len(invoked) != 4 {
				t.Errorf("InvokeModel body = %s", got[0].Body)
			}
			// The streamed chat request asked for usage; the plain one did not.
			if !gjson.Get(got[3].Body, "stream_options.include_usage").Bool() || gjson.Get(got[4].Body, "stream_options").Exists() {
				t.Errorf("chat bodies = %s / %s", got[3].Body, got[4].Body)
			}
		})
	}
}

// TestTestRequestRoundTrip sends the account test the way the host does:
// exactly as the plugin described it, signature included.
func TestTestRequestRoundTrip(t *testing.T) {
	for typ, creds := range typeCreds {
		t.Run(typ, func(t *testing.T) {
			f := newFakeBedrock(t)
			h := startPlugin(t, f)
			acc := save(t, h, typ, creds, settingsOf("us-west-2", "global"))
			tr, err := h.Platform.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{Account: acc})
			if err != nil {
				t.Fatalf("BuildTestRequest: %v", err)
			}
			a := coreForward(t, &pluginv1.BuildUpstreamRequestResponse{Method: tr.GetMethod(), Url: tr.GetUrl(), Headers: tr.GetHeaders()}, tr.GetBodyJson())
			if a.Status != http.StatusOK || gjson.GetBytes(a.Body, "usage.input_tokens").Int() != 9 {
				t.Fatalf("test request: %d %s", a.Status, a.Body)
			}
			if got := f.requests(); len(got) != 1 || got[0].Model != "global."+DefaultTestModel || got[0].Signed != (typ != TypeAPIKey) {
				t.Fatalf("served = %+v", got)
			}
		})
	}
}

// TestFailureRoundTrip classifies what the upstream really answers.
func TestFailureRoundTrip(t *testing.T) {
	const (
		failover = pluginv1.ClassifyErrorResponse_ACTION_FAILOVER
		none     = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED
		cooldown = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_COOLDOWN
		disable  = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE
	)
	for _, tc := range []struct {
		name     string
		typ      string
		creds    map[string]any
		model    string
		expired  bool
		wantType string
		want     pluginv1.ClassifyErrorResponse_AccountEffect
	}{
		{"wrong secret", TypeAccessKey, with(akskCreds, map[string]any{fieldSecretAccessKey: "not-the-secret"}), testHaiku, false, "InvalidSignatureException", disable},
		{"wrong session token", TypeSTS, with(stsCreds, map[string]any{fieldSessionToken: "AnotherToken"}), testHaiku, false, "UnrecognizedClientException", disable},
		{"expired temporary credentials", TypeSTS, stsCreds, testHaiku, true, "ExpiredTokenException", disable},
		// How Bedrock refuses an unknown API key is not documented; the fake
		// answers a 403 without proof against the key, which only cools down.
		{"unknown API key", TypeAPIKey, map[string]any{fieldAPIKey: "ABSKanotherKey00"}, testHaiku, false, "AccessDeniedException", cooldown},
		{"model without access", TypeAccessKey, akskCreds, "anthropic.claude-opus-4-6-v1", false, "AccessDeniedException", cooldown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBedrock(t)
			f.expired = tc.expired
			h := startPlugin(t, f)
			acc := save(t, h, tc.typ, tc.creds, settingsOf("us-west-2", ""))
			a := forward(t, h, buildRequest(acc, ProtocolMessages, tc.model, true), withStream(messagesBody, true))
			// A refused request is not an event stream: it stays as it is.
			if a.Status != http.StatusForbidden || a.ContentType != "application/json" || !strings.HasPrefix(a.Headers["x-amzn-errortype"], tc.wantType+":") {
				t.Fatalf("answer: %d %s %s %s", a.Status, a.ContentType, a.Headers["x-amzn-errortype"], a.Body)
			}
			cls := classifyAnswer(t, h, acc, ProtocolMessages, a)
			if cls.GetAction() != failover || cls.GetAccountEffect() != tc.want || !strings.Contains(cls.GetReason(), "["+tc.wantType+"]") {
				t.Fatalf("classified %s / %s: %s", cls.GetAction(), cls.GetAccountEffect(), cls.GetReason())
			}
			if len(f.requests()) != 0 {
				t.Fatalf("a refused request was served")
			}
		})
	}

	// A request the upstream rejects for its body goes back to the client.
	f := newFakeBedrock(t)
	h := startPlugin(t, f)
	acc := save(t, h, TypeAccessKey, akskCreds, settingsOf("us-west-2", ""))
	a := forward(t, h, buildRequest(acc, ProtocolResponses, "", false), `{"input":"ping"}`)
	cls := classifyAnswer(t, h, acc, ProtocolResponses, a)
	if a.Status != http.StatusBadRequest || cls.GetAction() != pluginv1.ClassifyErrorResponse_ACTION_RETURN_TO_CLIENT || cls.GetAccountEffect() != none ||
		cls.GetClientErrorType() != "invalid_request_error" || cls.GetClientMessage() != "you must provide a model parameter" {
		t.Fatalf("400: %d %+v", a.Status, cls)
	}
}

// A clock that is off: the signature is refused for its time, and that is
// not held against the account.
func TestClockSkewRoundTrip(t *testing.T) {
	f := newFakeBedrock(t)
	p := newTestPlugin(Endpoints{API: f.srv.URL})
	p.now = func() time.Time { return testNow.Add(20 * time.Minute) }
	h := pluginsdktest.Start(t, p, pluginsdktest.Options{SDK: []pluginsdk.Option{pluginsdk.WithInfo("bedrock", Version)}})
	acc := save(t, h, TypeAccessKey, akskCreds, settingsOf("us-west-2", ""))
	tr, err := h.Platform.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{Account: acc})
	if err != nil {
		t.Fatal(err)
	}
	a := coreForward(t, &pluginv1.BuildUpstreamRequestResponse{Method: tr.GetMethod(), Url: tr.GetUrl(), Headers: tr.GetHeaders()}, tr.GetBodyJson())
	cls := classifyAnswer(t, h, acc, ProtocolMessages, a)
	if a.Status != http.StatusForbidden || cls.GetAction() != pluginv1.ClassifyErrorResponse_ACTION_FAILOVER ||
		cls.GetAccountEffect() != pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED || !strings.Contains(cls.GetReason(), "clock") {
		t.Fatalf("skewed clock: %d %s / %s: %s", a.Status, cls.GetAction(), cls.GetAccountEffect(), cls.GetReason())
	}
}

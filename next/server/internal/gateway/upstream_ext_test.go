package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/Sub2API-Devs/sup2api/next/sdk/awseventstream"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// ---------------------------------------------------------------- fakes

const (
	extSecretPrefix = "SECRET-KEY-OF-"
	extTokenPrefix  = "SESSION-TOKEN-OF-"
	extInput        = 25
	extOutput       = 12
)

// extPlatform is a plugin that answers like a Bedrock plugin would: the
// model in the path, body patches, and the two optional fields.
type extPlatform struct {
	*fakePlatform
	// amend edits the built request; nil leaves it as the default below.
	amend func(in *pluginv1.BuildUpstreamRequestRequest, out *pluginv1.BuildUpstreamRequestResponse)
}

func (p *extPlatform) BuildUpstreamRequest(ctx context.Context, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	if _, err := p.fakePlatform.BuildUpstreamRequest(ctx, in); err != nil {
		return nil, err
	}
	creds := gjson.Parse(in.GetAccount().GetCredentialsJson())
	action := "invoke"
	out := &pluginv1.BuildUpstreamRequestResponse{Method: "POST", Headers: map[string]string{"content-type": "application/json", "accept": "application/json"},
		Patches: []*pluginv1.BodyPatch{
			{Op: pluginv1.BodyPatch_OP_DELETE, Path: "model"},
			{Op: pluginv1.BodyPatch_OP_DELETE, Path: "stream"},
			{Op: pluginv1.BodyPatch_OP_SET, Path: "anthropic_version", ValueJson: `"bedrock-2023-05-31"`},
		},
		Signing: &pluginv1.RequestSigning{Scheme: "aws-sigv4", Region: "us-east-1", Service: "bedrock",
			AccessKeyId: creds.Get("access_key_id").String(), SecretAccessKey: creds.Get("secret_access_key").String(),
			SessionToken: creds.Get("session_token").String()},
	}
	if in.GetMeta().GetStream() {
		action, out.ResponseStream = "invoke-with-response-stream", awseventstream.FormatChunk
	}
	// The model id carries ":" as %3A on the wire, as Bedrock's do.
	out.Url = p.base + "/model/anthropic." + in.GetMeta().GetModel() + "-v1%3A0/" + action
	if p.amend != nil {
		p.amend(in, out)
	}
	return out, nil
}

// extCall is one request the upstream received.
type extCall struct {
	accessKey string
	path      string
	header    http.Header
	body      []byte
	// sigErr is the outcome of the upstream's own signature check.
	sigErr error
}

// extUpstream is an upstream that checks SigV4 signatures itself and answers
// InvokeModel with JSON and InvokeModelWithResponseStream with an AWS event
// stream.
type extUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []extCall
	// status answers the access key with an error status.
	status map[string]int
	// exception ends a stream with an exception frame after message_start.
	exception bool
	// plainSSE answers a stream request with ordinary SSE.
	plainSSE bool
}

func newExtUpstream(t *testing.T) *extUpstream {
	u := &extUpstream{status: map[string]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *extUpstream) all() []extCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]extCall(nil), u.calls...)
}

func (u *extUpstream) last(t *testing.T) extCall {
	t.Helper()
	calls := u.all()
	require.NotEmpty(t, calls, "the upstream received nothing")
	return calls[len(calls)-1]
}

var sigV4Authorization = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/ ]+)/(\d{8})/([^/ ]+)/([^/ ]+)/aws4_request, SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]{64})$`)

// verifySigV4 recomputes the signature of a received request from what the
// server saw, following the AWS documentation step by step. It shares no
// code with sdk/reqsign.
func verifySigV4(r *http.Request, body []byte) (accessKey string, err error) {
	m := sigV4Authorization.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return "", fmt.Errorf("authorization header %q is not SigV4", r.Header.Get("Authorization"))
	}
	accessKey, date, region, service, signedHeaders, signature := m[1], m[2], m[3], m[4], m[5], m[6]
	amzDate := r.Header.Get("X-Amz-Date")
	at, perr := time.Parse("20060102T150405Z", amzDate)
	if perr != nil || !strings.HasPrefix(amzDate, date) {
		return accessKey, fmt.Errorf("x-amz-date %q does not fit the scope date %s", amzDate, date)
	}
	if d := time.Since(at); d < -5*time.Minute || d > 5*time.Minute {
		return accessKey, fmt.Errorf("signature expired: %s", amzDate)
	}
	signed := strings.Split(signedHeaders, ";")
	if !sort.StringsAreSorted(signed) {
		return accessKey, errors.New("signed headers are not sorted")
	}
	has := map[string]bool{}
	var headerLines strings.Builder
	for _, name := range signed {
		has[name] = true
		value := strings.Join(r.Header.Values(name), ",")
		if name == "host" {
			value = r.Host
		}
		headerLines.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	for _, name := range []string{"host", "x-amz-date"} {
		if !has[name] {
			return accessKey, fmt.Errorf("%s is not signed", name)
		}
	}
	for _, name := range []string{"content-type", "x-amz-security-token"} {
		if r.Header.Get(name) != "" && !has[name] {
			return accessKey, fmt.Errorf("%s is sent but not signed", name)
		}
	}
	segments := strings.Split(r.URL.EscapedPath(), "/")
	for i, s := range segments {
		segments[i] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	// Sorted by key, then value; a query that does not parse is refused, as
	// its parameters would not all be covered.
	query, qerr := url.ParseQuery(r.URL.RawQuery)
	if qerr != nil {
		return accessKey, fmt.Errorf("query %q: %w", r.URL.RawQuery, qerr)
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		sort.Strings(query[k])
		for _, v := range query[k] {
			pairs = append(pairs, strings.ReplaceAll(url.QueryEscape(k), "+", "%20")+"="+strings.ReplaceAll(url.QueryEscape(v), "+", "%20"))
		}
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	canonical := r.Method + "\n" + strings.Join(segments, "/") + "\n" + strings.Join(pairs, "&") + "\n" +
		headerLines.String() + "\n" + signedHeaders + "\n" + sum(body)
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sum([]byte(canonical))
	mac := func(key []byte, s string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(s))
		return h.Sum(nil)
	}
	// The upstream knows each access key's secret, as AWS does.
	key := mac(mac(mac(mac([]byte("AWS4"+extSecretPrefix+accessKey), date), region), service), "aws4_request")
	if want := hex.EncodeToString(mac(key, toSign)); !hmac.Equal([]byte(want), []byte(signature)) {
		return accessKey, fmt.Errorf("signature mismatch: got %s, want %s for\n%s", signature, want, canonical)
	}
	return accessKey, nil
}

func extEvents() []string {
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_bdrk_1","type":"message","role":"assistant","model":"claude-up","content":[],"usage":{"input_tokens":%d,"output_tokens":1}}}`, extInput),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":%d}}`, extOutput),
		fmt.Sprintf(`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":%d,"outputTokenCount":%d}}`, extInput, extOutput),
	}
}

func extFrame(headers []awseventstream.Header, payload []byte) []byte {
	frame, err := awseventstream.AppendMessage(nil, headers, payload)
	if err != nil {
		panic(err)
	}
	return frame
}

func extChunk(event string) []byte {
	return extFrame([]awseventstream.Header{{Name: ":event-type", Value: "chunk"}, {Name: ":content-type", Value: "application/json"}, {Name: ":message-type", Value: "event"}},
		[]byte(`{"bytes":"`+base64.StdEncoding.EncodeToString([]byte(event))+`"}`))
}

func (u *extUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	accessKey, sigErr := verifySigV4(r, body)
	u.mu.Lock()
	u.calls = append(u.calls, extCall{accessKey: accessKey, path: r.URL.EscapedPath(), header: r.Header.Clone(), body: body, sigErr: sigErr})
	status, exception, plainSSE := u.status[accessKey], u.exception, u.plainSSE
	u.mu.Unlock()
	switch {
	case strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-") && sigErr != nil:
		w.Header().Set("x-amzn-errortype", "InvalidSignatureException")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"The request signature we calculated does not match the signature you provided."}`))
	case status != 0:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"message":"mock status %d"}`, status)
	case !strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_bdrk_1","type":"message","role":"assistant","model":"claude-up","content":[{"type":"text","text":"hello"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":%d}}`, extInput, extOutput)
	case plainSSE:
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range extEvents() {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(ev, "type").String(), ev)
		}
	default:
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		events := extEvents()
		if exception {
			events = events[:1]
		}
		for _, ev := range events {
			_, _ = w.Write(extChunk(ev))
			w.(http.Flusher).Flush()
		}
		if exception {
			_, _ = w.Write(extFrame([]awseventstream.Header{{Name: ":exception-type", Value: "modelStreamErrorException"}, {Name: ":message-type", Value: "exception"}},
				[]byte(`{"message":"the model stream broke"}`)))
		}
	}
}

// syncBuffer is a log sink written from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// extEnv is an environment whose anthropic accounts are served by an
// extPlatform in front of an extUpstream, with every log line captured.
type extEnv struct {
	*env
	up   *extUpstream
	plat *extPlatform
	logs *syncBuffer
}

func newExtEnv(t *testing.T, opts ...envOpt) *extEnv {
	t.Helper()
	logs := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	x := &extEnv{env: newEnv(t, opts...), up: newExtUpstream(t), logs: logs}
	x.plat = &extPlatform{fakePlatform: &fakePlatform{base: x.up.srv.URL}}
	x.gen.accountTypes[0].Client = x.plat
	for id := int64(1); id <= 3; id++ {
		x.setCreds(id, false)
	}
	return x
}

// setCreds gives account id the access key "AK<id>", its secret and, with
// sts, a session token.
func (x *extEnv) setCreds(id int64, sts bool) {
	ak := fmt.Sprintf("AK%d", id)
	creds := map[string]string{"access_key_id": ak, "secret_access_key": extSecretPrefix + ak}
	if sts {
		creds["session_token"] = extTokenPrefix + ak
	}
	raw, _ := json.Marshal(creds)
	x.accounts.set(id, func(a *core.Account) { a.Credentials = raw })
}

// assertNoSecrets fails when a signing secret or session token shows in the
// logs or in anything the test hands over (client bodies, usage records).
func (x *extEnv) assertNoSecrets(t *testing.T, seen ...any) {
	t.Helper()
	haystack := x.logs.String()
	for _, v := range seen {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		haystack += fmt.Sprintf("\n%s\n%+v", raw, v)
	}
	for _, secret := range []string{extSecretPrefix, extTokenPrefix} {
		if i := strings.Index(haystack, secret); i >= 0 {
			t.Fatalf("secret %q leaked: …%s…", secret, haystack[max(0, i-200):min(len(haystack), i+80)])
		}
	}
}

// ---------------------------------------------------------------- signing

// The request the upstream receives carries a signature the upstream
// verifies on its own, over the final body: after the plugin's patches, the
// account's parameter override and the model mapping.
func TestSignedRequestIsVerifiedByUpstream(t *testing.T) {
	for name, tc := range map[string]struct {
		sts      bool
		override string
		prepare  func(x *extEnv)
		check    func(t *testing.T, got extCall)
	}{
		"access key": {},
		"temporary credentials": {sts: true, check: func(t *testing.T, got extCall) {
			assert.Equal(t, extTokenPrefix+"AK1", got.header.Get("X-Amz-Security-Token"))
			assert.Contains(t, got.header.Get("Authorization"), "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token,")
		}},
		"parameter override rewrote the body and set headers": {
			override: `{"operations":[{"mode":"set","path":"temperature","value":0.3},
				{"mode":"append","path":"messages.-1.content","value":"!"},
				{"mode":"set_header","path":"x-custom","value":"v1"}]}`,
			check: func(t *testing.T, got extCall) {
				assert.Equal(t, 0.3, gjson.GetBytes(got.body, "temperature").Float())
				assert.Equal(t, "hello there!", gjson.GetBytes(got.body, "messages.0.content").String())
				assert.Equal(t, "v1", got.header.Get("x-custom"), "an unsigned header the override added still goes out")
			}},
		"model mapping": {
			prepare: func(x *extEnv) {
				x.accounts.set(1, func(a *core.Account) { a.ModelMapping = map[string]string{testModel: "claude-mapped"} })
			},
			check: func(t *testing.T, got extCall) {
				assert.Equal(t, "/model/anthropic.claude-mapped-v1%3A0/invoke", got.path)
			}},
		"stale credential headers from a passthrough are replaced": {
			prepare: func(x *extEnv) {
				x.accounts.set(1, func(a *core.Account) { a.HeaderPassthrough = []string{"x-amz-*"} })
			},
			check: func(t *testing.T, got extCall) {
				assert.Empty(t, got.header.Get("X-Amz-Security-Token"), "the client's token is not sent for an account without one")
				assert.NotEqual(t, "20000101T000000Z", got.header.Get("X-Amz-Date"))
			}},
	} {
		t.Run(name, func(t *testing.T) {
			x := newExtEnv(t)
			x.setCreds(1, tc.sts)
			if tc.override != "" {
				setParamOverride(t, x.env, 1, tc.override)
			}
			if tc.prepare != nil {
				tc.prepare(x)
			}
			r := x.do("/v1/messages", body(testModel, false), map[string]string{"X-Amz-Date": "20000101T000000Z", "X-Amz-Security-Token": "client-token"})
			require.Equal(t, 200, r.status, string(r.body))
			got := x.up.last(t)
			require.NoError(t, got.sigErr)
			assert.Equal(t, "AK1", got.accessKey)
			assert.Contains(t, got.header.Get("Authorization"), "/us-east-1/bedrock/aws4_request, ")
			assert.Empty(t, got.header.Get("x-api-key"))
			assert.False(t, gjson.GetBytes(got.body, "model").Exists(), "the plugin's patches ran before the signature: %s", got.body)
			assert.Equal(t, "bedrock-2023-05-31", gjson.GetBytes(got.body, "anthropic_version").String())
			if tc.check != nil {
				tc.check(t, got)
			} else {
				assert.Equal(t, "/model/anthropic."+testModel+"-v1%3A0/invoke", got.path)
			}
			// Non-streaming JSON is relayed and metered as ever.
			assert.Equal(t, "hello", r.json().Get("content.0.text").String())
			assert.True(t, strings.HasPrefix(r.header.Get("Content-Type"), "application/json"))
			rec := x.record()
			assert.True(t, rec.Success)
			assert.Equal(t, core.UsageTokens{Input: extInput, Output: extOutput}, rec.Tokens)
			x.assertNoSecrets(t, rec, string(r.body), r.header)
		})
	}
}

// A stream-only type (CONTRACTS §83) gets stream: true put back after the
// plugin's patches: the signature covers that final body. (What the client
// gets is not asserted: collecting a forced stream into JSON only knows the
// Responses API's events, which is not what this upstream speaks.)
func TestForcedStreamBodyIsTheOneSigned(t *testing.T) {
	x := newExtEnv(t)
	typ := &x.gen.accountTypes[0].Type
	typ.Platforms = append([]manifest.AccountPlatform(nil), typ.Platforms...)
	typ.Platforms[0].StreamOnly = []string{"anthropic.messages"}
	x.accounts.groups[testGroup] = []int64{1}
	r := x.messages(body(testModel, false))
	got := x.up.last(t)
	require.NoError(t, got.sigErr, "%d %s", r.status, r.body)
	assert.Equal(t, gjson.True, gjson.GetBytes(got.body, "stream").Type, "forced after the plugin deleted it: %s", got.body)
	x.assertNoSecrets(t, x.record(), string(r.body))
}

// A request that fails over is signed again for the next account, with that
// account's credentials.
func TestFailoverSignsAgainPerAttempt(t *testing.T) {
	x := newExtEnv(t)
	x.setCreds(2, true)
	x.up.status["AK1"] = 500
	r := x.messages(body(testModel, false))
	require.Equal(t, 200, r.status, string(r.body))
	calls := x.up.all()
	require.Len(t, calls, 2)
	for i, want := range []string{"AK1", "AK2"} {
		require.NoError(t, calls[i].sigErr, "attempt %d", i)
		assert.Equal(t, want, calls[i].accessKey)
	}
	assert.Empty(t, calls[0].header.Get("X-Amz-Security-Token"))
	assert.Equal(t, extTokenPrefix+"AK2", calls[1].header.Get("X-Amz-Security-Token"))
	assert.NotEqual(t, calls[0].header.Get("Authorization"), calls[1].header.Get("Authorization"))
	rec := x.record()
	assert.True(t, rec.Success)
	assert.EqualValues(t, 2, *rec.AccountID)
	assert.Equal(t, 2, rec.Attempts)
	x.assertNoSecrets(t, rec, string(r.body))
}

// A signature the upstream rejects is an ordinary upstream error: the secret
// is in neither the client's answer, the usage record nor the logs.
func TestRejectedSignatureKeepsTheSecretPrivate(t *testing.T) {
	x := newExtEnv(t)
	x.accounts.groups[testGroup] = []int64{1}
	x.plat.amend = func(_ *pluginv1.BuildUpstreamRequestRequest, out *pluginv1.BuildUpstreamRequestResponse) {
		out.Signing.SecretAccessKey = extSecretPrefix + "someone-else"
		out.Signing.SessionToken = extTokenPrefix + "someone-else"
	}
	r := x.messages(body(testModel, false))
	require.Equal(t, 403, r.status, string(r.body))
	require.Error(t, x.up.last(t).sigErr)
	rec := x.record()
	assert.False(t, rec.Success)
	x.assertNoSecrets(t, rec, string(r.body), r.header)
}

// What this core cannot carry out fails the attempt as plugin-unavailable
// before anything is sent, and the next account is tried.
func TestUnknownSchemeOrFormatSendsNothing(t *testing.T) {
	for name, amend := range map[string]func(out *pluginv1.BuildUpstreamRequestResponse){
		"unknown signing scheme":         func(out *pluginv1.BuildUpstreamRequestResponse) { out.Signing.Scheme = "hmac-v9" },
		"signing without a scheme":       func(out *pluginv1.BuildUpstreamRequestResponse) { out.Signing.Scheme = "" },
		"signing without a secret":       func(out *pluginv1.BuildUpstreamRequestResponse) { out.Signing.SecretAccessKey = "" },
		"signing without a region":       func(out *pluginv1.BuildUpstreamRequestResponse) { out.Signing.Region = "" },
		"unknown response stream format": func(out *pluginv1.BuildUpstreamRequestResponse) { out.ResponseStream = "smithy-v7" },
		"unknown format on an unsigned request": func(out *pluginv1.BuildUpstreamRequestResponse) {
			out.Signing, out.ResponseStream = nil, "smithy-v7"
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newExtEnv(t)
			// Account 1 (first by priority) gets the bad answer.
			x.plat.amend = func(in *pluginv1.BuildUpstreamRequestRequest, out *pluginv1.BuildUpstreamRequestResponse) {
				if in.GetAccount().GetId() == 1 {
					amend(out)
				}
			}
			r := x.messages(body(testModel, true))
			require.Equal(t, 200, r.status, string(r.body))
			calls := x.up.all()
			require.Len(t, calls, 1, "the first attempt sent nothing")
			assert.Equal(t, "AK2", calls[0].accessKey)
			require.NoError(t, calls[0].sigErr)
			rec := x.record()
			assert.EqualValues(t, 2, *rec.AccountID)
			assert.Empty(t, x.accounts.disabled, "a plugin failure does not touch the account")
			x.assertNoSecrets(t, rec, string(r.body))

			// With no other account the client gets the plugin failure.
			x.accounts.groups[testGroup] = []int64{1}
			r = x.messages(body(testModel, true))
			assert.GreaterOrEqual(t, r.status, 500, string(r.body))
			assert.Len(t, x.up.all(), 1, "still nothing sent")
			rec = x.record()
			assert.False(t, rec.Success)
			// A plugin problem surfaces as "no available account" (dispatch).
			assert.Equal(t, errTypeNoAccount, rec.ErrorType)
			x.assertNoSecrets(t, rec, string(r.body))
		})
	}
}

// ---------------------------------------------------------------- response stream

// An AWS event stream answer reaches the client as SSE and is metered by the
// platform's ordinary usage rules.
func TestEventStreamIsRelayedAsSSEAndBilled(t *testing.T) {
	x := newExtEnv(t)
	r := x.messages(body(testModel, true))
	require.Equal(t, 200, r.status, string(r.body))
	assert.True(t, isSSE(r.header.Get("Content-Type")), r.header.Get("Content-Type"))
	var want strings.Builder
	for _, ev := range extEvents() {
		fmt.Fprintf(&want, "event: %s\ndata: %s\n\n", gjson.Get(ev, "type").String(), ev)
	}
	assert.Equal(t, want.String(), string(r.body))
	got := x.up.last(t)
	require.NoError(t, got.sigErr)
	assert.Equal(t, "/model/anthropic."+testModel+"-v1%3A0/invoke-with-response-stream", got.path)
	assert.False(t, gjson.GetBytes(got.body, "stream").Exists())
	rec := x.record()
	assert.True(t, rec.Success)
	assert.True(t, rec.Stream)
	assert.Equal(t, core.UsageTokens{Input: extInput, Output: extOutput}, rec.Tokens)
	assert.NotZero(t, rec.FirstTokenMs)
	assert.True(t, rec.Billable)
	x.assertNoSecrets(t, rec, string(r.body))
}

// A client speaking another protocol gets the converted stream: conversion
// sees ordinary Anthropic SSE.
func TestEventStreamOnAConvertedRoute(t *testing.T) {
	x := newExtEnv(t, openAIClients)
	path, input := openAIConversionInput("openai.chat", true)
	r := x.do(path, input, map[string]string{"authorization": "Bearer " + testKey})
	require.Equal(t, 200, r.status, string(r.body))
	assert.True(t, isSSE(r.header.Get("Content-Type")))
	assert.Contains(t, string(r.body), `"content":"hello"`)
	assert.Equal(t, 1, strings.Count(string(r.body), "data: [DONE]"), string(r.body))
	require.NoError(t, x.up.last(t).sigErr)
	rec := x.record()
	assert.True(t, rec.Success)
	assert.Equal(t, "openai.chat", rec.Protocol)
	assert.Equal(t, "anthropic.messages", rec.UpstreamProtocol)
	assert.Equal(t, core.UsageTokens{Input: extInput, Output: extOutput}, rec.Tokens)
}

// An exception frame in mid-stream reaches the client as an error event and
// is recorded as an upstream stream error.
func TestEventStreamExceptionBecomesAnErrorEvent(t *testing.T) {
	x := newExtEnv(t)
	x.up.exception = true
	r := x.messages(body(testModel, true))
	require.Equal(t, 200, r.status, string(r.body))
	assert.Contains(t, string(r.body), "event: message_start\n")
	assert.True(t, strings.HasSuffix(string(r.body),
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"modelStreamErrorException\",\"message\":\"the model stream broke\"}}\n\n"), string(r.body))
	rec := x.record()
	assert.False(t, rec.Success)
	assert.Contains(t, rec.ErrorMessage, "the model stream broke")
	assert.Equal(t, int64(extInput), rec.Tokens.Input)
}

// Answers that are not in the named framing are left alone: a JSON answer
// to a non-streaming request, and plain SSE.
func TestResponseStreamOnlyAdaptsItsOwnContentType(t *testing.T) {
	x := newExtEnv(t)
	x.plat.amend = func(_ *pluginv1.BuildUpstreamRequestRequest, out *pluginv1.BuildUpstreamRequestResponse) {
		out.ResponseStream = awseventstream.FormatChunk // also on the non-streaming request
	}
	r := x.messages(body(testModel, false))
	require.Equal(t, 200, r.status, string(r.body))
	assert.Equal(t, "hello", r.json().Get("content.0.text").String())
	assert.Equal(t, core.UsageTokens{Input: extInput, Output: extOutput}, x.record().Tokens)

	x.up.plainSSE = true
	r = x.messages(body(testModel, true))
	require.Equal(t, 200, r.status, string(r.body))
	assert.True(t, strings.HasPrefix(string(r.body), "event: message_start\ndata: {"), string(r.body))
	assert.Equal(t, core.UsageTokens{Input: extInput, Output: extOutput}, x.record().Tokens)
}

// ---------------------------------------------------------------- plugins without the fields

// A plugin that sets neither field gets the request it built, byte for
// byte: no signing header appears, and an event stream answer it did not
// announce is not decoded.
func TestPluginsWithoutTheFieldsAreUntouched(t *testing.T) {
	x := newExtEnv(t)
	var sent *pluginv1.BuildUpstreamRequestResponse
	x.plat.amend = func(_ *pluginv1.BuildUpstreamRequestRequest, out *pluginv1.BuildUpstreamRequestResponse) {
		out.Signing, out.ResponseStream = nil, ""
		out.Headers["authorization"] = "Bearer plugin-key"
		sent = out
	}
	in := body(testModel, false)
	r := x.messages(in)
	require.Equal(t, 200, r.status, string(r.body))
	got := x.up.last(t)
	assert.Equal(t, "Bearer plugin-key", got.header.Get("Authorization"))
	for name := range got.header {
		assert.False(t, strings.HasPrefix(strings.ToLower(name), "x-amz-"), "unexpected header %s", name)
	}
	want, err := applyPatches(mustJSON(in), sent.GetPatches())
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got.body), "the body is the client's with the plugin's patches, nothing else")
	assert.True(t, x.record().Success)

	// The same exchange through a core without the extension points: the
	// requests on the wire are identical.
	before := wireRequest(got)
	x.gen.accountTypes[0].Client = &legacyPlatform{x.plat}
	r = x.messages(in)
	require.Equal(t, 200, r.status, string(r.body))
	assert.Equal(t, before, wireRequest(x.up.last(t)))
	assert.True(t, x.record().Success)

	// An event stream the plugin did not announce stays binary.
	r = x.messages(body(testModel, true))
	assert.False(t, isSSE(r.header.Get("Content-Type")), "%d %s", r.status, r.header.Get("Content-Type"))
	assert.NotContains(t, string(r.body), "event: message_start")
}

// legacyPlatform answers with a message built by a plugin compiled before
// the fields existed: the response is re-encoded without them.
type legacyPlatform struct{ *extPlatform }

func (p *legacyPlatform) BuildUpstreamRequest(ctx context.Context, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	out, err := p.extPlatform.BuildUpstreamRequest(ctx, in)
	if err != nil {
		return nil, err
	}
	return &pluginv1.BuildUpstreamRequestResponse{Method: out.GetMethod(), Url: out.GetUrl(), Headers: out.GetHeaders(),
		Patches: out.GetPatches(), UpstreamModel: out.GetUpstreamModel()}, nil
}

// wireRequest is what an upstream saw of a request, in a comparable form.
func wireRequest(c extCall) string {
	names := make([]string, 0, len(c.header))
	for name := range c.header {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(c.path + "\n")
	for _, name := range names {
		b.WriteString(name + ": " + strings.Join(c.header[name], ",") + "\n")
	}
	b.WriteString("\n" + string(c.body))
	return b.String()
}

// ---------------------------------------------------------------- units

func TestCheckUpstreamExt(t *testing.T) {
	sig := func(scheme string) *pluginv1.RequestSigning {
		return &pluginv1.RequestSigning{Scheme: scheme, SecretAccessKey: extSecretPrefix + "x", SessionToken: extTokenPrefix + "x"}
	}
	for name, tc := range map[string]struct {
		built *pluginv1.BuildUpstreamRequestResponse
		ok    bool
	}{
		"nil":            {nil, true},
		"neither":        {&pluginv1.BuildUpstreamRequestResponse{}, true},
		"both known":     {&pluginv1.BuildUpstreamRequestResponse{Signing: sig("aws-sigv4"), ResponseStream: awseventstream.FormatChunk}, true},
		"unknown scheme": {&pluginv1.BuildUpstreamRequestResponse{Signing: sig("x")}, false},
		"empty scheme":   {&pluginv1.BuildUpstreamRequestResponse{Signing: sig("")}, false},
		"unknown format": {&pluginv1.BuildUpstreamRequestResponse{ResponseStream: "x"}, false},
	} {
		err := checkUpstreamExt(tc.built)
		assert.Equal(t, tc.ok, err == nil, "%s: %v", name, err)
		if err != nil {
			assert.NotContains(t, err.Error(), extSecretPrefix, name)
			assert.NotContains(t, err.Error(), extTokenPrefix, name)
		}
	}
	assert.Nil(t, unsupportedOnWebSocket(&pluginv1.BuildUpstreamRequestResponse{}))
	for _, built := range []*pluginv1.BuildUpstreamRequestResponse{{Signing: sig("aws-sigv4")}, {ResponseStream: awseventstream.FormatChunk}} {
		res := unsupportedOnWebSocket(built)
		require.NotNil(t, res)
		assert.Equal(t, attemptFailover, res.kind)
		assert.Equal(t, errTypePluginUnavailable, res.err.RecordType)
	}
}

// The request leaves as it is signed: the query in canonical order, no
// default port in Host, and a body-less request over the empty payload even
// though the caller still holds body bytes.
func TestSignUpstreamRequest(t *testing.T) {
	built := &pluginv1.BuildUpstreamRequestResponse{Signing: &pluginv1.RequestSigning{Scheme: "aws-sigv4", Region: "us-east-1", Service: "bedrock",
		AccessKeyId: "AKIAUNIT", SecretAccessKey: extSecretPrefix + "AKIAUNIT"}}
	held := []byte(`{"held":"by the caller"}`)
	for name, tc := range map[string]struct {
		method, url string
		sent        []byte
		query, host string
	}{
		"post":             {"POST", "https://bedrock.example.com/model/m%3A0/invoke", held, "", "bedrock.example.com"},
		"get, no body":     {"GET", "https://bedrock.example.com/foundation-models", nil, "", "bedrock.example.com"},
		"query reordered":  {"GET", "https://bedrock.example.com/x?b=2&a=2&a=1&c=a+b", nil, "a=1&a=2&b=2&c=a%20b", "bedrock.example.com"},
		"default port":     {"POST", "https://bedrock.example.com:443/model/m/invoke", held, "", "bedrock.example.com"},
		"non-default port": {"POST", "https://bedrock.example.com:8443/model/m/invoke", held, "", "bedrock.example.com:8443"},
	} {
		var reqBody io.Reader
		if tc.sent != nil {
			reqBody = bytes.NewReader(tc.sent)
		}
		req, err := http.NewRequest(tc.method, tc.url, reqBody)
		require.NoError(t, err, name)
		require.Nil(t, signUpstreamRequest(req, held, built), name)
		assert.Equal(t, tc.query, req.URL.RawQuery, name)
		assert.Equal(t, tc.host, req.Host, name)
		// The upstream sees the body that was sent, not the one held.
		_, err = verifySigV4(req, tc.sent)
		assert.NoError(t, err, name)
	}

	// Nothing asked: the request is left alone.
	req, _ := http.NewRequest("GET", "https://example.com:443/x?b=2&a=1", nil)
	require.Nil(t, signUpstreamRequest(req, nil, &pluginv1.BuildUpstreamRequestResponse{}))
	assert.Equal(t, "b=2&a=1", req.URL.RawQuery)
	assert.Equal(t, "example.com:443", req.Host)
	assert.Empty(t, req.Header)

	// What the signer refuses fails the attempt over to the next account and
	// names no credential.
	for name, edit := range map[string]func(*http.Request, *pluginv1.BuildUpstreamRequestResponse){
		"unparsable query": func(r *http.Request, _ *pluginv1.BuildUpstreamRequestResponse) { r.URL.RawQuery = "a=1;b=2" },
		"unknown scheme":   func(_ *http.Request, b *pluginv1.BuildUpstreamRequestResponse) { b.Signing.Scheme = "x" },
		"no region":        func(_ *http.Request, b *pluginv1.BuildUpstreamRequestResponse) { b.Signing.Region = "" },
		"unknown format":   func(_ *http.Request, b *pluginv1.BuildUpstreamRequestResponse) { b.ResponseStream = "aws-eventstream" }, // the name before FormatChunk
	} {
		req, _ := http.NewRequest("GET", "https://example.com/x", nil)
		b := &pluginv1.BuildUpstreamRequestResponse{Signing: &pluginv1.RequestSigning{Scheme: "aws-sigv4", Region: "us-east-1", Service: "bedrock",
			AccessKeyId: "AKIAUNIT", SecretAccessKey: extSecretPrefix + "AKIAUNIT"}}
		edit(req, b)
		res := signUpstreamRequest(req, nil, b)
		require.NotNil(t, res, name)
		assert.Equal(t, attemptFailover, res.kind, name)
		assert.Equal(t, errTypePluginUnavailable, res.err.RecordType, name)
		assert.NotContains(t, fmt.Sprint(res.err), extSecretPrefix, name)
		assert.Empty(t, req.Header.Get("Authorization"), name)
	}
}

func TestAdaptResponseStream(t *testing.T) {
	frames := extChunk(`{"type":"ping"}`)
	for name, tc := range map[string]struct {
		format, contentType string
		adapted             bool
	}{
		"announced":                {awseventstream.FormatChunk, "application/vnd.amazon.eventstream", true},
		"announced, with a param":  {awseventstream.FormatChunk, "Application/VND.amazon.eventstream; charset=binary", true},
		"not announced":            {"", "application/vnd.amazon.eventstream", false},
		"announced, JSON answer":   {awseventstream.FormatChunk, "application/json", false},
		"announced, SSE answer":    {awseventstream.FormatChunk, "text/event-stream", false},
		"announced, no type":       {awseventstream.FormatChunk, "", false},
		"unknown format (guarded)": {"x", "application/vnd.amazon.eventstream", false},
	} {
		closed := false
		resp := &http.Response{Header: http.Header{"Content-Length": {"99"}}, ContentLength: 99,
			Body: closeSpy{bytes.NewReader(frames), &closed}}
		if tc.contentType != "" {
			resp.Header.Set("Content-Type", tc.contentType)
		}
		adaptResponseStream(resp, tc.format)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err, name)
		require.NoError(t, resp.Body.Close(), name)
		assert.True(t, closed, "%s: closing reaches the upstream body", name)
		if !tc.adapted {
			assert.Equal(t, frames, got, name)
			assert.Equal(t, tc.contentType, resp.Header.Get("Content-Type"), name)
			assert.Equal(t, "99", resp.Header.Get("Content-Length"), name)
			continue
		}
		assert.Equal(t, "event: ping\ndata: {\"type\":\"ping\"}\n\n", string(got), name)
		assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"), name)
		assert.Empty(t, resp.Header.Get("Content-Length"), name)
		assert.EqualValues(t, -1, resp.ContentLength, name)
	}
}

type closeSpy struct {
	io.Reader
	closed *bool
}

func (c closeSpy) Close() error { *c.closed = true; return nil }

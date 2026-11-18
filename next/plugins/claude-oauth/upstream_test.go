package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// messagesBody is a Messages body as the host hands it over, written the
// way no encoder would write it again: unknown fields, an unusual key order,
// exponent and trailing-zero numbers, escapes and spacing. The plugin makes
// no body edits, so these bytes must reach the upstream unchanged.
const messagesBody = `{"stream":true,  "model":"claude-opus-5","max_tokens":1.0e3,` +
	`"x_unknown":{"b":1,"a":[1.50,"café"]},` +
	`"messages":[{"role":"user","content":"Hi"}],"temperature":0.70}`

const plainBody = `{"model":"claude-opus-5","messages":[{"role":"user","content":"Hi"}]}`

// executeCase is one request-path input and the request the plugin builds
// for it. The inputs (in) are also what the conformance suite
// (sdk/pluginsdk/pluginsdktest/conformance) is to be run with.
type executeCase struct {
	name string
	in   *pluginv1.ExecuteRequest
	want *pluginv1.UpstreamRequest
}

func executeRequest(accountType, protocol string, stream bool, body string) *pluginv1.ExecuteRequest {
	return &pluginv1.ExecuteRequest{
		Meta: &pluginv1.RequestMeta{Protocol: protocol, Model: "claude-opus-5", Stream: stream},
		Account: &pluginv1.Account{
			Id: 1, Name: "test", Platform: "anthropic", Type: accountType,
			CredentialsJson: `{"access_token":"sk-ant-test123","refresh_token":"rt","expires_at":9999999999}`,
		},
		Body:           []byte(body),
		InboundHeaders: map[string]string{"anthropic-beta": "client-beta", "user-agent": "claude-cli/2.1.90"},
	}
}

// executeCases covers both account types and both protocols of the
// anthropic platform, streaming and not. Every header is authoritative (see
// BuildUpstream); the inbound headers are not forwarded by the plugin.
func executeCases() []executeCase {
	headers := map[string]string{
		"authorization":     "Bearer sk-ant-test123",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "oauth-2025-04-20",
		"content-type":      "application/json",
	}
	var cases []executeCase
	for _, typ := range []string{"claude_oauth", "claude_setup_token"} {
		cases = append(cases,
			executeCase{typ + "/messages stream", executeRequest(typ, "anthropic.messages", true, messagesBody),
				&pluginv1.UpstreamRequest{Method: "POST", Url: MessagesURL, Headers: headers, Body: []byte(messagesBody)}},
			executeCase{typ + "/messages", executeRequest(typ, "anthropic.messages", false, plainBody),
				&pluginv1.UpstreamRequest{Method: "POST", Url: MessagesURL, Headers: headers, Body: []byte(plainBody)}},
			// count_tokens goes to its own endpoint with the same OAuth
			// headers: an OAuth bearer without the oauth beta is a 401.
			executeCase{typ + "/count_tokens", executeRequest(typ, "anthropic.count_tokens", false, plainBody),
				&pluginv1.UpstreamRequest{Method: "POST", Url: CountTokensURL, Headers: headers, Body: []byte(plainBody)}},
		)
	}
	return cases
}

// startPlatform starts the plugin under the fake host and fails the test if
// anything dials through the default HTTP transport while it runs: the
// request path relays through the host and opens no socket.
func startPlatform(t *testing.T) *pluginsdktest.Harness {
	t.Helper()
	var dials atomic.Int32
	saved := http.DefaultTransport
	http.DefaultTransport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("the request path must not dial")
	}}
	t.Cleanup(func() {
		http.DefaultTransport = saved
		if n := dials.Load(); n != 0 {
			t.Errorf("%d dials on the request path", n)
		}
	})
	return pluginsdktest.Start(t, newPlugin(), pluginsdktest.Options{SDK: []pluginsdk.Option{pluginsdk.WithManifest(manifestRaw)}})
}

// relayed asserts the form of a plain endpoint, exactly one relay that does
// not wait and a host_usage answer, and returns the relayed request.
func relayed(t *testing.T, ex *pluginsdktest.Execution) *pluginv1.UpstreamRequest {
	t.Helper()
	relay := ex.Relayed(t)
	if relay.Wait || !proto.Equal(ex.Response, pluginsdk.ReportHostUsage()) {
		t.Fatalf("wait = %v, response = %v; want a relay without waiting and host_usage", relay.Wait, ex.Response)
	}
	return relay.Request
}

func TestExecuteRelaysTheFinalRequest(t *testing.T) {
	h := startPlatform(t)
	for _, tc := range executeCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := relayed(t, h.Execute(t.Context(), tc.in))
			if !proto.Equal(got, tc.want) {
				t.Fatalf("relayed\n %v\nwant\n %v", got, tc.want)
			}
		})
	}
}

// The same input builds the same request. BuildUpstream reads no clock, so
// no clock needs fixing.
func TestExecuteIsDeterministic(t *testing.T) {
	h := startPlatform(t)
	for _, tc := range executeCases() {
		first := relayed(t, h.Execute(t.Context(), tc.in))
		if second := relayed(t, h.Execute(t.Context(), tc.in)); !proto.Equal(first, second) {
			t.Fatalf("%s: two builds differ\n %v\n %v", tc.name, first, second)
		}
	}
}

// A later attempt is built from scratch: here the host renewed the token
// between the attempts, and the second request carries the new one.
func TestExecuteRebuildsEveryAttempt(t *testing.T) {
	h := startPlatform(t)
	in := executeRequest("claude_oauth", "anthropic.messages", true, messagesBody)
	first := relayed(t, h.Execute(t.Context(), in))
	in.Attempt = 1
	in.Account.CredentialsJson = `{"access_token":"sk-ant-renewed"}`
	second := relayed(t, h.Execute(t.Context(), in))
	if first.GetHeaders()["authorization"] != "Bearer sk-ant-test123" || second.GetHeaders()["authorization"] != "Bearer sk-ant-renewed" {
		t.Fatalf("authorization: attempt 0 %q, attempt 1 %q", first.GetHeaders()["authorization"], second.GetHeaders()["authorization"])
	}
}

// A body far over any message size of the old contract goes through whole
// and byte for byte.
func TestExecuteCarriesLargeBodies(t *testing.T) {
	h := startPlatform(t)
	body := `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 48<<20) + `"}]}`
	got := relayed(t, h.Execute(t.Context(), executeRequest("claude_oauth", "anthropic.messages", true, body)))
	if !bytes.Equal(got.GetBody(), []byte(body)) {
		t.Fatalf("body changed: %d bytes relayed, %d handed over", len(got.GetBody()), len(body))
	}
}

// What cannot be built fails the attempt before anything is relayed, and a
// relay the host refuses is not tried again.
func TestExecuteFailsWithoutRelaying(t *testing.T) {
	h := startPlatform(t)
	noAccount := executeRequest("claude_oauth", "anthropic.messages", true, messagesBody)
	noAccount.Account = nil
	noMeta := executeRequest("claude_oauth", "anthropic.messages", true, messagesBody)
	noMeta.Meta = nil
	badCredentials := executeRequest("claude_oauth", "anthropic.messages", true, messagesBody)
	badCredentials.Account.CredentialsJson = `{invalid`
	for name, in := range map[string]*pluginv1.ExecuteRequest{"no account": noAccount, "no meta": noMeta, "bad credentials": badCredentials} {
		if ex := h.Execute(t.Context(), in); ex.Err == nil || len(ex.Relays())+len(ex.RelayErrors()) != 0 {
			t.Errorf("%s: err %v, %d relays, %d refused", name, ex.Err, len(ex.Relays()), len(ex.RelayErrors()))
		}
	}

	h.Host.RelayFunc = func(context.Context, *pluginv1.UpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "refused")
	}
	ex := h.Execute(t.Context(), executeRequest("claude_oauth", "anthropic.messages", true, messagesBody))
	if ex.Err == nil || len(ex.Relays()) != 0 || len(ex.RelayErrors()) != 1 {
		t.Fatalf("refused relay: err %v, %d relays, %d refused", ex.Err, len(ex.Relays()), len(ex.RelayErrors()))
	}
}

// A count_tokens request never reaches the Messages URL, whatever its body
// asks for: on Messages a body with max_tokens would run a real generation
// on an endpoint billed as free. The stream flag (absent on count_tokens)
// changes nothing, and a protocol the plugin has no URL for is refused
// instead of falling back to Messages.
func TestCountTokensNeverReachesMessages(t *testing.T) {
	h := startPlatform(t)
	generating := `{"model":"claude-opus-5","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	for _, typ := range []string{"claude_oauth", "claude_setup_token"} {
		for _, stream := range []bool{false, true} {
			in := executeRequest(typ, "anthropic.count_tokens", stream, generating)
			in.InboundHeaders["anthropic-beta"] = "oauth-2025-04-20"
			got := relayed(t, h.Execute(t.Context(), in))
			if got.GetUrl() != "https://api.anthropic.com/v1/messages/count_tokens" || got.GetHeaders()["anthropic-beta"] != OAuthBeta {
				t.Errorf("%s stream=%v: url %q, anthropic-beta %q", typ, stream, got.GetUrl(), got.GetHeaders()["anthropic-beta"])
			}
		}
	}
	for _, protocol := range []string{"", "anthropic.unknown", "openai.chat_completions"} {
		if ex := h.Execute(t.Context(), executeRequest("claude_oauth", protocol, false, plainBody)); ex.Err == nil || len(ex.Relays()) != 0 {
			t.Errorf("protocol %q: err %v, %d relays; want refused without relaying", protocol, ex.Err, len(ex.Relays()))
		}
	}
}

package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/jsonedit"
)

// unsupportedFields are body fields the ChatGPT Codex backend rejects
// ("Unsupported parameter"); they are removed, not reinterpreted.
var unsupportedFields = []string{
	"max_output_tokens", "max_completion_tokens", "temperature", "top_p",
	"frequency_penalty", "presence_penalty", "user", "metadata",
	"safety_identifier", "stream_options", "truncation", "prompt_cache_retention",
	"stop_sequences",
}

// wsKept is the field of unsupportedFields a WebSocket message keeps: a
// response.create has no stream, and the plugin leaves stream settings alone
// there.
const wsKept = "stream_options"

// removedFields are the fields removed from a request, over HTTP or in a
// WebSocket message.
func removedFields(ws bool) []string {
	if !ws {
		return unsupportedFields
	}
	return slices.DeleteFunc(slices.Clone(unsupportedFields), func(f string) bool { return f == wsKept })
}

// accountHeaders are the bearer token, the ChatGPT account it acts for and
// sup2api's own, truthful identification. Every header of the plugin
// (these, httpHeaders, the WebSocket beta and the session) is authoritative,
// written after the operator's header settings, as every header of a code
// plugin was before forwarding v2: no passthrough or override changes what
// the plugin sends. No client header is forwarded.
func accountHeaders(c credentials) map[string]string {
	h := map[string]string{
		"authorization": "Bearer " + c.AccessToken,
		"originator":    Originator,
		"user-agent":    UserAgent,
	}
	if c.ChatGPTAccountID != "" {
		h["chatgpt-account-id"] = c.ChatGPTAccountID
	}
	return h
}

// httpHeaders are the further headers of an HTTP request: the upstream only
// streams.
var httpHeaders = map[string]string{"accept": "text/event-stream", "content-type": "application/json"}

// upstreamSession is the upstream session of the request's conversation,
// "" for none. The Codex backend keeps a conversation's prompt cache on the
// machine its session picks; without one the turns land anywhere and most
// of a long prompt is read uncached. The session is derived from the
// client's prompt_cache_key - the key the core's sticky rule already binds
// the conversation to one account by - mixed with the API key, so two keys
// sending the same prompt_cache_key never share an upstream session and the
// client's value is not echoed. A body without the key (or with a key that
// is not a non-empty string) gets none.
func upstreamSession(apiKeyID int64, body []byte) string {
	key := gjson.GetBytes(body, "prompt_cache_key")
	if key.Type != gjson.String || key.Str == "" {
		return ""
	}
	return upstreamSessionID(apiKeyID, key.Str)
}

// upstreamSessionID is a UUID (version 8, RFC 9562) hashed from the API key
// id and the prompt cache key.
func upstreamSessionID(apiKeyID int64, key string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(apiKeyID, 10) + ":" + key))
	sum[6] = sum[6]&0x0f | 0x80
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// responsesURL is the HTTP Responses endpoint of the Codex backend.
func (p *Plugin) responsesURL() string { return p.ep.ChatGPT + "/backend-api/codex/responses" }

// accountCredentials checks the account type and reads the credentials.
func accountCredentials(acc *pluginv1.Account) (credentials, error) {
	if typ := acc.GetType(); !knownType(typ) {
		return credentials{}, status.Errorf(codes.FailedPrecondition, "account %d: unsupported account type %q", acc.GetId(), typ)
	}
	c, err := parseCredentials(acc.GetCredentialsJson())
	if err != nil {
		return c, status.Errorf(codes.InvalidArgument, "account %d: parse credentials: %v", acc.GetId(), err)
	}
	if c.AccessToken == "" {
		return c, status.Errorf(codes.FailedPrecondition, "account %d has no access_token; authorize it again / 账号没有 access_token，请重新授权", acc.GetId())
	}
	return c, nil
}

// userMessage is a Responses input array holding one user text message.
func userMessage(text string) []byte {
	b, _ := json.Marshal([]map[string]any{{
		"type": "message", "role": "user",
		"content": []map[string]string{{"type": "input_text", "text": text}},
	}})
	return b
}

// Execute implements pluginsdk.Executor: build the request (BuildUpstream)
// and hand it to the host without waiting; the host sends it after Execute
// returned and reads the usage by the openai platform's rules (host_usage).
func (p *Plugin) Execute(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
	return pluginsdk.ExecuteDefault(ctx, p, in)
}

// BuildUpstream implements pluginsdk.UpstreamBuilder.
//
// HTTP (openai.responses): POST to the Codex backend's Responses endpoint
// with the body made acceptable by codexBody. stream is not touched: the
// upstream only streams, and the core sets it before Execute for account
// types that declare streamOnly; keeping it is all the plugin does.
//
// WebSocket (openai.responses_ws): one call per turn, the body being the
// turn's response.create message. The request is the handshake (GET on the
// ws(s) URL with the beta header) and the edited message; the core opens
// the connection with the handshake of the session's first turn and sends
// every turn's message on it.
func (p *Plugin) BuildUpstream(_ context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error) {
	c, err := accountCredentials(in.GetAccount())
	if err != nil {
		return nil, err
	}
	u, ws, err := p.transport(in.GetMeta().GetProtocol())
	if err != nil {
		return nil, err
	}
	body, err := codexBody(in.GetBody(), ws)
	if err != nil {
		return nil, err
	}
	session := upstreamSession(in.GetMeta().GetApiKeyId(), body)
	return u.HeaderMap(accountHeaders(c)).
		Header("session_id", session).
		Header("conversation_id", session).
		Body(body).
		Model(pluginsdk.RequestModel(in)).
		Build()
}

// transport starts the request of an upstream protocol: the HTTP request
// or the WebSocket handshake, with the headers of each.
func (p *Plugin) transport(protocol string) (u *pluginsdk.Upstream, ws bool, err error) {
	switch protocol {
	case ProtocolResponses:
		return pluginsdk.NewUpstream(http.MethodPost, p.responsesURL()).HeaderMap(httpHeaders), false, nil
	case ProtocolResponsesWS:
		wsURL, err := webSocketURL(p.responsesURL())
		if err != nil {
			return nil, false, err
		}
		return pluginsdk.NewUpstream(http.MethodGet, wsURL).Header("openai-beta", WebSocketBeta), true, nil
	default:
		return nil, false, status.Errorf(codes.InvalidArgument, "unsupported upstream protocol %q", protocol)
	}
}

// webSocketURL is the ws(s) URL of an http(s) one.
func webSocketURL(httpURL string) (string, error) {
	if rest, ok := strings.CutPrefix(httpURL, "https://"); ok {
		return "wss://" + rest, nil
	}
	if rest, ok := strings.CutPrefix(httpURL, "http://"); ok {
		return "ws://" + rest, nil
	}
	return "", status.Errorf(codes.Internal, "upstream URL %q has no http(s) scheme", httpURL)
}

// codexBody makes a Responses body (an HTTP request or a WebSocket
// response.create message) one the Codex backend accepts, editing the
// received bytes in place of the fields concerned (jsonedit):
//
//   - store=false (stored responses are refused);
//   - a string input becomes an array holding one user message (an array
//     is required; a blank (whitespace-only) string becomes []);
//   - input items with role "system" get role "developer" (instructions or
//     developer messages carry the system prompt);
//   - instructions "" when the body has none (the field is required);
//   - the fields the upstream refuses are removed (removedFields).
func codexBody(body []byte, ws bool) ([]byte, error) {
	d, err := jsonedit.Parse(body)
	if err != nil || !d.Root().IsObject() {
		return nil, status.Error(codes.InvalidArgument, "the request body is not a JSON object")
	}
	for _, f := range removedFields(ws) {
		d.Delete(f)
	}
	d.Set("store", []byte("false"))
	d.Default("instructions", []byte(`""`))
	inputEdits(d, d.Root().Get("input"))
	out, err := d.Bytes()
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return out, nil
}

// inputEdits turn a string input into an array and system items into
// developer items.
func inputEdits(d *jsonedit.Doc, input gjson.Result) {
	switch {
	case input.Type == gjson.String && strings.TrimSpace(input.Str) == "":
		d.Replace(input, []byte("[]"))
	case input.Type == gjson.String:
		d.Replace(input, userMessage(input.Str))
	default:
		jsonedit.EachElement(input, func(item gjson.Result) {
			if role, ok := systemRole(item); ok {
				d.Replace(role, []byte(`"developer"`))
			}
		})
	}
}

// systemRole returns the role of an input item whose role is the string
// "system".
func systemRole(item gjson.Result) (gjson.Result, bool) {
	role := item.Get("role")
	return role, role.Type == gjson.String && role.Str == "system"
}

// BuildTestRequest implements pluginsdk.Platform: the smallest streaming
// Responses request (store=false, stream=true, an input array, empty
// instructions). Its usage is read with the openai.responses rules.
func (p *Plugin) BuildTestRequest(_ context.Context, in *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
	c, err := accountCredentials(in.GetAccount())
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(in.GetModel())
	if model == "" {
		model = DefaultTestModel
	}
	b, _ := json.Marshal(map[string]any{
		"model":        model,
		"instructions": "",
		"input":        json.RawMessage(userMessage("ping")),
		"store":        false,
		"stream":       true,
	})
	h := accountHeaders(c)
	maps.Copy(h, httpHeaders)
	return &pluginv1.BuildTestRequestResponse{
		Method:        http.MethodPost,
		Url:           p.responsesURL(),
		Headers:       h,
		BodyJson:      string(b),
		Model:         model,
		UsageProtocol: ProtocolResponses,
	}, nil
}

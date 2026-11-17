package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// Request fields the manifest asks the core for (accountTypes[].platforms[]
// .requestFields), besides "model". They are gjson queries, so the plugin
// learns what it needs without being handed the whole prompt:
//
//   - FieldHasInstructions is 1 when the body has an instructions field,
//     0 when not (the Codex backend requires one);
//   - FieldStringInput is the input when it is a JSON string, and absent
//     when it is an array (the Codex backend requires an array);
//   - FieldSystemRoles has one entry per item of an array input, in order:
//     ["system"] for an item whose role is "system", [] for any other item
//     (another role, no role, not an object). It is absent when the input is
//     not an array. Only "system" is ever echoed, so the field stays at about
//     three bytes per item whatever the items hold;
//   - FieldPromptCacheKey is the client's prompt_cache_key (sessionHeaders).
const (
	FieldHasInstructions = "[instructions]|#"
	FieldStringInput     = `[input]|#(%"*")`
	FieldSystemRoles     = `input.#.[role].#(=="system")#`
	FieldPromptCacheKey  = "prompt_cache_key"
)

// maxRoleRewrites caps the role patches of one request. Each patch is a copy
// of the body in the core, so a body made of thousands of system items would
// cost thousands of copies; real requests carry a few. Items past the cap are
// left as they are (the upstream answers its own error).
const maxRoleRewrites = 256

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

// upstreamHeaders are the upstream headers of an account: the bearer token,
// the ChatGPT account and sup2api's own, truthful identification. No client
// header is forwarded.
func upstreamHeaders(c credentials) map[string]string {
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

// sessionHeaders add the upstream session of the request's conversation.
// The Codex backend keeps a conversation's prompt cache on the machine its
// session picks; without one the turns land anywhere and most of a long
// prompt is read uncached. The session is derived from the client's
// prompt_cache_key - the key the core's sticky rule already binds the
// conversation to one account by - mixed with the API key, so two keys
// sending the same prompt_cache_key never share an upstream session and the
// client's value is not echoed. A request without the key gets none.
func sessionHeaders(h map[string]string, apiKeyID int64, fields map[string]string) {
	var key string
	if json.Unmarshal([]byte(fields[FieldPromptCacheKey]), &key) != nil || key == "" {
		return
	}
	id := upstreamSessionID(apiKeyID, key)
	h["session_id"] = id
	h["conversation_id"] = id
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

func set(path, valueJSON string) *pluginv1.BodyPatch {
	return &pluginv1.BodyPatch{Op: pluginv1.BodyPatch_OP_SET, Path: path, ValueJson: valueJSON}
}

func del(path string) *pluginv1.BodyPatch {
	return &pluginv1.BodyPatch{Op: pluginv1.BodyPatch_OP_DELETE, Path: path}
}

// userMessage is a Responses input array holding one user text message.
func userMessage(text string) string {
	b, _ := json.Marshal([]map[string]any{{
		"type": "message", "role": "user",
		"content": []map[string]string{{"type": "input_text", "text": text}},
	}})
	return string(b)
}

// BuildUpstreamRequest implements pluginsdk.Platform.
//
// HTTP (openai.responses): POST to the Codex backend's Responses endpoint
// with the body patches of bodyPatches. stream is not touched: the upstream
// only streams, and the core turns a non-streaming request into a
// streaming one for account types that declare streamOnly (CONTRACTS §83).
//
// WebSocket (openai.responses_ws): GET on the wss URL with the beta header.
// The core calls this for every turn's response.create message (the first
// call also opens the connection, CONTRACTS §83.6), so the same patches are
// returned for that message, without the stream fields.
func (p *Plugin) BuildUpstreamRequest(_ context.Context, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	c, err := accountCredentials(in.GetAccount())
	if err != nil {
		return nil, err
	}
	resp := &pluginv1.BuildUpstreamRequestResponse{
		Method:        http.MethodPost,
		Url:           p.responsesURL(),
		Headers:       upstreamHeaders(c),
		UpstreamModel: pluginsdk.RequestModel(in),
	}
	ws := false
	switch proto := in.GetMeta().GetProtocol(); proto {
	case ProtocolResponsesWS:
		if err := webSocketRequest(resp); err != nil {
			return nil, err
		}
		ws = true
	case ProtocolResponses:
		resp.Headers["accept"] = "text/event-stream"
		resp.Headers["content-type"] = "application/json"
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported upstream protocol %q", proto)
	}
	sessionHeaders(resp.Headers, in.GetMeta().GetApiKeyId(), in.GetFields())
	resp.Patches = bodyPatches(ws, in.GetFields())
	return resp, nil
}

// bodyPatches make a Responses body (an HTTP request or a WebSocket
// response.create message) one the Codex backend accepts:
//
//   - store=false (stored responses are refused);
//   - a string input becomes an array holding one user message (an array
//     is required; a blank (whitespace-only) string becomes []);
//   - input items with role "system" get role "developer" (instructions or
//     developer messages carry the system prompt);
//   - instructions "" when the body has none (the field is required);
//   - the fields the upstream refuses are removed (removedFields).
func bodyPatches(ws bool, fields map[string]string) []*pluginv1.BodyPatch {
	patches := []*pluginv1.BodyPatch{set("store", "false")}
	if raw, ok := fields[FieldStringInput]; ok && raw != "" {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			if strings.TrimSpace(s) == "" {
				patches = append(patches, set("input", "[]"))
			} else {
				patches = append(patches, set("input", userMessage(s)))
			}
		}
	}
	patches = append(patches, systemToDeveloper(fields[FieldSystemRoles])...)
	if fields[FieldHasInstructions] == "0" {
		patches = append(patches, set("instructions", `""`))
	}
	for _, f := range removedFields(ws) {
		patches = append(patches, del(f))
	}
	return patches
}

// systemToDeveloper turns the FieldSystemRoles value into input.<i>.role
// patches, one per system item, at most maxRoleRewrites.
func systemToDeveloper(raw string) []*pluginv1.BodyPatch {
	if raw == "" {
		return nil
	}
	var items [][]string
	if json.Unmarshal([]byte(raw), &items) != nil {
		return nil
	}
	var out []*pluginv1.BodyPatch
	for i, roles := range items {
		if len(roles) == 1 && roles[0] == "system" {
			out = append(out, set("input."+strconv.Itoa(i)+".role", `"developer"`))
			if len(out) == maxRoleRewrites {
				break
			}
		}
	}
	return out
}

// webSocketRequest turns the HTTP request into the WebSocket handshake.
func webSocketRequest(resp *pluginv1.BuildUpstreamRequestResponse) error {
	switch {
	case strings.HasPrefix(resp.Url, "https://"):
		resp.Url = "wss://" + strings.TrimPrefix(resp.Url, "https://")
	case strings.HasPrefix(resp.Url, "http://"):
		resp.Url = "ws://" + strings.TrimPrefix(resp.Url, "http://")
	default:
		return status.Errorf(codes.Internal, "upstream URL %q has no http(s) scheme", resp.Url)
	}
	resp.Method = http.MethodGet
	resp.Headers["openai-beta"] = WebSocketBeta
	return nil
}

// Execute implements pluginsdk.Executor with the shared SDK flow: build the
// upstream request, forward it through the host and let the host record the
// usage with the declarative rules.
func (p *Plugin) Execute(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
	return pluginsdk.ExecuteDefault(ctx, p, in)
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
	h := upstreamHeaders(c)
	h["accept"] = "text/event-stream"
	h["content-type"] = "application/json"
	return &pluginv1.BuildTestRequestResponse{
		Method:        http.MethodPost,
		Url:           p.responsesURL(),
		Headers:       h,
		BodyJson:      string(b),
		Model:         model,
		UsageProtocol: ProtocolResponses,
	}, nil
}

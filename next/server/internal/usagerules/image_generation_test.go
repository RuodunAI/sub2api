package usagerules

import (
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// The shapes measured on 2026-11-18 (CONTRACTS §120): the Codex backend
// reports the conversation model's usage and, beside it, the image tool's
// under tool_usage.image_gen; the tools echo names the image model.
const (
	imageGenUsage = `{"input_tokens":39,"input_tokens_details":{"text_tokens":39,"image_tokens":0},"output_tokens":515,"output_tokens_details":{"text_tokens":0,"image_tokens":515},"total_tokens":554}`
	zeroGenUsage  = `{"input_tokens":0,"input_tokens_details":{"text_tokens":0,"image_tokens":0},"output_tokens":0,"output_tokens_details":{"text_tokens":0,"image_tokens":0},"total_tokens":0}`
	echoTools     = `[{"type":"function","name":"f"},{"type":"image_generation","model":"gpt-image-2-codex","quality":"auto","size":"auto"}]`
	imageRequest  = `{"model":"gpt-5.6","stream":true,"tools":[{"type":"image_generation","model":"gpt-image-1.5"}]}`
)

func responseObject(tools, toolUsage string) string {
	return `{"id":"resp_1","model":"gpt-5.6","output":[],"tools":` + tools + `,"usage":{"input_tokens":2304,"input_tokens_details":{"cached_tokens":0},"output_tokens":52},"tool_usage":{"image_gen":` + toolUsage + `}}`
}

var wantImage = core.AdditionalUsage{Kind: "image_generation", Model: "gpt-image-2-codex", UsageSemantics: "inclusive", ResponseModel: true,
	Tokens: core.UsageTokens{Input: 39, Output: 515, ImageOutput: 515}}

func onlyImage(t *testing.T, u *Acc, want core.AdditionalUsage) {
	t.Helper()
	got := u.Additional()
	if u.AdditionalError != "" || len(got) != 1 || got[0].Kind != want.Kind || got[0].Model != want.Model ||
		got[0].Tokens != want.Tokens || got[0].UsageSemantics != want.UsageSemantics || !got[0].ResponseModel {
		t.Fatalf("additional = %+v (error %q), want %+v", got, u.AdditionalError, want)
	}
	if main := u.Tokens(); main != (core.UsageTokens{Input: 2304, Output: 52}) {
		t.Fatalf("conversation tokens = %+v", main)
	}
}

// A Codex backend stream: the tools echo and tool_usage come in the
// terminal event, whose output is [] (the image is in output_item.done).
func TestImageGenerationCodexStream(t *testing.T) {
	u := New(imageRules(t, "responses")).WithRequest([]byte(imageRequest))
	created := `{"type":"response.created","response":{"id":"resp_1","tools":` + echoTools + `,"usage":null,"tool_usage":{"image_gen":` + zeroGenUsage + `}}}`
	u.ApplySSE("response.created", []byte(created))
	if len(u.Additional()) != 0 {
		t.Fatal("an all-zero tool usage is usage")
	}
	u.ApplySSE("response.output_item.done", []byte(`{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"aGk="}}`))
	u.ApplySSE("response.completed", []byte(`{"type":"response.completed","response":`+responseObject(echoTools, imageGenUsage)+`}`))
	onlyImage(t, u, wantImage)
}

// An API key account's JSON answer, and the JSON a stream-only account's
// forced stream is folded into (the terminal event's response object).
func TestImageGenerationJSON(t *testing.T) {
	u := New(imageRules(t, "responses")).WithRequest([]byte(imageRequest))
	u.ApplyJSON([]byte(responseObject(echoTools, imageGenUsage)))
	onlyImage(t, u, wantImage)
}

// A WebSocket turn: unnamed events, the same paths as the SSE stream.
func TestImageGenerationWebSocketTurn(t *testing.T) {
	u := New(imageRules(t, "responses_websocket")).WithRequest([]byte(`{"type":"response.create","model":"gpt-5.6","tools":[{"type":"image_generation"}]}`))
	u.ApplySSE("", []byte(`{"type":"response.completed","response":`+responseObject(echoTools, imageGenUsage)+`}`))
	onlyImage(t, u, wantImage)
}

// The model comes from the echo, else the request as sent upstream, else the
// rule's default.
func TestImageGenerationModelSources(t *testing.T) {
	noModelEcho := `[{"type":"image_generation","quality":"auto"}]`
	for name, tc := range map[string]struct{ tools, request, want string }{
		"echo":    {echoTools, imageRequest, "gpt-image-2-codex"},
		"request": {noModelEcho, imageRequest, "gpt-image-1.5"},
		"default": {noModelEcho, `{"model":"gpt-5.6","tools":[{"type":"image_generation"}]}`, "gpt-image-2"},
		"no echo": {`[]`, `{"model":"gpt-5.6"}`, "gpt-image-2"},
	} {
		for _, stream := range []bool{false, true} {
			u := New(imageRules(t, "responses")).WithRequest([]byte(tc.request))
			if stream {
				u.ApplySSE("response.completed", []byte(`{"type":"response.completed","response":`+responseObject(tc.tools, imageGenUsage)+`}`))
			} else {
				u.ApplyJSON([]byte(responseObject(tc.tools, imageGenUsage)))
			}
			want := wantImage
			want.Model = tc.want
			t.Run(name, func(t *testing.T) { onlyImage(t, u, want) })
		}
	}
}

// No tool usage, or one that is all 0, is no additional usage: an ordinary
// conversation carries no zero-priced line.
func TestImageGenerationAbsentOrZero(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5.6","usage":{"input_tokens":2304,"output_tokens":52}}`,
		`{"model":"gpt-5.6","usage":{"input_tokens":2304,"output_tokens":52},"tool_usage":null}`,
		`{"model":"gpt-5.6","usage":{"input_tokens":2304,"output_tokens":52},"tool_usage":{"image_gen":null}}`,
		`{"model":"gpt-5.6","usage":{"input_tokens":2304,"output_tokens":52},"tool_usage":{"image_gen":` + zeroGenUsage + `}}`,
		`{"model":"gpt-5.6","usage":{"input_tokens":2304,"output_tokens":52},"tool_usage":{"image_gen":{}}}`,
	} {
		u := New(imageRules(t, "responses"))
		u.ApplyJSON([]byte(body))
		if got := u.Additional(); len(got) != 0 || u.AdditionalError != "" {
			t.Fatalf("%s: additional = %+v, error %q", body, got, u.AdditionalError)
		}
	}
}

// A malformed object or model blocks the record's billing, as a malformed
// array entry does.
func TestImageGenerationMalformed(t *testing.T) {
	for _, body := range []string{
		responseObject(echoTools, `[1]`),
		responseObject(echoTools, `{"input_tokens":-1,"output_tokens":5}`),
		responseObject(echoTools, `{"input_tokens":"39","output_tokens":5}`),
		responseObject(`[{"type":"image_generation","model":7}]`, imageGenUsage),
	} {
		u := New(imageRules(t, "responses"))
		u.ApplyJSON([]byte(body))
		if !strings.HasPrefix(u.AdditionalError, "additional usage image_generation:") {
			t.Fatalf("%s: error %q", body, u.AdditionalError)
		}
	}
}

package usagerules

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/platforms"
)

// builtinRules returns the rules the built-in platforms declare for protocol.
func builtinRules(t *testing.T, protocol string) manifest.UsageRules {
	t.Helper()
	for _, pf := range platforms.Builtin() {
		for i := range pf.Endpoints {
			if pf.Endpoints[i].Protocol == protocol {
				return For(manifest.AccountPlatform{}, &pf.Endpoints[i], &pf, protocol)
			}
		}
	}
	t.Fatalf("no built-in endpoint speaks %s", protocol)
	return manifest.UsageRules{}
}

// A stream is final once the event its rules name for the output count
// reported it - and only then: early counts, unnamed usage chunks and a null
// count do not make it final.
func TestFinal(t *testing.T) {
	type step struct {
		data  string
		final bool
	}
	for protocol, steps := range map[string][]step{
		"anthropic.messages": {
			{`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":5,"output_tokens":1}}}`, false},
			{`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, false},
			{`{"type":"message_delta","usage":{"output_tokens":9}}`, true},
			{`{"type":"message_stop"}`, true},
		},
		"openai.responses": {
			{`{"type":"response.created","response":{"usage":null}}`, false},
			{`{"type":"response.output_text.delta","delta":"hi"}`, false},
			{`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":9}}}`, true},
		},
		"openai.chat": {
			{`{"choices":[{"delta":{"content":"hi"}}],"usage":null}`, false},
			{`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":9}}`, false},
		},
		"gemini.stream_generate": {
			{`{"candidates":[{}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`, false},
			{`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":9}}`, false},
		},
	} {
		u := New(builtinRules(t, protocol))
		for i, s := range steps {
			u.ApplySSE("", []byte(s.data))
			if u.Final() != s.final {
				t.Errorf("%s event %d: final = %v, want %v", protocol, i, u.Final(), s.final)
			}
		}
	}

	nullCount := New(manifest.UsageRules{SSE: []manifest.SSEUsageMap{{Event: "done", Map: map[string]string{manifest.UsageOutputTokens: "usage.out"}}}})
	nullCount.ApplySSE("done", []byte(`{"usage":{"out":null}}`))
	if nullCount.Final() {
		t.Error("a null output count made the stream final")
	}
}

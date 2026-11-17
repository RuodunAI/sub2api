package pluginsdk

import (
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// TestRequestModel: the body field wins over meta.model, and anything that is
// not a non-empty JSON string is ignored (the plugin then sends meta.model).
// The four Anthropic/OpenAI-compatible plugins all relied on this precedence
// before the helper existed.
func TestRequestModel(t *testing.T) {
	base := func(fields map[string]string, model string) *pluginv1.BuildUpstreamRequestRequest {
		return &pluginv1.BuildUpstreamRequestRequest{
			Meta:   &pluginv1.RequestMeta{Model: model},
			Fields: fields,
		}
	}
	for _, tc := range []struct {
		name   string
		in     *pluginv1.BuildUpstreamRequestRequest
		wanted string
	}{
		{"no fields", base(nil, "meta-model"), "meta-model"},
		{"body wins", base(map[string]string{"model": `"body-model"`}, "meta-model"), "body-model"},
		{"empty body value", base(map[string]string{"model": `""`}, "meta-model"), "meta-model"},
		{"empty raw", base(map[string]string{"model": ""}, "meta-model"), "meta-model"},
		{"not a string", base(map[string]string{"model": `42`}, "meta-model"), "meta-model"},
		{"invalid json", base(map[string]string{"model": `{"a":1}`}, "meta-model"), "meta-model"},
		{"other fields only", base(map[string]string{"stream": `true`}, "meta-model"), "meta-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestModel(tc.in); got != tc.wanted {
				t.Fatalf("RequestModel = %q, want %q", got, tc.wanted)
			}
		})
	}
	if got := RequestModel(&pluginv1.BuildUpstreamRequestRequest{}); got != "" {
		t.Fatalf("empty request = %q, want empty", got)
	}
}

func TestPatches(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{true, "true"},
		{"bedrock-2023-05-31", `"bedrock-2023-05-31"`},
		{[]string{"a", "b"}, `["a","b"]`},
		{nil, "null"},
		{func() {}, "null"},
	} {
		p := SetPatch("a.b", tc.value)
		if p.GetOp() != pluginv1.BodyPatch_OP_SET || p.GetPath() != "a.b" || p.GetValueJson() != tc.want {
			t.Errorf("SetPatch(%v) = %v", tc.value, p)
		}
	}
	if p := DeletePatch("model"); p.GetOp() != pluginv1.BodyPatch_OP_DELETE || p.GetPath() != "model" || p.GetValueJson() != "" {
		t.Errorf("DeletePatch = %v", p)
	}
}

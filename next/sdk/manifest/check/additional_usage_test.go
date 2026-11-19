package check

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"testing"
)

func TestAdditionalUsageManifestContract(t *testing.T) {
	p := validPlatform()
	r := manifest.AdditionalUsageRule{Name: "advisor", JSONPath: "usage.iterations", SSEPath: "usage.iterations", SSEEvent: "message_delta", TypePath: "type", TypeValue: "advisor_message", ModelPath: "model", Semantics: "exclusive", Map: map[string]string{"input_tokens": "input_tokens", "output_tokens": "output_tokens"}}
	p.Usage.Additional = []manifest.AdditionalUsageRule{r}
	if got := platformCodes(p); len(got) > 0 {
		t.Fatal(got)
	}
	for _, mutate := range []func(*manifest.AdditionalUsageRule){func(r *manifest.AdditionalUsageRule) { r.Name = "" }, func(r *manifest.AdditionalUsageRule) { r.ModelPath = "" }, func(r *manifest.AdditionalUsageRule) { r.Semantics = "" }, func(r *manifest.AdditionalUsageRule) {
		r.Map = map[string]string{"input_tokens": "input+cache", "output_tokens": "out"}
	}} {
		copy := r
		mutate(&copy)
		p.Usage.Additional = []manifest.AdditionalUsageRule{copy}
		if len(platformCodes(p)) == 0 {
			t.Fatal("invalid additional usage accepted")
		}
	}
	p.Usage.Additional = []manifest.AdditionalUsageRule{r, r}
	if len(platformCodes(p)) == 0 {
		t.Fatal("duplicate counters accepted")
	}
	r.UsePrimaryModel = true
	r.ModelPath = ""
	r.RequiredBy = []string{"operation.options"}
	p.Usage.Additional = []manifest.AdditionalUsageRule{r}
	if codes := platformCodes(p); len(codes) > 0 {
		t.Fatal("host-primary rule rejected", codes)
	}
	r.RequiredBy = []string{"operation.#"}
	p.Usage.Additional = []manifest.AdditionalUsageRule{r}
	if len(platformCodes(p)) == 0 {
		t.Fatal("requirement expression accepted")
	}
}

// TestObjectAdditionalUsageRule pins the object form (CONTRACTS §120): one
// usage object whose model the response, the request or a default names.
func TestObjectAdditionalUsageRule(t *testing.T) {
	p := validPlatform()
	r := manifest.AdditionalUsageRule{Name: "image_generation", Object: true, JSONPath: "tool_usage.image_gen", SSEPath: "response.tool_usage.image_gen",
		Semantics: "inclusive", DefaultModel: "image-default",
		ModelSources: []manifest.UsageModelSource{
			{From: "sse", ArrayPath: "response.tools", Match: map[string]string{"type": "image_generation"}, ModelPath: "model"},
			{From: "json", ArrayPath: "tools", Match: map[string]string{"type": "image_generation"}, ModelPath: "model"},
			{From: "request", ModelPath: "tool_model"},
		},
		Map: map[string]string{"input_tokens": "input_tokens", "output_tokens": "output_tokens",
			"image_input_tokens": "input_tokens_details.image_tokens", "image_output_tokens": "output_tokens_details.image_tokens"}}
	p.Usage.Additional = []manifest.AdditionalUsageRule{r}
	if got := platformCodes(p); len(got) > 0 {
		t.Fatal(got)
	}
	for name, mutate := range map[string]func(*manifest.AdditionalUsageRule){
		"no model":         func(r *manifest.AdditionalUsageRule) { r.ModelSources, r.DefaultModel = nil, "" },
		"type selector":    func(r *manifest.AdditionalUsageRule) { r.TypePath, r.TypeValue = "type", "x" },
		"entry model path": func(r *manifest.AdditionalUsageRule) { r.ModelPath = "model" },
		"primary model":    func(r *manifest.AdditionalUsageRule) { r.UsePrimaryModel = true },
		"bad from": func(r *manifest.AdditionalUsageRule) {
			r.ModelSources = []manifest.UsageModelSource{{From: "header", ModelPath: "m"}}
		},
		"query model path": func(r *manifest.AdditionalUsageRule) {
			r.ModelSources = []manifest.UsageModelSource{{From: "json", ModelPath: `tools.#(type=="x").model`}}
		},
		"match on root": func(r *manifest.AdditionalUsageRule) {
			r.ModelSources = []manifest.UsageModelSource{{From: "json", Match: map[string]string{"type": "x"}, ModelPath: "m"}}
		},
		"padded default": func(r *manifest.AdditionalUsageRule) { r.DefaultModel = " m" },
		"summed object":  func(r *manifest.AdditionalUsageRule) { r.JSONPath = "a+b" },
		"sources on arrays": func(r *manifest.AdditionalUsageRule) {
			r.Object = false
			r.TypePath, r.TypeValue, r.ModelPath = "type", "x", "model"
		},
	} {
		copy := r
		mutate(&copy)
		p.Usage.Additional = []manifest.AdditionalUsageRule{copy}
		if len(platformCodes(p)) == 0 {
			t.Fatalf("%s: invalid object rule accepted", name)
		}
	}
	p.Usage.Additional = []manifest.AdditionalUsageRule{r, func() manifest.AdditionalUsageRule { d := r; d.Name = "other"; return d }()}
	if len(platformCodes(p)) == 0 {
		t.Fatal("the same usage object billed twice accepted")
	}
}

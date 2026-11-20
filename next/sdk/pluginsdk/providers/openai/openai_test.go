package openai

import "testing"

// The chat test asks for one token in the field the model takes: OpenAI
// refuses max_tokens for the o series and gpt-5 onward, every other model
// (and every OpenAI-compatible upstream) keeps max_tokens.
func TestChatPingBodyLimit(t *testing.T) {
	for model, limit := range map[string]string{
		"gpt-4o-mini": "max_tokens", "gpt-4.1": "max_tokens", "gpt-image-1": "max_tokens", "gpt-oss-120b": "max_tokens",
		"golden-model": "max_tokens", "doubao-seed-1-6": "max_tokens", "omni-moderation-latest": "max_tokens",
		"o1": "max_completion_tokens", "o3-mini": "max_completion_tokens", "o4-mini": "max_completion_tokens",
		"gpt-5": "max_completion_tokens", "gpt-5.4": "max_completion_tokens", "gpt-5-mini": "max_completion_tokens",
		"gpt-6.2-astra": "max_completion_tokens", "gpt-10": "max_completion_tokens",
	} {
		want := `{"` + limit + `":1,"messages":[{"content":"ping","role":"user"}],"model":"` + model + `"}`
		if got := ChatPingBody(model, 1); got != want {
			t.Errorf("%s: %s, want %s", model, got, want)
		}
	}
}

package openai

import "testing"

// The chat test asks for one token in max_tokens, except from the models
// OpenAI refuses max_tokens for (the o series and gpt-5 onward): they reason
// within the limit and get max_completion_tokens 16.
func TestChatPingBodyLimit(t *testing.T) {
	const tok, reason = `"max_tokens":1`, `"max_completion_tokens":16`
	for model, limit := range map[string]string{
		"gpt-4o-mini": tok, "gpt-4.1": tok, "gpt-image-1": tok, "gpt-oss-120b": tok,
		"golden-model": tok, "deepseek-chat": tok, "omni-moderation-latest": tok,
		"o1": reason, "o3-mini": reason, "o4-mini": reason,
		"gpt-5": reason, "gpt-5.4": reason, "gpt-5-mini": reason,
		"gpt-6.2-astra": reason, "gpt-10": reason,
	} {
		want := `{` + limit + `,"messages":[{"content":"ping","role":"user"}],"model":"` + model + `"}`
		if got := ChatPingBody(model, 1); got != want {
			t.Errorf("%s: %s, want %s", model, got, want)
		}
	}
}

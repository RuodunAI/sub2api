package bedrock

import (
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// What Claude Code sends Anthropic and InvokeModel refuses, deep inside a
// Messages body (the previous implementation's Bedrock rules,
// backend/internal/service/bedrock_request.go):
//
//   - "custom" in a tool definition ({"defer_loading": true}): "Extra
//     inputs are not permitted";
//   - "scope" in a cache_control, and "ttl" unless the model takes it;
//   - a tool use id with characters outside [a-zA-Z0-9_-];
//   - a thinking setting the model does not take.

// defaultThinkingBudget is the budget_tokens of an "enabled" thinking
// setting that names none: InvokeModel requires one.
const defaultThinkingBudget = "10000"

// cacheTTLs are the cache_control ttl values InvokeModel takes.
var cacheTTLs = []string{"5m", "1h"}

// idChar keeps the characters InvokeModel takes in a tool use id
// ([a-zA-Z0-9_-]) and maps the others to "_" (for strings.Map).
func idChar(r rune) rune {
	if r == '_' || r == '-' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' {
		return r
	}
	return '_'
}

// sanitize adds the edits that clean doc, a Messages body, for model m.
func sanitize(e *edits, doc gjson.Result, m claudeModel) {
	eachElement(doc.Get("tools"), func(tool gjson.Result) {
		e.object(tool, isKey("custom"))
		cacheControl(e, tool, m)
	})
	eachElement(doc.Get("system"), func(block gjson.Result) { cacheControl(e, block, m) })
	eachElement(doc.Get("messages"), func(message gjson.Result) {
		eachElement(message.Get("content"), func(block gjson.Result) {
			cacheControl(e, block, m)
			toolUseID(e, block)
		})
	})
	thinking(e, doc.Get("thinking"), m)
}

// cacheControl removes from the cache_control of block what InvokeModel
// refuses: scope always, ttl unless the model takes it with that value.
func cacheControl(e *edits, block gjson.Result, m claudeModel) {
	e.object(block.Get("cache_control"), func(key string, value gjson.Result) bool {
		return key == "scope" || key == "ttl" && !(m.hasCacheTTL() && value.Type == gjson.String && slices.Contains(cacheTTLs, value.Str))
	})
}

// toolUseID replaces the characters InvokeModel refuses in the id of a
// tool_use block and in the tool_use_id of a tool_result block, the same
// way in both, so the pair still matches.
func toolUseID(e *edits, block gjson.Result) {
	var id gjson.Result
	switch block.Get("type").String() {
	case "tool_use":
		id = block.Get("id")
	case "tool_result":
		id = block.Get("tool_use_id")
	}
	if id.Type != gjson.String {
		return
	}
	if clean := strings.Map(idChar, id.Str); clean != id.Str {
		e.replace(id, []byte(`"`+clean+`"`)) // clean holds no character JSON escapes
	}
}

// thinking fits the thinking setting to the model: a model that thinks
// adaptively only gets "adaptive" without a budget, and an "enabled"
// setting without a budget gets the default one.
func thinking(e *edits, setting gjson.Result, m claudeModel) {
	switch typ := setting.Get("type").String(); {
	case m.adaptiveThinkingOnly() && (typ == "enabled" || typ == "adaptive"):
		e.object(setting, isKey("budget_tokens"), member{"type", []byte(`"adaptive"`)})
	case typ == "enabled" && !setting.Get("budget_tokens").Exists():
		e.object(setting, nil, member{"budget_tokens", []byte(defaultThinkingBudget)})
	}
}

// isKey is a drop rule removing the members named keys.
func isKey(keys ...string) func(string, gjson.Result) bool {
	return func(key string, _ gjson.Result) bool { return slices.Contains(keys, key) }
}

// eachElement calls fn with the elements of an array (nothing for any other
// value, where gjson's ForEach would call it with the value itself).
func eachElement(array gjson.Result, fn func(gjson.Result)) {
	if !array.IsArray() {
		return
	}
	array.ForEach(func(_, v gjson.Result) bool {
		fn(v)
		return true
	})
}

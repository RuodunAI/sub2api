package paramoverride

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// The header override of an account (CONTRACTS §108) has the format of a
// new-api channel's header_override: an object of header name → value. A
// value is a fixed text, {client_header:<name>} (the client's header, not
// set when the client sent none), {client_header:<name>:<append>} (the
// client's value and append, comma-separated, or append alone) or {unset}
// (remove the header); an empty value is ignored. The keys "*" and
// "re:<pattern>" / "regex:<pattern>" are passthrough rules (§81) whose
// values are ignored; a blank key is ignored too. Unlike new-api, no blocked header (HeaderBlocked) may
// be set, removed or read, and {api_key} is not supported: an account's
// credential is set by its type's auth only.

// MaxHeaderOverrideEntries caps the keys of one header override.
const MaxHeaderOverrideEntries = 100

const (
	headerUnset        = "{unset}"
	clientHeaderPrefix = "{client_header:"
	apiKeyPlaceholder  = "{api_key}"
)

// HeaderOverrideRules is a compiled header override. nil means none.
type HeaderOverrideRules struct {
	raw         json.RawMessage
	passthrough []string
	headers     map[string]headerRule // lower-case name → rule
	// broken is set on a stored override that no longer compiles: every
	// Resolve fails rather than sending the request without it.
	broken string
}

// headerRule is the value of one header: value, or with client the
// {client_header} placeholder whose append value is value; or unset.
type headerRule struct {
	value, client string
	unset         bool
}

// Raw is the header override as saved (compact JSON); null when there is none.
func (m *HeaderOverrideRules) Raw() json.RawMessage {
	if m == nil {
		return json.RawMessage("null")
	}
	return m.raw
}

// Passthrough are its passthrough rules, normalized like an account's
// header_passthrough (NormalizePassthrough), for PassthroughHeaders.
func (m *HeaderOverrideRules) Passthrough() []string {
	if m == nil || m.broken != "" {
		return nil
	}
	return m.passthrough
}

// Resolve decides the headers for a request from the client headers: the
// headers to set (lower-case name → value) and to remove. A client value
// that cannot be sent (a control character) is an error, not a header.
func (m *HeaderOverrideRules) Resolve(client http.Header) (HeaderOverride, error) {
	if m == nil {
		return HeaderOverride{}, nil
	}
	if m.broken != "" {
		return HeaderOverride{}, errors.New(m.broken)
	}
	out := HeaderOverride{Set: map[string]string{}}
	for name, rule := range m.headers {
		v, ok := rule.resolve(client)
		switch {
		case rule.unset:
			out.Delete = append(out.Delete, name)
		case !ok:
		case !ValidHeaderValue(v):
			return HeaderOverride{}, fmt.Errorf("header %s: the client's %s contains a control character", name, rule.client)
		default:
			out.Set[name] = v
		}
	}
	sort.Strings(out.Delete)
	return out, nil
}

func (h headerRule) resolve(client http.Header) (string, bool) {
	if h.client == "" {
		return h.value, true
	}
	v := headerValue(client, h.client)
	switch {
	case v == "":
		return h.value, h.value != ""
	case h.value == "":
		return v, true
	}
	return v + "," + h.value, true
}

// headerValue is the value of a client header, its values comma-separated.
func headerValue(h http.Header, name string) string {
	return strings.TrimSpace(strings.Join(h.Values(name), ", "))
}

// LoadHeaderOverride compiles a stored header override. One that no longer
// compiles (it was validated when saved) fails every request of the account.
func LoadHeaderOverride(raw []byte) *HeaderOverrideRules {
	m, errs := CompileHeaderOverride("header_override", raw, false)
	if len(errs) > 0 {
		return &HeaderOverrideRules{raw: raw, broken: "stored header override is invalid: " + errs[0].Message}
	}
	return m
}

// CompileHeaderOverride validates and compiles a header override. null and
// {} compile to nil (none). Errors are field errors on field whose messages
// name the key, e.g. "User-Agent: must be a string"; zh picks the Chinese
// wording.
func CompileHeaderOverride(field string, raw []byte, zh bool) (*HeaderOverrideRules, []FieldError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	c := &overrideCompiler{zh: zh, field: field}
	root, ok := c.object(raw, "the header override", "请求头覆盖")
	if !ok {
		return nil, c.errs
	}
	m := &HeaderOverrideRules{headers: map[string]headerRule{}}
	var rules []string
	keys := 0
	root.ForEach(func(k, v gjson.Result) bool {
		keys++
		if strings.TrimSpace(k.Str) == "" {
			return true // ignored, as by new-api
		} else if passthroughKey(k.Str) {
			rules = append(rules, k.Str)
		} else if name, rule, ok := c.compileHeaderRule(k.Str, v); ok {
			m.headers[name] = rule // a later spelling of a name wins
		}
		return true
	})
	if keys > MaxHeaderOverrideEntries {
		c.fail("too_many", "", "at most 100 entries", "最多 100 项")
	}
	var errs []FieldError
	m.passthrough, errs = NormalizePassthrough(field, rules, zh)
	if c.errs = append(c.errs, errs...); len(c.errs) > 0 {
		return nil, c.errs
	}
	if keys == 0 {
		return nil, nil
	}
	m.raw = compact(raw)
	return m, nil
}

// passthroughKey reports whether a key is a passthrough rule: "*" or a regex.
func passthroughKey(key string) bool {
	key = strings.TrimSpace(key)
	_, regex := passthroughPattern(key)
	return key == passthroughAll || regex
}

// compileHeaderRule compiles the value of the header key; ok is false for
// an empty value, which is ignored.
func (c *overrideCompiler) compileHeaderRule(key string, v gjson.Result) (string, headerRule, bool) {
	name := c.header(key, key)
	if v.Type != gjson.String {
		c.fail("invalid", key, "must be a string", "必须是字符串")
		return name, headerRule{}, false
	}
	value := strings.TrimSpace(v.Str)
	switch {
	case value == "":
		return name, headerRule{}, false
	case !ValidHeaderValue(value):
		c.fail("invalid", key, controlCharsEn, controlCharsZh)
	case strings.Contains(value, apiKeyPlaceholder):
		c.fail("unsupported", key, "{api_key} is not supported: the credential is set by the account type's auth",
			"不支持 {api_key}：凭证由账号类型的鉴权设置")
	case value == headerUnset:
		return name, headerRule{unset: true}, true
	case strings.HasPrefix(value, clientHeaderPrefix) && strings.IndexByte(value, '}') == len(value)-1:
		return name, c.clientHeader(key, value[len(clientHeaderPrefix):len(value)-1]), true
	case strings.Contains(value, clientHeaderPrefix) || strings.Contains(value, headerUnset):
		c.fail("invalid", key, "a placeholder must be the whole value", "占位符必须是整个值")
	}
	return name, headerRule{value: value}, true
}

// clientHeader compiles the inside of {client_header:<name>[:<append>]};
// a header name has no ":", so the first one ends it.
func (c *overrideCompiler) clientHeader(key, inner string) headerRule {
	name, appendValue, hasAppend := strings.Cut(inner, ":")
	rule := headerRule{value: strings.TrimSpace(appendValue)}
	if strings.TrimSpace(name) == "" {
		c.fail("required", key, "client_header needs a header name", "client_header 缺少请求头名称")
		return rule
	}
	rule.client = c.header(key, name)
	if hasAppend && rule.value == "" {
		c.fail("invalid", key, "the client_header append value must not be empty", "client_header 的追加值不能为空")
	}
	return rule
}

package paramoverride

import (
	"net/http"
	"strings"
	"testing"
)

func compileHeaders(t *testing.T, raw string) *HeaderOverrideRules {
	t.Helper()
	m, errs := CompileHeaderOverride("header_override", []byte(raw), false)
	requireEmpty(t, errs)
	return m
}

func TestHeaderOverrideResolve(t *testing.T) {
	m := compileHeaders(t, `{"*":"","re:^x-claude-code-":"","User-Agent":" my-client/1.0 ","anthropic-beta":"{client_header:anthropic-beta:context-1m-2025-08-07}",
		"x-extra":"{client_header:x-missing:only}","x-trace-id":"{client_header:x-request-id}","x-none":"{client_header:x-missing}",
		"x-unwanted":"{unset}","x-empty":"","X-Dup":"a","x-dup":"b"}`)
	assertEqual(t, []string{"*", "regex:^x-claude-code-"}, m.Passthrough())

	client := http.Header{"Anthropic-Beta": {"a", "b"}, "X-Request-Id": {"r1"}}
	out, err := m.Resolve(client)
	requireNoError(t, err)
	assertEqual(t, map[string]string{"user-agent": "my-client/1.0", "anthropic-beta": "a, b,context-1m-2025-08-07",
		"x-extra": "only", "x-trace-id": "r1", "x-dup": "b"}, out.Set)
	assertEqual(t, []string{"x-unwanted"}, out.Delete)

	out, err = m.Resolve(nil)
	requireNoError(t, err)
	assertEqual(t, "context-1m-2025-08-07", out.Set["anthropic-beta"])
	_, set := out.Set["x-trace-id"]
	assertTrue(t, !set)

	var none *HeaderOverrideRules
	out, err = none.Resolve(client)
	requireNoError(t, err)
	assertEmpty(t, out.Set)
	assertNil(t, none.Passthrough())
	assertEqual(t, "null", string(none.Raw()))
}

func TestHeaderOverrideCompile(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string // code: message prefix
	}{
		{"not an object", `["x"]`, []string{"invalid: must be a JSON object"}},
		{"value types", `{"x-a":1,"x-b":null}`, []string{"invalid: x-a: must be a string", "invalid: x-b: must be a string"}},
		{"names", `{"x a":"v","Authorization":"Bearer x","x-api-key":"{unset}","Host":"h","x-sub2api-node":"n"}`,
			[]string{"invalid: x a: invalid header name", "blocked: Authorization: header authorization", "blocked: x-api-key", "blocked: Host", "blocked: x-sub2api-node"}},
		{"api key", `{"x-key":"Bearer {api_key}"}`, []string{"unsupported: x-key: {api_key} is not supported"}},
		{"control characters", `{"x-a":"v\r\nx-b: v","x-b":"a\u0001b","x-c":"a\u007fb","x-d":"{client_header:x-e:a\u0000}","x-tab":"a\tb"}`,
			[]string{"invalid: x-a: must not contain control characters", "invalid: x-b: must not", "invalid: x-c: must not", "invalid: x-d: must not"}},
		{"placeholders", `{"x-a":"pre {client_header:x-b}","x-c":"{unset} x","x-d":"{client_header:}","x-e":"{client_header:x-f:}","x-g":"{client_header:cookie}",
			"x-h":"{client_header:x i}","x-j":"{client_header:x-k}}"}`,
			[]string{"invalid: x-a: a placeholder must be the whole value", "invalid: x-c: a placeholder", "required: x-d: client_header needs a header name",
				"invalid: x-e: the client_header append value", "blocked: x-g: header cookie", "invalid: x-h: invalid header name", "invalid: x-j: a placeholder"}},
		{"passthrough", `{"re:(":"","regex:":""}`, []string{"invalid: invalid header passthrough regex re:(", "invalid: header passthrough regex is empty"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, errs := CompileHeaderOverride("header_override", []byte(tc.raw), false)
			assertNil(t, m)
			requireLen(t, errs, len(tc.want), "%v", errs)
			for i, e := range errs {
				assertEqual(t, "header_override", e.Field)
				assertTrue(t, strings.HasPrefix(e.Code+": "+e.Message, tc.want[i]), "%s: %s, want %s", e.Code, e.Message, tc.want[i])
			}
		})
	}

	many := `{` + strings.TrimSuffix(strings.Repeat(`"x-a":"v",`, MaxHeaderOverrideEntries+1), ",") + `}`
	_, errs := CompileHeaderOverride("header_override", []byte(many), false)
	requireLen(t, errs, 1)
	assertEqual(t, "too_many", errs[0].Code)
	_, errs = CompileHeaderOverride("header_override", []byte(`{"x-a":"`+strings.Repeat("x", MaxBytes)+`"}`), false)
	requireLen(t, errs, 1)
	assertEqual(t, "too_large", errs[0].Code)
	_, errs = CompileHeaderOverride("header_override", []byte(`{"x-key":"{api_key}"}`), true)
	requireLen(t, errs, 1)
	assertEqual(t, "x-key: 不支持 {api_key}：凭证由账号类型的鉴权设置", errs[0].Message)

	for _, raw := range []string{``, `null`, `{}`} {
		m, errs := CompileHeaderOverride("header_override", []byte(raw), false)
		assertNil(t, m, raw)
		assertEmpty(t, errs, raw)
	}
	assertEqual(t, `{"x-a":"v","*":""}`, string(compileHeaders(t, `{ "x-a": "v", "*": "" }`).Raw()))
	// Blank keys are ignored, as by new-api.
	out, err := compileHeaders(t, `{"":"x"," ":"{unset}","x-a":"v"}`).Resolve(nil)
	requireNoError(t, err)
	assertEqual(t, HeaderOverride{Set: map[string]string{"x-a": "v"}}, out)

	broken := LoadHeaderOverride([]byte(`{"Authorization":"x","*":""}`))
	_, err = broken.Resolve(http.Header{})
	assertErrorContains(t, err, "stored header override is invalid")
	assertNil(t, broken.Passthrough())
	assertEqual(t, `{"Authorization":"x","*":""}`, string(broken.Raw()))
}

// The header override is the initial header state of the param override:
// its conditions, keep_origin and header reads see it, it may change and
// remove what it set, and the result includes the rest.
func TestHeaderOverrideSeedsParamOverride(t *testing.T) {
	m := compileHeaders(t, `{"x-a":"from-ho","x-b":"b","x-gone":"{unset}","x-trace":"{client_header:x-request-id}"}`)
	client := http.Header{"X-Request-Id": {"r1"}, "X-Gone": {"client"}}
	seed, err := m.Resolve(client)
	requireNoError(t, err)
	in := &Context{Headers: client, InitialHeaders: seed}

	p := compileOverride(t, `{"operations":[
		{"mode":"set_header","path":"x-a","value":"op","keep_origin":true},
		{"mode":"delete_header","path":"x-b"},
		{"mode":"copy_header","from":"x-gone","to":"x-copy"},
		{"mode":"copy_header","from":"x-trace","to":"x-trace-copy"},
		{"mode":"set","path":"seen","value":true,"conditions":[{"path":"header_override.x-a","mode":"full","value":"from-ho"}]},
		{"mode":"sync_fields","from":"header:x-a","to":"json:meta.a"}]}`)
	out, err := p.Apply([]byte(`{}`), in)
	requireNoError(t, err)
	assertJSONEq(t, `{"seen":true,"meta":{"a":"from-ho"}}`, string(out.Body))
	assertEqual(t, map[string]string{"x-a": "from-ho", "x-trace": "r1", "x-trace-copy": "r1"}, out.Headers.Set)
	assertEqual(t, []string{"x-b", "x-gone"}, out.Headers.Delete)
	// The seed is not changed by the run.
	assertEqual(t, "b", seed.Set["x-b"])

	// Without a param override the header override is the result.
	var none *Override
	out, err = none.Apply([]byte(`{}`), in)
	requireNoError(t, err)
	assertEqual(t, seed, out.Headers)
}

// A client value with a control character (not one net/http lets in, but
// a header map is not always from net/http) is an error, not a header.
func TestHeaderOverrideResolveControlCharacters(t *testing.T) {
	_, err := compileHeaders(t, `{"x-a":"{client_header:x-b:tail}"}`).Resolve(http.Header{"X-B": {"a\nb"}})
	assertErrorContains(t, err, "header x-a: the client's x-b contains a control character")
}

package paramoverride

import (
	"net/http"
	"testing"
)

// The forms new-api's param_override has beyond the first engine:
// regex conditions, sync_fields, prune_objects, pass_headers objects and
// the using_group context key.
func TestParamOverrideNewAPIModes(t *testing.T) {
	cases := []struct {
		name, body, override, want string
	}{
		{"regex condition", `{"model":"claude-3-5-sonnet-20241022"}`,
			`{"operations":[{"mode":"set","path":"hit","value":1,"conditions":[{"path":"model","mode":"regex","value":"^claude-.*-\\d{8}$"}]},
			{"mode":"set","path":"miss","value":1,"conditions":[{"path":"model","mode":"regex","value":"^gpt"}]}]}`,
			`{"model":"claude-3-5-sonnet-20241022","hit":1}`},
		{"sync to missing side", `{"a":{"x":1},"c":""}`,
			`{"operations":[{"mode":"sync_fields","from":"json:a","to":"b"},{"mode":"sync_fields","from":"body:d","to":"a.x"},{"mode":"sync_fields","from":"c","to":"e"}]}`,
			`{"a":{"x":1},"b":{"x":1},"c":"","d":1}`},
		{"sync both present", `{"a":1,"b":2}`, `{"operations":[{"mode":"sync_fields","from":"a","to":"b"}]}`, `{"a":1,"b":2}`},
		{"prune by type", `{"tools":[{"type":"web_search"},{"type":"function","f":{"type":"web_search"}},{"type":"web_search"}]}`,
			`{"operations":[{"mode":"prune_objects","path":"tools","value":"web_search"}]}`,
			`{"tools":[{"type":"function"}]}`},
		{"prune not recursive", `{"tools":[{"type":"x"},{"type":"y","inner":{"type":"x"}}]}`,
			`{"operations":[{"mode":"prune_objects","path":"tools","value":{"type":"x","recursive":false}}]}`,
			`{"tools":[{"type":"y","inner":{"type":"x"}}]}`},
		{"prune whole body keeps root", `{"type":"x","m":[{"type":"x","k":1},{"type":"x"}]}`,
			`{"operations":[{"mode":"prune_objects","value":{"type":"x","where":{"k":1}}}]}`,
			`{"type":"x","m":[{"type":"x"}]}`},
		{"prune conditions OR", `{"m":[{"a":1},{"b":2},{"c":3}]}`,
			`{"operations":[{"mode":"prune_objects","path":"m","value":{"logic":"OR","conditions":[{"path":"a","mode":"full","value":1},{"path":"b","mode":"gte","value":2}]}}]}`,
			`{"m":[{"c":3}]}`},
		{"prune falls back to the context", `{"m":[{"a":1},{"a":2}]}`,
			`{"operations":[{"mode":"prune_objects","path":"m","value":{"conditions":{"a":2,"group":"vip"}}}]}`,
			`{"m":[{"a":1}]}`},
		{"prune missing path", `{"a":1}`, `{"operations":[{"mode":"prune_objects","path":"m","value":"x"}]}`, `{"a":1}`},
		{"using_group", `{}`, `{"operations":[{"mode":"set","path":"g","value":1,"conditions":{"using_group":"vip"}}]}`, `{"g":1}`},
	}
	in := &Context{Group: "vip"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := compileOverride(t, tc.override).Apply([]byte(tc.body), in)
			requireNoError(t, err)
			assertJSONEq(t, tc.want, string(out.Body))
		})
	}
}

func TestParamOverrideSyncHeaders(t *testing.T) {
	in := &Context{Headers: http.Header{"X-Session": {"s1"}}}
	out, err := compileOverride(t, `{"operations":[
		{"mode":"sync_fields","from":"header:x-session","to":"json:metadata.session"},
		{"mode":"sync_fields","from":"json:user","to":"header:X-User"},
		{"mode":"sync_fields","from":"json:n","to":"header:x-n"}]}`).Apply([]byte(`{"user":"u1","n":3}`), in)
	requireNoError(t, err)
	assertJSONEq(t, `{"user":"u1","n":3,"metadata":{"session":"s1"}}`, string(out.Body))
	assertEqual(t, map[string]string{"x-user": "u1", "x-n": "3"}, out.Headers.Set)
}

func TestParamOverridePassHeadersForms(t *testing.T) {
	in := &Context{Headers: http.Header{"X-A": {"a"}, "X-B": {"b"}, "X-C": {"c"}}}
	for _, value := range []string{
		`{"headers":["x-a"],"names":"x-b","header":"x-c"}`,
		`"[\"x-a\",\"x-b\",\"x-c\"]"`,
		`"{\"headers\":\"x-a, x-b\",\"names\":[\"x-c\"]}"`,
		`"x-a,x-b,x-c"`,
	} {
		out, err := compileOverride(t, `{"operations":[{"mode":"pass_headers","value":`+value+`}]}`).Apply([]byte(`{}`), in)
		requireNoError(t, err, value)
		assertEqual(t, map[string]string{"x-a": "a", "x-b": "b", "x-c": "c"}, out.Headers.Set, value)
	}
	_, errs := Compile("param_override", []byte(`{"operations":[{"mode":"pass_headers","value":{"other":"x"}},{"mode":"pass_headers","value":{"headers":["cookie"]}}]}`), false)
	requireLen(t, errs, 2, "%v", errs)
	assertEqual(t, "operations[0].value: required", errs[0].Message)
	assertEqual(t, "blocked", errs[1].Code)
}

// Review regressions of prune_objects: wildcard paths, keys with gjson
// path syntax, duplicate keys (every copy goes, as in new-api), trimmed
// type strings and where paths.
func TestParamOverridePrune(t *testing.T) {
	cases := []struct {
		name, body, value, path, want string
	}{
		{"wildcard over members", `{"tools":{"a":[{"type":"x"},{"type":"y"}],"b":[{"type":"x"}]}}`, `"x"`, "tools.*",
			`{"tools":{"a":[{"type":"y"}],"b":[]}}`},
		{"wildcard over elements", `{"messages":[{"content":[{"type":"x"},{"type":"y"}]},{"content":[{"type":"x"}]}]}`, `"x"`, "messages.*.content",
			`{"messages":[{"content":[{"type":"y"}]},{"content":[]}]}`},
		{"keys with path syntax", `{"k#":{"type":"x"},"@m":{"type":"x"},"a|b":{"type":"x"},"q?":{"type":"x"},"a.b":{"type":"x"},"keep":{"type":"y"}}`, `"x"`, "",
			`{"keep":{"type":"y"}}`},
		{"duplicate keys", `{"c":{"type":"keep"},"c":{"type":"x"},"d":1}`, `"x"`, "", `{"d":1}`},
		{"trimmed type", `{"m":[{"type":"x"},{"type":"y"}]}`, `" x "`, "m", `{"m":[{"type":"y"}]}`},
		{"trimmed where", `{"m":[{"k":1},{"k":2}]}`, `{"where":{" k ":1,"  ":2}}`, "m", `{"m":[{"k":2}]}`},
		{"nested arrays", `{"d":[[{"type":"x"},{"type":"y"},{"type":"x"}]]}`, `"x"`, "", `{"d":[[{"type":"y"}]]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := `{"mode":"prune_objects","value":` + tc.value + `,"path":"` + tc.path + `"}`
			out, err := compileOverride(t, `{"operations":[`+op+`]}`).Apply([]byte(tc.body), &Context{})
			requireNoError(t, err)
			assertJSONEq(t, tc.want, string(out.Body))
		})
	}
}

// Member keys with gjson path syntax are literal wherever the engine
// builds a path from a key: legacy keys, wildcard expansion, merges.
func TestParamOverrideSpecialKeys(t *testing.T) {
	out, err := compileOverride(t, `{"k#":1,"@m":2,"a|b":3,"operations":[{"mode":"append","path":"o","value":{"x#":1,"@y":2}}]}`).
		Apply([]byte(`{"o":{}}`), &Context{})
	requireNoError(t, err)
	assertJSONEq(t, `{"o":{"x#":1,"@y":2},"k#":1,"@m":2,"a|b":3}`, string(out.Body))

	out, err = compileOverride(t, `{"operations":[{"mode":"delete","path":"*"}]}`).Apply([]byte(`{"k#":1,"@m":2,"a|b":3,"keep":4}`), &Context{})
	requireNoError(t, err)
	assertJSONEq(t, `{}`, string(out.Body))
}

// The conditions object of an operation keeps its keys as written (blank
// ones too, as before prune_objects shared it); only where trims.
func TestParamOverrideConditionsObjectKeys(t *testing.T) {
	out, err := compileOverride(t, `{"operations":[{"mode":"set","path":"hit","value":1,"conditions":{" k":1}},
		{"mode":"set","path":"blank","value":1,"conditions":{"":1}}]}`).Apply([]byte(`{" k":1,"k":2}`), &Context{})
	requireNoError(t, err)
	assertJSONEq(t, `{" k":1,"k":2,"hit":1}`, string(out.Body))
}

// A header value that cannot be sent never reaches the upstream request:
// literal values fail when saved, values from the body or the client fail
// the run (the gateway fails over neutrally).
func TestParamOverrideHeaderControlCharacters(t *testing.T) {
	_, errs := Compile("param_override", []byte(`{"operations":[
		{"mode":"set_header","path":"x-a","value":"a\nb"},
		{"mode":"set_header","path":"x-b","value":"a\u0001b"},
		{"mode":"set_header","path":"x-c","value":{"$append":["ok\u007f"]}},
		{"mode":"set_header","path":"x-d","value":"tab\tok"}]}`), false)
	requireLen(t, errs, 3, "%v", errs)
	for i, loc := range []string{"operations[0].value", "operations[1].value", "operations[2].value.$append"} {
		assertEqual(t, loc+": must not contain control characters", errs[i].Message)
	}

	client := &Context{Headers: http.Header{"X-Bad": {"a\x01b"}}}
	for _, tc := range []struct{ override, body string }{
		{`{"operations":[{"mode":"sync_fields","from":"json:u","to":"header:x-u"}]}`, `{"u":"a\nb"}`},
		{`{"operations":[{"mode":"sync_fields","from":"json:m","to":"header:x-m"}]}`, "{\"m\":{\n\"a\":1}}"},
		{`{"operations":[{"mode":"copy_header","from":"x-bad","to":"x-c"}]}`, `{}`},
		{`{"operations":[{"mode":"pass_headers","value":["x-bad"]}]}`, `{}`},
		{`{"operations":[{"mode":"set_header","path":"x-bad","value":{"$append":["t"]}}]}`, `{}`},
	} {
		_, err := compileOverride(t, tc.override).Apply([]byte(tc.body), client)
		assertErrorContains(t, err, "contains a control character")
	}
}

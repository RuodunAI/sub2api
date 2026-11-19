package apikey

import (
	"reflect"
	"strings"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/fieldcheck"
)

func TestValidator(t *testing.T) {
	v := Validator{Prefixes: []string{"sk-test-", "sk-live-"}, MinLength: 15, Field: "api_key"}
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"  sk-test-abcdef1234567890  ", "sk-test-abcdef1234567890", false},
		{"sk-live-xyz123456789012", "sk-live-xyz123456789012", false},
		{"", "", true},
		{"   ", "", true},
		{"sk-prod-short", "", true},           // wrong prefix
		{"sk-test-abc", "", true},             // too short
		{"sk-test-abc def", "", true},         // whitespace
		{"sk-test-abc\tdef", "", true},        // tab
		{"sk-test-abc\ndef1234567", "", true}, // newline
	} {
		got, err := v.Check(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("Check(%q): err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("Check(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if err != nil && err.Field != "api_key" {
			t.Errorf("error field = %q, want api_key", err.Field)
		}
	}
}

func TestBuiltinValidators(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    Validator
		good []string
		bad  []string
	}{
		{"OpenAI", OpenAI, []string{"sk-proj-abcdef123456789012", "sess-xyzabcdefgh123456"}, []string{"sk-", "apikey-123"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range tc.good {
				if _, err := tc.v.Check(s); err != nil {
					t.Errorf("rejected valid key %q: %v", s, err)
				}
			}
			for _, s := range tc.bad {
				if _, err := tc.v.Check(s); err == nil {
					t.Errorf("accepted invalid key %q", s)
				}
			}
		})
	}
}

// Normalization stays in the object each field came from: base_url in
// settings remains a setting (the core's guardedSettings check and the
// console form read it there), and keys the validator does not know pass
// through. The stage 1 refactor once merged everything into the
// credentials and returned "{}" settings.
func TestValidateNormalizesInPlace(t *testing.T) {
	s := Spec{AccountType: "apikey", DefaultBaseURL: "https://api.example.com", StripSuffixes: []string{"/v1"}}
	r := s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey",
		CredentialsJson: `{"api_key":"  sk-example-123  ","note":"keep"}`,
		SettingsJson:    `{"base_url":"https://proxy.example.com/v1/","region":"eu"}`})
	if len(r.GetErrors()) != 0 {
		t.Fatal(r.GetErrors())
	}
	if r.GetNormalizedCredentialsJson() != `{"api_key":"sk-example-123","note":"keep"}` ||
		r.GetNormalizedSettingsJson() != `{"base_url":"https://proxy.example.com","region":"eu"}` {
		t.Fatalf("creds=%s settings=%s", r.GetNormalizedCredentialsJson(), r.GetNormalizedSettingsJson())
	}
	// Nothing in settings: "" keeps whatever the core has.
	r = s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: `{"api_key":"sk-example-123"}`})
	if r.GetNormalizedSettingsJson() != "" || r.GetNormalizedCredentialsJson() != `{"api_key":"sk-example-123"}` {
		t.Fatalf("creds=%s settings=%q", r.GetNormalizedCredentialsJson(), r.GetNormalizedSettingsJson())
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	strip := []string{"/v1beta", "/v1"}
	for in, want := range map[string]string{
		" https://proxy.example.com/v1beta/ ":   "https://proxy.example.com",
		"https://proxy.example.com/x/v1/v1beta": "https://proxy.example.com/x/v1",
		"https://proxy.example.com/v1":          "https://proxy.example.com",
		"https://proxy.example.com":             "https://proxy.example.com",
	} {
		if got, err := NormalizeBaseURL(in, strip); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ftp://x.example", "https://u:p@x.example", "https://x.example/?", "https://x.example/?a=1", "https://x.example/#f", "/relative",
		"https://x.example/#", "https://x.example/a b", "https://x.example/a\tb", "https://x.example/a\nb", "https://x.example/a\u00a0b", "https://x.example/a\u3000b", "https://x .example"} {
		if _, err := NormalizeBaseURL(in, strip); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// TestNormalizeBaseURLIsStable: requests normalize the stored base_url
// again (Spec.FromAccount). A stored value is an output of NormalizeBaseURL,
// u.String() with RawPath cleared: spaces, every non-ASCII byte, "?" and "#"
// in it are percent-escaped, so it holds no raw whitespace, "?" or "#" and
// comes out unchanged and without an error. (The former form schema is no
// reason: RE2's \s is ASCII only, a no-break space passed it.)
func TestNormalizeBaseURLIsStable(t *testing.T) {
	strip := []string{"/v1beta", "/v1"}
	for _, in := range []string{
		" https://proxy.example.com/v1beta/ ", "HTTPS://Proxy.Example.com/x/v1", "https://proxy.example.com:8443/a/b/",
		"http://127.0.0.1:8080", "https://x.example.com/a%20b", "https://x.example.com/a%2Fb/c", "https://[::1]:8080/p",
		"https://x.example.com/ключ", "https://x.example.com/~user/v1/",
		"https://x.example.com/a%C2%A0b", "https://x%C2%A0y.example", "https://x.example.com/a%E3%80%80b",
		"https://x.example.com/a%C2%85b", "https://x.example.com/a%3Fb", "https://x.example.com/a%23b",
	} {
		stored, err := NormalizeBaseURL(in, strip)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if again, err := NormalizeBaseURL(stored, strip); err != nil || again != stored {
			t.Errorf("%q: stored %q normalizes to %q %v", in, stored, again, err)
		}
	}
}

// TestModelListRequest: the four model-list callers (anthropic, openai,
// gemini, relay) build the same request; the path and the id location are the
// only differences, and content-type (a body header) is always dropped. The
// caller's header map is copied, so reusing it after the call is safe.
func TestModelListRequest(t *testing.T) {
	cfg := &Config{APIKey: "k", BaseURL: "https://api.example.com"}
	h := map[string]string{"x-api-key": "k", "content-type": "application/json", "anthropic-version": "2023-06-01"}
	r := ModelListRequest(cfg, ModelList{Path: "/v1/models?limit=1000", IdsPath: "data.#.id"}, h)
	if r.GetMethod() != "GET" || r.GetUrl() != "https://api.example.com/v1/models?limit=1000" ||
		r.GetIdsPath() != "data.#.id" || r.GetStripPrefix() != "" {
		t.Fatalf("request = %v", r)
	}
	if _, ok := r.GetHeaders()["content-type"]; ok {
		t.Fatal("content-type must be dropped from a GET")
	}
	if r.GetHeaders()["x-api-key"] != "k" || r.GetHeaders()["anthropic-version"] != "2023-06-01" {
		t.Fatalf("headers = %v", r.GetHeaders())
	}
	if len(h) != 3 || h["content-type"] != "application/json" {
		t.Fatalf("the caller's header map was modified: %v", h)
	}
	// A differently-cased content type is dropped too.
	r = ModelListRequest(cfg, ModelList{Path: "/v1beta/models", IdsPath: "models.#.name", StripPrefix: "models/"},
		map[string]string{"Content-Type": "application/json", "authorization": "Bearer k"})
	if got := r.GetStripPrefix(); got != "models/" {
		t.Fatalf("strip prefix = %q", got)
	}
	if len(r.GetHeaders()) != 1 || r.GetHeaders()["authorization"] != "Bearer k" {
		t.Fatalf("headers = %v", r.GetHeaders())
	}
}

// TestTextsKeepShippedWording: the plugin-specific messages a plugin shipped
// before Spec.Validate existed still come out verbatim, so the console keeps
// showing what it always did.
func TestTextsKeepShippedWording(t *testing.T) {
	unset := Spec{AccountType: "apikey", DefaultBaseURL: "x"}
	if r := unset.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "other"}); r.GetErrors()[0].GetMessage() != "unsupported account type / 不支持的账号类型" {
		t.Fatalf("default message = %q", r.GetErrors()[0].GetMessage())
	}

	s := Spec{AccountType: "relay_key", RequireBaseURL: true, Texts: Texts{
		UnsupportedAccountType: "unsupported account type {type} / 不支持的账号类型",
		KeyNotString:           "API key must be a string / api_key 必须是字符串",
		KeyNotStringCode:       "pattern",
		BaseURLRequired:        "Base URL of the relay is required / 请填写中转站的 Base URL",
	}}
	r := s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey"})
	if r.GetErrors()[0].GetMessage() != `unsupported account type "apikey" / 不支持的账号类型` {
		t.Fatalf("account type message = %q", r.GetErrors()[0].GetMessage())
	}
	if r.GetErrors()[0].GetCode() != "unsupported" || r.GetErrors()[0].GetField() != "account_type" {
		t.Fatalf("account type error = %v", r.GetErrors()[0])
	}

	r = s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "relay_key", CredentialsJson: `{"api_key":42}`})
	if e := r.GetErrors()[0]; e.GetCode() != "pattern" || e.GetMessage() != "API key must be a string / api_key 必须是字符串" {
		t.Fatalf("non-string key = %v", e)
	}

	// api_key is checked before base_url, so an empty form lists them in that
	// order (relay used to list base_url first).
	r = s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "relay_key", CredentialsJson: `{}`})
	if len(r.GetErrors()) != 2 || r.GetErrors()[0].GetField() != "api_key" || r.GetErrors()[1].GetField() != "base_url" {
		t.Fatalf("empty form errors = %v", r.GetErrors())
	}
	if got := r.GetErrors()[1].GetMessage(); got != "Base URL of the relay is required / 请填写中转站的 Base URL" {
		t.Fatalf("required message = %q", got)
	}
}

// TestTextsAreNotFormatStrings: a Texts message is plain text, so a bare
// percent sign ("100%") reaches the console unchanged and an unknown
// placeholder stays as written. Before, the message went through Sprintf and
// "100%" came out as "%!d(MISSING)" or swallowed the argument.
func TestTextsAreNotFormatStrings(t *testing.T) {
	s := Spec{AccountType: "apikey", DefaultBaseURL: "https://api.example.com", Texts: Texts{
		UnsupportedAccountType: "unsupported account type {type} (100% sure) / 不支持的账号类型",
		KeyNotString:           "key must be text, 100% / key 必须是字符串",
		KeyNotStringCode:       "type",
		BaseURLFormat:          "base_url {reason}: must be 100% absolute / base_url 格式错误",
	}}
	r := s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "other"})
	if got := r.GetErrors()[0].GetMessage(); got != `unsupported account type "other" (100% sure) / 不支持的账号类型` {
		t.Fatalf("account type message = %q", got)
	}
	r = s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: `{"api_key":42}`})
	if got := r.GetErrors()[0].GetMessage(); got != "key must be text, 100% / key 必须是字符串" {
		t.Fatalf("key message = %q", got)
	}
	r = s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: `{"api_key":42}`})
	if e := r.GetErrors()[0]; e.GetCode() != "type" || e.GetMessage() != "key must be text, 100% / key 必须是字符串" {
		t.Fatalf("non-string key = %v", e)
	}
	// The code is configured separately from the message: leaving it empty
	// keeps the historical "pattern".
	s.Texts.KeyNotStringCode = ""
	r = s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: `{"api_key":42}`})
	if e := r.GetErrors()[0]; e.GetCode() != "pattern" || e.GetMessage() != "key must be text, 100% / key 必须是字符串" {
		t.Fatalf("non-string key = %v", e)
	}
	r = s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: `{"api_key":"sk-12345678"}`, SettingsJson: `{"base_url":"ftp://x"}`})
	if e := r.GetErrors()[0]; e.GetField() != "base_url" || e.GetCode() != "format" ||
		e.GetMessage() != "base_url must be an absolute http(s) URL: must be 100% absolute / base_url 格式错误" {
		t.Fatalf("format error = %v", e)
	}
}

// Fields, when set, checks the keys it names and refuses the ones the form
// does not send (in either object); nil keeps letting them through. A rule
// on api_key or base_url runs only where the Spec's own check passed.
func TestSpecFields(t *testing.T) {
	req := &pluginv1.ValidateCredentialsRequest{AccountType: "apikey",
		CredentialsJson: `{"api_key":"sk-12345678","organization":"o","extra":1}`, SettingsJson: `{"base_url":"https://u.example","proxy":"p"}`}
	if resp := (Spec{AccountType: "apikey"}).Validate(req); len(resp.GetErrors()) != 0 {
		t.Fatalf("nil Fields = %v", resp.GetErrors())
	}
	errs := func(s Spec, creds, settings string) map[string]string {
		resp := s.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: creds, SettingsJson: settings})
		got := map[string]string{}
		for _, e := range resp.GetErrors() {
			got[e.GetField()] = e.GetCode()
		}
		return got
	}
	org := Spec{AccountType: "apikey", Fields: fieldcheck.Fields{"organization": fieldcheck.String().MaxLen(3)}}
	if got := errs(org, req.CredentialsJson, req.SettingsJson); !reflect.DeepEqual(got, map[string]string{"extra": "unknown", "proxy": "unknown"}) {
		t.Fatalf("unknown keys = %v", got)
	}
	if got := errs(org, `{"api_key":"sk-12345678","organization":"long"}`, ""); !reflect.DeepEqual(got, map[string]string{"organization": "maxLength"}) {
		t.Fatalf("rule = %v", got)
	}
	if got := errs(org, `{"api_key":"sk-12345678"}`, `{"organization":7}`); !reflect.DeepEqual(got, map[string]string{"organization": "type"}) {
		t.Fatalf("rule on a setting = %v", got)
	}
	empty := Spec{AccountType: "apikey", Fields: fieldcheck.Fields{}}
	if got := errs(empty, `{"api_key":"sk-12345678","organization":"o"}`, ""); !reflect.DeepEqual(got, map[string]string{"organization": "unknown"}) {
		t.Fatalf("an empty map allows api_key and base_url only: %v", got)
	}
	resp := empty.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey",
		CredentialsJson: `{"api_key":"sk-12345678"}`, SettingsJson: `{"base_url":"https://u.example/"}`})
	if len(resp.GetErrors()) != 0 || resp.GetNormalizedSettingsJson() != `{"base_url":"https://u.example"}` {
		t.Fatalf("valid = %v", resp)
	}
	endpoint := Spec{AccountType: "apikey", Fields: fieldcheck.Fields{"base_url": fieldcheck.String().Pattern(`^https://[a-z.]+$`)}}
	if got := errs(endpoint, `{"api_key":"sk-12345678","base_url":"https://u.example/p"}`, ""); !reflect.DeepEqual(got, map[string]string{"base_url": "pattern"}) {
		t.Fatalf("base_url rule = %v", got)
	}
	// The rules see what Validate saves: the key trimmed, the base URL
	// normalized, the one Spec read (settings before credentials) in both
	// objects.
	key := Spec{AccountType: "apikey", Fields: fieldcheck.Fields{"api_key": fieldcheck.String().MaxLen(512).Pattern(`^[!-~]*$`)}}
	if got := errs(key, `{"api_key":" sk-`+strings.Repeat("k", 509)+` "}`, ""); len(got) != 0 {
		t.Fatalf("a 512-character key with spaces around = %v", got)
	}
	root := Spec{AccountType: "apikey", StripSuffixes: []string{"/v1"}, Fields: fieldcheck.Fields{"base_url": fieldcheck.String().Pattern(`^https://good\.example$`)}}
	if got := errs(root, `{"api_key":"sk-12345678","base_url":" HTTPS://good.example/v1/ "}`, ""); len(got) != 0 {
		t.Fatalf("a base URL normalized onto the rule = %v", got)
	}
	if got := errs(root, `{"api_key":"sk-12345678","base_url":"https://bad.example"}`, `{"base_url":"https://good.example"}`); len(got) != 0 {
		t.Fatalf("the rule checks the base URL Spec uses (settings first) = %v", got)
	}
	if got := errs(root, `{"api_key":"sk-12345678","base_url":"https://good.example"}`, `{"base_url":"https://bad.example"}`); !reflect.DeepEqual(got, map[string]string{"base_url": "pattern"}) {
		t.Fatalf("the settings' base URL is the one saved = %v", got)
	}
	// One error per field: the Spec's own, not the rule's too.
	if resp := endpoint.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: `{"api_key":"sk-12345678","base_url":"ftp://u"}`}); len(resp.GetErrors()) != 1 || resp.GetErrors()[0].GetCode() != "format" {
		t.Fatalf("refused base_url = %v", resp.GetErrors())
	}
}

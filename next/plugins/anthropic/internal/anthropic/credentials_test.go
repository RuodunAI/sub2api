package anthropic

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// TestCredentialsRefused: every constraint of the account form's former
// JSON Schema (forms/apikey.schema.json: api_key required, a string of 8-512
// characters of [!-~]; base_url a string matching ^(https?://[^\s?#]+)?$), one input it refused each, is still refused by
// ValidateCredentials, at the same field. The core no longer checks a
// schema; the plugin's check is final.
func TestCredentialsRefused(t *testing.T) {
	long, longest := "sk-"+strings.Repeat("k", 510), "sk-"+strings.Repeat("k", 509) // 513 and 512 characters
	for _, c := range []struct{ name, accountType, creds, field, code string }{
		{"api_key missing", "apikey", `{}`, "api_key", ""},
		{"api_key empty", "apikey", `{"api_key": ""}`, "api_key", ""},
		{"api_key not a string", "apikey", `{"api_key": 123456789}`, "api_key", ""},
		{"api_key shorter than 8", "apikey", `{"api_key": "sk-kkkk"}`, "api_key", ""},
		{"api_key longer than 512", "apikey", `{"api_key": "` + long + `"}`, "api_key", ""},
		{"api_key with a space", "apikey", `{"api_key": "sk-abc def"}`, "api_key", ""},
		{"api_key not ASCII", "apikey", `{"api_key": "sk-ключ-abcdef"}`, "api_key", ""},
		{"base_url not a string", "apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": 1}`, "base_url", ""},
		{"base_url not http", "apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": "ftp://x.example.com"}`, "base_url", ""},
		{"base_url without a scheme", "apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": "x.example.com"}`, "base_url", ""},
		{"base_url without a host", "apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": "https://"}`, "base_url", ""},
		{"base_url with a query", "apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": "https://x.example.com/?a=1"}`, "base_url", ""},
		{"base_url with a fragment", "apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": "https://x.example.com/#a"}`, "base_url", ""},
	} {
		errs := validate(t, c.accountType, c.creds).GetErrors()
		if len(errs) == 0 || errs[0].GetField() != c.field || (c.code != "" && errs[0].GetCode() != c.code) {
			t.Errorf("%s %s: errors = %v, want one at %s %s", c.accountType, c.name, errs, c.field, c.code)
		}
	}
	// The values at the schema's limits are accepted.
	for _, c := range []struct{ name, accountType, creds string }{
		{"api_key of 512 characters", "apikey", `{"api_key": "` + longest + `"}`},
		{"api_key of 8 characters", "apikey", `{"api_key": "sk-kkkkk"}`},
	} {
		if errs := validate(t, c.accountType, c.creds).GetErrors(); len(errs) != 0 {
			t.Errorf("%s %s: errors = %v, want none", c.accountType, c.name, errs)
		}
	}
	// What the schema refused for surrounding spaces, an upper-case scheme
	// or a space in the path is saved normalized instead, into a value it
	// accepted: the key and the URL trimmed, the scheme lower-cased, the
	// space escaped.
	for _, c := range []struct{ accountType, creds, want string }{
		{"apikey", `{"api_key": " sk-ant-api03-abcdef ", "base_url": "https://x.example.com"}`, `{"api_key":"sk-ant-api03-abcdef","base_url":"https://x.example.com"}`},
		{"apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": " https://x.example.com "}`, `{"api_key":"sk-ant-api03-abcdef","base_url":"https://x.example.com"}`},
		{"apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": "HTTPS://x.example.com"}`, `{"api_key":"sk-ant-api03-abcdef","base_url":"https://x.example.com"}`},
		{"apikey", `{"api_key": "sk-ant-api03-abcdef", "base_url": "https://x.example.com/a b"}`, `{"api_key":"sk-ant-api03-abcdef","base_url":"https://x.example.com/a%20b"}`},
	} {
		if resp := validate(t, c.accountType, c.creds); len(resp.GetErrors()) != 0 || resp.GetNormalizedCredentialsJson() != c.want {
			t.Errorf("%s %s: %v %s, want %s", c.accountType, c.creds, resp.GetErrors(), resp.GetNormalizedCredentialsJson(), c.want)
		}
	}
}

func validate(t *testing.T, accountType, creds string) *pluginv1.ValidateCredentialsResponse {
	t.Helper()
	resp, err := New().ValidateCredentials(context.Background(), &pluginv1.ValidateCredentialsRequest{AccountType: accountType, CredentialsJson: creds})
	if err != nil {
		t.Fatalf("%s: %v", creds, err)
	}
	return resp
}

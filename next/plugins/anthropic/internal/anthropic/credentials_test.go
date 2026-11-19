package anthropic

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// TestCredentialsRefused: every constraint of the account form's former
// JSON Schema (api_key required, a string of 8-512 characters of [!-~];
// base_url a string matching ^(https?://[^\s?#]+)?$), one input it refused
// each, is still refused by ValidateCredentials, at the same field. The
// core no longer checks a schema; the plugin's check is final.
func TestCredentialsRefused(t *testing.T) {
	const key = `"api_key": "sk-ant-api03-abcdef"`
	for _, c := range []struct{ name, creds, field string }{
		{"api_key missing", `{}`, "api_key"},
		{"api_key empty", `{"api_key": ""}`, "api_key"},
		{"api_key not a string", `{"api_key": 123456789}`, "api_key"},
		{"api_key shorter than 8", `{"api_key": "` + strings.Repeat("k", 7) + `"}`, "api_key"},
		{"api_key longer than 512", `{"api_key": "` + strings.Repeat("k", 513) + `"}`, "api_key"},
		{"api_key with a space", `{"api_key": "sk-ant api03"}`, "api_key"},
		{"api_key not ASCII", `{"api_key": "sk-ключ-abcdef"}`, "api_key"},
		{"base_url not a string", `{` + key + `, "base_url": 1}`, "base_url"},
		{"base_url not http", `{` + key + `, "base_url": "ftp://x.example.com"}`, "base_url"},
		{"base_url without a scheme", `{` + key + `, "base_url": "x.example.com"}`, "base_url"},
		{"base_url without a host", `{` + key + `, "base_url": "https://"}`, "base_url"},
		{"base_url with a query", `{` + key + `, "base_url": "https://x.example.com/?a=1"}`, "base_url"},
		{"base_url with a fragment", `{` + key + `, "base_url": "https://x.example.com/#a"}`, "base_url"},
	} {
		if errs := validate(t, c.creds).GetErrors(); len(errs) == 0 || errs[0].GetField() != c.field {
			t.Errorf("%s: errors = %v, want one at %s", c.name, errs, c.field)
		}
	}
	// What the schema refused for spaces or an upper-case scheme is saved
	// normalized instead, into a value the schema accepts: the key trimmed,
	// the URL trimmed, its scheme lower-cased and a space in its path escaped.
	for creds, want := range map[string]string{
		`{"api_key": " sk-ant-api03-abcdef "}`:                   `{"api_key":"sk-ant-api03-abcdef"}`,
		`{` + key + `, "base_url": " https://x.example.com "}`:   `{"api_key":"sk-ant-api03-abcdef","base_url":"https://x.example.com"}`,
		`{` + key + `, "base_url": "HTTPS://x.example.com"}`:     `{"api_key":"sk-ant-api03-abcdef","base_url":"https://x.example.com"}`,
		`{` + key + `, "base_url": "https://x.example.com/a b"}`: `{"api_key":"sk-ant-api03-abcdef","base_url":"https://x.example.com/a%20b"}`,
	} {
		if resp := validate(t, creds); len(resp.GetErrors()) != 0 || resp.GetNormalizedCredentialsJson() != want {
			t.Errorf("%s: %v %s, want %s", creds, resp.GetErrors(), resp.GetNormalizedCredentialsJson(), want)
		}
	}
}

func validate(t *testing.T, creds string) *pluginv1.ValidateCredentialsResponse {
	t.Helper()
	resp, err := New().ValidateCredentials(context.Background(), &pluginv1.ValidateCredentialsRequest{AccountType: "apikey", CredentialsJson: creds})
	if err != nil {
		t.Fatalf("%s: %v", creds, err)
	}
	return resp
}

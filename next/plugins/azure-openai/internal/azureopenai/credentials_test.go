package azureopenai

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// TestCredentialsRefused: every constraint of the account form's former
// JSON Schema (forms/azure_v1.schema.json, forms/azure_deployment.schema.json:
// base_url and api_key required; api_key a string of 8-512 characters of
// [!-~]; base_url a string matching EndpointPattern; azure_deployment
// also api_version, required, matching ^\d{4}-\d{2}-\d{2}(-preview)?$), one input it refused each, is still refused by
// ValidateCredentials, at the same field. The core no longer checks a
// schema; the plugin's check is final.
// The endpoint is matched as entered, so surrounding spaces and another
// casing of the scheme are refused, as the form refuses them.
func TestCredentialsRefused(t *testing.T) {
	long, longest := "sk-"+strings.Repeat("k", 510), "sk-"+strings.Repeat("k", 509) // 513 and 512 characters
	for _, c := range []struct{ name, accountType, creds, field, code string }{
		{"api_key missing", "azure_v1", `{"base_url": "https://res.openai.azure.com"}`, "api_key", ""},
		{"api_key empty", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": ""}`, "api_key", ""},
		{"api_key not a string", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": 123456789}`, "api_key", ""},
		{"api_key shorter than 8", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": "sk-kkkk"}`, "api_key", ""},
		{"api_key longer than 512", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": "` + long + `"}`, "api_key", ""},
		{"api_key with a space", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": "sk-abc def"}`, "api_key", ""},
		{"api_key not ASCII", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": "sk-ключ-abcdef"}`, "api_key", ""},
		{"base_url missing", "azure_v1", `{"api_key": "0123456789abcdef"}`, "base_url", ""},
		{"base_url empty", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": ""}`, "base_url", ""},
		{"base_url not a string", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": 1}`, "base_url", ""},
		{"base_url not http", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "ftp://x.example.com"}`, "base_url", ""},
		{"base_url without a scheme", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "x.example.com"}`, "base_url", ""},
		{"base_url without a host", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "https://"}`, "base_url", ""},
		{"base_url with a query", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "https://x.example.com/?a=1"}`, "base_url", ""},
		{"base_url with a fragment", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "https://x.example.com/#a"}`, "base_url", ""},
		{"base_url with a deployment", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com/openai/deployments/gpt-4o"}`, "base_url", ""},
		{"base_url with an operation", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com/openai/v1/chat/completions"}`, "base_url", ""},
		{"base_url with a space", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com/a b"}`, "base_url", ""},
		{"base_url with an upper-case scheme", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": "HTTPS://res.openai.azure.com"}`, "base_url", ""},
		{"base_url with a leading space", "azure_v1", `{"api_key": "0123456789abcdef", "base_url": " https://res.openai.azure.com"}`, "base_url", ""},
		{"api_key missing", "azure_deployment", `{"api_version": "2024-10-21", "base_url": "https://res.openai.azure.com"}`, "api_key", ""},
		{"api_key empty", "azure_deployment", `{"api_version": "2024-10-21", "base_url": "https://res.openai.azure.com", "api_key": ""}`, "api_key", ""},
		{"api_key not a string", "azure_deployment", `{"api_version": "2024-10-21", "base_url": "https://res.openai.azure.com", "api_key": 123456789}`, "api_key", ""},
		{"api_key shorter than 8", "azure_deployment", `{"api_version": "2024-10-21", "base_url": "https://res.openai.azure.com", "api_key": "sk-kkkk"}`, "api_key", ""},
		{"api_key longer than 512", "azure_deployment", `{"api_version": "2024-10-21", "base_url": "https://res.openai.azure.com", "api_key": "` + long + `"}`, "api_key", ""},
		{"api_key with a space", "azure_deployment", `{"api_version": "2024-10-21", "base_url": "https://res.openai.azure.com", "api_key": "sk-abc def"}`, "api_key", ""},
		{"api_key not ASCII", "azure_deployment", `{"api_version": "2024-10-21", "base_url": "https://res.openai.azure.com", "api_key": "sk-ключ-abcdef"}`, "api_key", ""},
		{"base_url missing", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef"}`, "base_url", ""},
		{"base_url empty", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": ""}`, "base_url", ""},
		{"base_url not a string", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": 1}`, "base_url", ""},
		{"base_url not http", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "ftp://x.example.com"}`, "base_url", ""},
		{"base_url without a scheme", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "x.example.com"}`, "base_url", ""},
		{"base_url without a host", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "https://"}`, "base_url", ""},
		{"base_url with a query", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "https://x.example.com/?a=1"}`, "base_url", ""},
		{"base_url with a fragment", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "https://x.example.com/#a"}`, "base_url", ""},
		{"base_url with a deployment", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com/openai/deployments/gpt-4o"}`, "base_url", ""},
		{"base_url with an operation", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com/openai/v1/chat/completions"}`, "base_url", ""},
		{"base_url with a space", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com/a b"}`, "base_url", ""},
		{"base_url with an upper-case scheme", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": "HTTPS://res.openai.azure.com"}`, "base_url", ""},
		{"base_url with a leading space", "azure_deployment", `{"api_version": "2024-10-21", "api_key": "0123456789abcdef", "base_url": " https://res.openai.azure.com"}`, "base_url", ""},
		{"api_version missing", "azure_deployment", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com"}`, "api_version", "required"},
		{"api_version empty", "azure_deployment", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com", "api_version": ""}`, "api_version", "required"},
		{"api_version not a string", "azure_deployment", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com", "api_version": 20241021}`, "api_version", ""},
		{"api_version not a date", "azure_deployment", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com", "api_version": "latest"}`, "api_version", "pattern"},
		{"api_version with another suffix", "azure_deployment", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com", "api_version": "2024-10-21-beta"}`, "api_version", ""},
	} {
		errs := validate(t, c.accountType, c.creds).GetErrors()
		if len(errs) == 0 || errs[0].GetField() != c.field || (c.code != "" && errs[0].GetCode() != c.code) {
			t.Errorf("%s %s: errors = %v, want one at %s %s", c.accountType, c.name, errs, c.field, c.code)
		}
	}
	// The values at the schema's limits are accepted.
	for _, c := range []struct{ name, accountType, creds string }{
		{"api_key of 512 characters", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": "` + longest + `"}`},
		{"api_key of 8 characters", "azure_v1", `{"base_url": "https://res.openai.azure.com", "api_key": "sk-kkkkk"}`},
		{"api_key of 512 characters", "azure_deployment", `{"base_url": "https://res.openai.azure.com", "api_version": "2024-10-21", "api_key": "` + longest + `"}`},
		{"api_key of 8 characters", "azure_deployment", `{"base_url": "https://res.openai.azure.com", "api_version": "2024-10-21", "api_key": "sk-kkkkk"}`},
		{"a preview api_version", "azure_deployment", `{"api_key": "0123456789abcdef", "base_url": "https://res.openai.azure.com", "api_version": "2025-04-01-preview"}`},
		{"a Foundry project endpoint", "azure_deployment", `{"api_key": "0123456789abcdef", "base_url": "https://res.services.ai.azure.com/api/projects/p1", "api_version": "2024-10-21"}`},
	} {
		if errs := validate(t, c.accountType, c.creds).GetErrors(); len(errs) != 0 {
			t.Errorf("%s %s: errors = %v, want none", c.accountType, c.name, errs)
		}
	}
	// Spaces around the key, which the schema refused, are trimmed
	// instead: the key is saved without them.
	for _, c := range []struct{ accountType, creds, want string }{
		{"azure_v1", `{"api_key": " 0123456789abcdef ", "base_url": "https://res.openai.azure.com"}`, `{"api_key":"0123456789abcdef","base_url":"https://res.openai.azure.com"}`},
		{"azure_deployment", `{"api_version": "2024-10-21", "api_key": " 0123456789abcdef ", "base_url": "https://res.openai.azure.com"}`, `{"api_key":"0123456789abcdef","api_version":"2024-10-21","base_url":"https://res.openai.azure.com"}`},
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
